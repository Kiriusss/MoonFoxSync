// Package objectfmt implements the canonical KiriusDrive v1 snapshot format.
package objectfmt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	ChunkSize = 4 << 20
	fanout    = 256
	// Reader budgets bound the amount of untrusted metadata expanded in one call.
	maxFileChunks = 1 << 20
	maxEntries    = 1 << 20
	maxDirDepth   = 256
)

type Ref struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
}

type Entry struct {
	Name       string
	Kind       string
	Digest     string
	Target     string
	Executable bool
	MtimeNS    int64
}

type FileInfo struct {
	Size   int64
	SHA256 string
	Chunks []Ref
}

type Load func(Ref) ([]byte, error)
type Store func(Ref, []byte) error

var ErrInvalid = errors.New("invalid snapshot object")

// ErrObjectIntegrity identifies stored bytes that disagree with their object key.
var ErrObjectIntegrity = fmt.Errorf("%w: object integrity check failed", ErrInvalid)

func invalid(detail string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, detail)
}

func Digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func validName(name string) bool {
	return len(name) >= 1 && len(name) <= 255 && utf8.ValidString(name) &&
		name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func rawDigest(s string) ([]byte, error) {
	if len(s) != 64 || strings.ToLower(s) != s {
		return nil, invalid("expected lowercase SHA-256")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, invalid("expected hex SHA-256")
	}
	return b, nil
}

func get(ref Ref, load Load) ([]byte, error) {
	if _, err := rawDigest(ref.Digest); err != nil {
		return nil, err
	}
	b, err := load(ref)
	if err != nil {
		return nil, err
	}
	if len(b) > ChunkSize || Digest(b) != ref.Digest {
		return nil, fmt.Errorf("%w: %s wrong size or SHA-256: %s", ErrObjectIntegrity, ref.Kind, ref.Digest)
	}
	return b, nil
}

func put(kind string, raw []byte, store Store) (string, error) {
	ref := Ref{Kind: kind, Digest: Digest(raw)}
	if err := store(ref, raw); err != nil {
		return "", err
	}
	return ref.Digest, nil
}

func header(tag string, n int) []byte {
	return binary.BigEndian.AppendUint32([]byte(tag), uint32(n))
}

func hashList(dst []byte, refs []string) ([]byte, error) {
	for _, ref := range refs {
		b, err := rawDigest(ref)
		if err != nil {
			return nil, err
		}
		dst = append(dst, b...)
	}
	return dst, nil
}

// buildIndex groups adjacent child pages, left to right, into canonical levels.
// The base pages have height zero.
func buildIndex(kind, tag string, pages []string, store Store) (string, byte, error) {
	var height byte
	for len(pages) > 1 {
		height++
		var next []string
		for i := 0; i < len(pages); i += fanout {
			batch := pages[i:min(i+fanout, len(pages))]
			raw := binary.BigEndian.AppendUint32(append([]byte(tag), height), uint32(len(batch)))
			raw, err := hashList(raw, batch)
			if err != nil {
				return "", 0, err
			}
			id, err := put(kind, raw, store)
			if err != nil {
				return "", 0, err
			}
			next = append(next, id)
		}
		pages = next
	}
	return pages[0], height, nil
}

type decoder struct {
	rest []byte
	err  error
}

func (d *decoder) take(n int) []byte {
	if n < 0 || n > len(d.rest) || d.err != nil {
		d.err = invalid("truncated object")
		return nil
	}
	p := d.rest[:n]
	d.rest = d.rest[n:]
	return p
}
func (d *decoder) u8() byte {
	b := d.take(1)
	if len(b) == 0 {
		return 0
	}
	return b[0]
}
func (d *decoder) u16() int {
	b := d.take(2)
	if len(b) == 0 {
		return 0
	}
	return int(binary.BigEndian.Uint16(b))
}
func (d *decoder) u32() uint32 {
	b := d.take(4)
	if len(b) == 0 {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}
func (d *decoder) u64() uint64 {
	b := d.take(8)
	if len(b) == 0 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
func (d *decoder) hash() string { return hex.EncodeToString(d.take(32)) }
func (d *decoder) done() error {
	if d.err != nil {
		return d.err
	}
	if len(d.rest) > 0 {
		return invalid("non-canonical trailing bytes")
	}
	return nil
}

// BuildFile consumes src once. Store receives immutable bytes for each new
// occurrence; it may deduplicate repeated references.
func BuildFile(src io.Reader, store Store) (string, error) {
	sum := sha256.New()
	var size uint64
	var chunks []string
	for {
		var b []byte
		var err error
		if len(chunks) == 0 {
			// The common small-file path never allocates a full block.
			b, err = io.ReadAll(io.LimitReader(src, ChunkSize))
		} else {
			b = make([]byte, ChunkSize)
			var n int
			n, err = io.ReadFull(src, b)
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				err = nil
			}
			b = b[:n]
		}
		if err != nil {
			return "", err
		}
		if len(b) == 0 {
			break
		}
		if len(chunks) >= maxFileChunks {
			return "", invalid("file chunk limit exceeded")
		}
		id, err := put("chunk", b, store)
		if err != nil {
			return "", err
		}
		chunks = append(chunks, id)
		sum.Write(b)
		size += uint64(len(b))
		if len(b) < ChunkSize {
			break
		}
	}
	return storeFileRoot(size, sum.Sum(nil), chunks, store)
}

func storeFileRoot(size uint64, checksum []byte, chunks []string, store Store) (string, error) {
	raw := binary.BigEndian.AppendUint64([]byte("KDF1"), size)
	raw = append(raw, checksum...)
	raw = binary.BigEndian.AppendUint32(raw, uint32(len(chunks)))
	if len(chunks) <= fanout {
		raw, err := hashList(raw, chunks)
		if err != nil {
			return "", err
		}
		return put("file", raw, store)
	}
	var pages []string
	for i := 0; i < len(chunks); i += fanout {
		group := chunks[i:min(i+fanout, len(chunks))]
		leaf, err := hashList(header("KDCL", len(group)), group)
		if err != nil {
			return "", err
		}
		id, err := put("file", leaf, store)
		if err != nil {
			return "", err
		}
		pages = append(pages, id)
	}
	id, height, err := buildIndex("file", "KDCI", pages, store)
	if err != nil {
		return "", err
	}
	raw, err = hashList(append(raw, height), []string{id})
	if err != nil {
		return "", err
	}
	return put("file", raw, store)
}

// ReadFile checks a file manifest and expands its hash pages without fetching data chunks.
func ReadFile(root string, load Load) (FileInfo, error) {
	var info FileInfo
	raw, err := get(Ref{"file", root}, load)
	if err != nil {
		return info, err
	}
	d := decoder{rest: raw}
	if string(d.take(4)) != "KDF1" {
		return info, invalid("expected file root")
	}
	size := d.u64()
	if size > uint64(maxFileChunks)*ChunkSize {
		return info, invalid("file exceeds chunk limit")
	}
	info.Size = int64(size)
	info.SHA256 = d.hash()
	count := int(d.u32())
	if count != int((size+ChunkSize-1)/ChunkSize) {
		return info, invalid("inconsistent file length")
	}
	if count <= fanout {
		for i := 0; i < count; i++ {
			info.Chunks = append(info.Chunks, Ref{"chunk", d.hash()})
		}
		return info, d.done()
	}
	height, id := d.u8(), d.hash()
	if err := d.done(); err != nil {
		return info, err
	}
	capacity, expectedHeight := fanout, byte(0)
	for capacity < count {
		capacity *= fanout
		expectedHeight++
	}
	if height != expectedHeight {
		return info, invalid("incorrect file page height")
	}
	err = readFilePage(id, height, count, capacity, load, &info)
	return info, err
}

func readFilePage(id string, height byte, count, capacity int, load Load, info *FileInfo) error {
	raw, err := get(Ref{"file", id}, load)
	if err != nil {
		return err
	}
	d := decoder{rest: raw}
	tag := string(d.take(4))
	if height == 0 {
		if tag != "KDCL" || int(d.u32()) != count {
			return invalid("invalid file leaf page")
		}
		for i := 0; i < count; i++ {
			info.Chunks = append(info.Chunks, Ref{"chunk", d.hash()})
		}
		return d.done()
	}
	childCap := capacity / fanout
	n := (count + childCap - 1) / childCap
	if tag != "KDCI" || d.u8() != height || int(d.u32()) != n {
		return invalid("invalid file index page")
	}
	if len(d.rest) != 32*n {
		return invalid("invalid file page size")
	}
	for i := 0; i < n; i++ {
		size := min(count, childCap)
		if err := readFilePage(d.hash(), height-1, size, childCap, load, info); err != nil {
			return err
		}
		count -= size
	}
	return d.done()
}

// WriteRange checks each chunk before writing the requested byte range.
func WriteRange(info FileInfo, off, length int64, w io.Writer, load Load) error {
	if off < 0 || length < 0 || off > info.Size || length > info.Size-off {
		return invalid("range outside file")
	}
	for length > 0 {
		idx := off / ChunkSize
		if idx >= int64(len(info.Chunks)) {
			return invalid("missing chunk reference")
		}
		ref := info.Chunks[idx]
		b, err := get(ref, load)
		if err != nil {
			return err
		}
		if int64(len(b)) != min(int64(ChunkSize), info.Size-idx*ChunkSize) {
			return invalid("wrong chunk length")
		}
		start := off % ChunkSize
		n := min(int64(len(b))-start, length)
		written, err := w.Write(b[start : start+n])
		if err != nil {
			return err
		}
		if int64(written) != n {
			return io.ErrShortWrite
		}
		off += n
		length -= n
	}
	return nil
}

// VerifyFile checks a complete file including its whole-file digest.
func VerifyFile(id string, load Load) error {
	info, err := ReadFile(id, load)
	if err != nil {
		return err
	}
	if len(info.Chunks) == 1 {
		chunk := info.Chunks[0]
		data, err := get(chunk, load)
		if err != nil {
			return err
		}
		if int64(len(data)) != info.Size {
			return invalid("wrong chunk length")
		}
		// get has already hashed these exact bytes. A one-chunk file has the same digest.
		if chunk.Digest != info.SHA256 {
			return invalid("whole-file checksum mismatch")
		}
		return nil
	}
	h := sha256.New()
	if err = WriteRange(info, 0, info.Size, h, load); err != nil {
		return err
	}
	whole, _ := rawDigest(info.SHA256)
	if !bytes.Equal(h.Sum(nil), whole) {
		return invalid("whole-file checksum mismatch")
	}
	return nil
}
