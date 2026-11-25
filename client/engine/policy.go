package engine

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

const smallLimit int64 = 10 << 20
const dailyLimit int64 = 100 << 20

var ErrWaiting = errors.New("等待家庭网络或手动放行")

type AutoPolicy struct {
	HomeURL   string
	Interface string
	AwaySmall bool
}
type checkedTransport struct {
	base  http.RoundTripper
	check func(context.Context) error
}

func (t checkedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.check(req.Context()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func physicalRoute(ctx context.Context, name string, ip net.IP) (*net.Interface, net.IP, error) {
	iface, e := net.InterfaceByName(name)
	if e != nil {
		return nil, nil, ErrWaiting
	}
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
		return nil, nil, ErrWaiting
	}
	addresses, e := iface.Addrs()
	if e != nil {
		return nil, nil, e
	}
	var local net.IP
	for _, addr := range addresses {
		own, subnet, err := net.ParseCIDR(addr.String())
		if err == nil && own.To4() != nil && subnet.Contains(ip) {
			local = own.To4()
			break
		}
	}
	if local == nil {
		return nil, nil, ErrWaiting
	}
	switch runtime.GOOS {
	case "darwin":
		if !strings.HasPrefix(iface.Name, "en") {
			return nil, nil, ErrWaiting
		}
		raw, e := exec.CommandContext(ctx, "/sbin/route", "-n", "get", ip.String()).Output()
		if e != nil {
			return nil, nil, ErrWaiting
		}
		matched := false
		for _, line := range strings.Split(string(raw), "\n") {
			kv := strings.SplitN(strings.TrimSpace(line), ":", 2)
			if len(kv) == 2 {
				if kv[0] == "interface" {
					matched = strings.TrimSpace(kv[1]) == name
				}
				if kv[0] == "flags" && strings.Contains(kv[1], "GATEWAY") {
					return nil, nil, ErrWaiting
				}
			}
		}
		if !matched {
			return nil, nil, ErrWaiting
		}
	case "linux":
		if _, e := os.Stat(filepath.Join("/sys/class/net", name, "device")); e != nil {
			return nil, nil, ErrWaiting
		}
		raw, e := exec.CommandContext(ctx, "ip", "-4", "route", "get", ip.String()).Output()
		if e != nil {
			return nil, nil, ErrWaiting
		}
		fields := strings.Fields(string(raw))
		matched := false
		for i, f := range fields {
			if f == "via" {
				return nil, nil, ErrWaiting
			}
			if f == "dev" && i+1 < len(fields) && fields[i+1] == name {
				matched = true
			}
		}
		if !matched {
			return nil, nil, ErrWaiting
		}
	case "windows":
		// Both substitutions are parsed numeric values, never user-supplied shell strings.
		script := fmt.Sprintf("$ErrorActionPreference='Stop'; $r=Find-NetRoute -RemoteIPAddress '%s' | Where-Object { $_.CimClass.CimClassName -eq 'MSFT_NetRoute' }; $a=Get-NetAdapter -InterfaceIndex %d; if ($r.InterfaceIndex -eq %d -and $r.NextHop -eq '0.0.0.0' -and $a.HardwareInterface) { 'on-link' }", ip.String(), iface.Index, iface.Index)
		raw, e := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
		if e != nil || strings.TrimSpace(string(raw)) != "on-link" {
			return nil, nil, ErrWaiting
		}
	default:
		return nil, nil, ErrWaiting
	}
	return iface, local, nil
}
func (s *Store) homeRemote(ctx context.Context, p AutoPolicy) (*Remote, error) {
	u, e := url.Parse(p.HomeURL)
	if e != nil {
		return nil, e
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsPrivate() || ip.To4() == nil || p.Interface == "" {
		return nil, fmt.Errorf("家庭端点需要明确的私网 IPv4 和物理网卡名称")
	}
	iface, local, e := physicalRoute(ctx, p.Interface, ip)
	if e != nil {
		return nil, e
	}
	var ca []byte
	var dev, pin string
	e = s.db.QueryRowContext(ctx, "SELECT ca,device,pin FROM account JOIN service_identity ON account.id=service_identity.id WHERE account.id=1").Scan(&ca, &dev, &pin)
	if e != nil {
		return nil, e
	}
	tok, e := readSecret(dev)
	if e != nil {
		return nil, e
	}
	r, e := makeRemote(p.HomeURL, ca, tok)
	if e != nil {
		return nil, e
	}
	wire := r.client.Transport.(*commitTransport)
	t := wire.normal
	t.TLSClientConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("家庭端点缺少证书")
		}
		sum := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
		if hex.EncodeToString(sum[:]) != pin {
			return errors.New("家庭端点与配对服务身份不符")
		}
		return nil
	}
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: local}, Timeout: 5 * time.Second, Control: interfaceControl(iface)}
	t.DialContext = d.DialContext
	r.client.Transport = checkedTransport{wire, func(ctx context.Context) error {
		found, addr, e := physicalRoute(ctx, p.Interface, ip)
		if e != nil {
			return e
		}
		if found.Index != iface.Index || !addr.Equal(local) {
			return ErrWaiting
		}
		return nil
	}}
	return r, nil
}

// meter reserves the request length and counts bytes delivered to the application.
// Its total allowance is reserved in SQLite before any request is sent; after a
// crash the conservative reservation remains charged for the full 24h window.
type meter struct {
	next        http.RoundTripper
	limit, used int64
}
type meteredBody struct {
	io.ReadCloser
	m *meter
}

func (b meteredBody) Read(p []byte) (int, error) {
	left := b.m.limit - b.m.used
	if left == 0 {
		return 0, ErrWaiting
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, e := b.ReadCloser.Read(p)
	b.m.used += int64(n)
	return n, e
}
func (m *meter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.ContentLength < 0 || req.ContentLength > m.limit-m.used {
		return nil, ErrWaiting
	}
	m.used += req.ContentLength
	resp, e := m.next.RoundTrip(req)
	if e != nil {
		return nil, e
	}
	if resp.ContentLength > m.limit-m.used {
		resp.Body.Close()
		return nil, ErrWaiting
	}
	resp.Body = meteredBody{resp.Body, m}
	return resp, nil
}
func (s *Store) estimate(ctx context.Context, v Snapshot) (int64, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT o.kind,o.digest,o.size FROM snapshot_objects o WHERE o.snapshot=? AND NOT EXISTS
 (SELECT 1 FROM snapshot_objects x JOIN snapshots p ON p.id=x.snapshot AND p.receipt IS NOT NULL
 WHERE x.snapshot=? AND x.kind=o.kind AND x.digest=o.digest)`, v.ID, v.Parent)
	if e != nil {
		return 0, e
	}
	defer rows.Close()
	// An upper bound includes two negotiation bodies and framing per object.
	estimate := int64(8 << 10)
	for rows.Next() {
		var k, d string
		var size int64
		if e = rows.Scan(&k, &d, &size); e != nil {
			return 0, e
		}
		if k == "file" {
			f, e := os.Open(s.objectPath(objectfmt.Ref{Kind: k, Digest: d}))
			if e != nil {
				return 0, e
			}
			var hdr [12]byte
			_, e = io.ReadFull(f, hdr[:])
			f.Close()
			// Paged non-root file objects have a different header.
			if e != nil {
				return 0, e
			}
			if string(hdr[:4]) == "KDF1" && binary.BigEndian.Uint64(hdr[4:]) > uint64(smallLimit) {
				return 0, ErrWaiting
			}
		}
		if size < 1<<20 {
			estimate += (size + 2) / 3 * 4
		} else {
			estimate += size
		}
		estimate += 1024
		if estimate > smallLimit {
			return 0, ErrWaiting
		}
	}
	return estimate, rows.Err()
}
func (s *Store) AutoSync(ctx context.Context, v Snapshot, p AutoPolicy) (*Receipt, error) {
	activity, lockErr := s.localLock(false)
	if lockErr != nil {
		return nil, lockErr
	}
	defer activity.Close()
	b, e := s.binding(ctx, v.Unit)
	if e != nil {
		return nil, e
	}
	if b.Profile == "codex" {
		lock, e := s.presetLock()
		if e != nil {
			return nil, e
		}
		defer lock.Close()
		state, e := s.CodexStatus(ctx)
		if e != nil {
			return nil, e
		}
		if !state.Enabled {
			return nil, ErrWaiting
		}
	}
	cfg, e := s.Schedule(ctx, v.Unit)
	if e != nil {
		return nil, e
	}
	if cfg.Policy == "manual" {
		return nil, ErrWaiting
	}
	if cfg.Policy == "home" {
		p.AwaySmall = false
	}
	receipt, _, err := s.withUpload(ctx, v.Unit, v.ID, func(current Snapshot, request string) (*Receipt, error) {
		return s.autoTransfer(ctx, current, request, p)
	})
	if errors.Is(err, errUploadActive) {
		return nil, ErrWaiting
	}
	if b.Profile == "codex" && !errors.Is(err, ErrWaiting) {
		s.recordPresetError(ctx, err)
	}
	return receipt, err
}
func (s *Store) autoTransfer(ctx context.Context, v Snapshot, request string, p AutoPolicy) (*Receipt, error) {
	if p.HomeURL != "" {
		r, e := s.homeRemote(ctx, p)
		if e == nil {
			return s.sendSnapshot(ctx, r, v, request)
		}
		if !errors.Is(e, ErrWaiting) {
			return nil, e
		}
	}
	if !p.AwaySmall {
		return nil, ErrWaiting
	}
	estimate, e := s.estimate(ctx, v)
	if e != nil {
		return nil, e
	}
	r, e := s.Remote(ctx)
	if e != nil {
		return nil, e
	}
	now := time.Now().Unix()
	key := newID()
	result, e := s.db.ExecContext(ctx, `INSERT INTO traffic(id,snapshot,at,charged)
 SELECT ?,?,?,? WHERE (SELECT COALESCE(SUM(charged),0) FROM traffic WHERE at>?) + ? <= ?
 AND (SELECT COALESCE(SUM(charged),0) FROM traffic WHERE snapshot=?) + ? <= ?`, key, v.ID, now, estimate, now-86400, estimate, dailyLimit, v.ID, estimate, smallLimit)
	if e != nil {
		return nil, e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return nil, e
	}
	if n != 1 {
		return nil, ErrWaiting
	}
	m := &meter{next: r.client.Transport, limit: estimate}
	r.client.Transport = m
	ack, err := s.sendSnapshot(ctx, r, v, request)
	_, settleErr := s.db.ExecContext(context.Background(), "UPDATE traffic SET charged=? WHERE id=?", m.used, key)
	if err == nil {
		err = settleErr
	}
	return ack, err
}
