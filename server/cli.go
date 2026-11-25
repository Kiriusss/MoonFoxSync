package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const Version = "0.4.4"

func Run(args []string, out io.Writer) error {
	usage := "梦枢天罗 · 心狐同步 NAS 服务端\nkdrive-server init|serve|unit add|unit grant|unit allow-delete|unit disallow-delete|pair issue|device revoke --config <file>\n"
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(out, usage)
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintf(out, "梦枢天罗 %s\n", Version)
		return nil
	}
	command := args[0]
	args = args[1:]
	switch command {
	case "unit", "pair", "device":
		if len(args) == 0 {
			return errors.New(usage)
		}
		command += " " + args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("梦枢天罗 "+command, flag.ContinueOnError)
	config := fs.String("config", "", "JSON configuration file")
	name := fs.String("name", "", "unit display name")
	kind := fs.String("kind", "managed", "unit kind")
	root := fs.String("root", "", "absolute existing read-only directory")
	role := fs.String("role", "reader", "reader or sync")
	id := fs.String("id", "", "unit or device id")
	unit := fs.String("unit", "", "unit id")
	device := fs.String("device", "", "device id")
	// The documented positional id form is accepted in addition to --id.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		*id = args[0]
		args = args[1:]
	}
	if e := fs.Parse(args); e != nil {
		return e
	}
	if fs.NArg() > 0 {
		return errors.New(usage)
	}
	c, e := readConfig(*config)
	if e != nil {
		return e
	}
	if command == "init" {
		return Init(c.DataDir)
	}
	s, e := Open(c.DataDir)
	if e != nil {
		return e
	}
	defer s.Close()
	switch command {
	case "serve":
		return serve(s, c)
	case "unit add":
		var uid string
		switch *kind {
		case "managed":
			if *root != "" {
				return errors.New("--root is only for external units")
			}
			uid, e = s.AddUnit(*name)
		case "external":
			uid, e = s.AddExternal(*name, *root, c.TLS.KeyFile, *config)
		default:
			return errors.New("kind must be managed or external")
		}
		if e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(fields{"unitId": uid})
	case "pair issue":
		v, e := s.Issue(*unit, *role)
		if e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(fields{"code": v, "expiresIn": 600})
	case "unit grant":
		if *unit == "" {
			*unit = *id
		}
		return s.Grant(*unit, *device, *role)
	case "unit allow-delete", "unit disallow-delete":
		return s.setDeleteManager(*unit, *device, command == "unit allow-delete")
	case "device revoke":
		return s.Revoke(*id)
	default:
		return errors.New(usage)
	}
}
func serve(s *Store, c Config) error {
	if len(c.Listen) == 0 || c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
		return errors.New("listen addresses and TLS certFile/keyFile required")
	}
	pair, e := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
	if e != nil {
		return e
	}
	service := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}}
	var listeners []net.Listener
	for _, addr := range c.Listen {
		l, e := net.Listen("tcp", addr)
		if e != nil {
			for _, v := range listeners {
				v.Close()
			}
			return e
		}
		listeners = append(listeners, l)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gcDone := make(chan struct{})
	go func() {
		defer close(gcDone)
		s.runLibraryGC(ctx)
	}()
	defer func() { stop(); <-gcDone }()
	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		fmt.Fprintln(os.Stderr, "梦枢天罗 HTTPS", l.Addr())
		go func(l net.Listener) { errs <- service.ServeTLS(l, "", "") }(l)
	}
	select {
	case e = <-errs:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ce := service.Shutdown(shutdown)
	if e != nil && !errors.Is(e, http.ErrServerClosed) {
		return e
	}
	return ce
}
