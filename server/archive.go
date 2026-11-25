package server

import (
	"archive/zip"
	"compress/flate"
	"context"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

// Encode path syntax that common ZIP extractors interpret specially, including
// backslashes, drive/stream separators, and DOS device names. Literal percent
// signs are escaped as well, keeping ordinary names and encoded names distinct.
func archiveComponent(name string) string {
	const hex = "0123456789ABCDEF"
	last := len(strings.TrimRight(name, " ."))
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$"
	if suffix, ok := strings.CutPrefix(base, "COM"); ok || strings.HasPrefix(base, "LPT") {
		if !ok {
			suffix = base[3:]
		}
		reserved = len([]rune(suffix)) == 1 && strings.Contains("123456789¹²³", suffix)
	}
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 32 || c == 127 || strings.IndexByte("%/\\:\"<>|?*", c) >= 0 || i >= last || (i == 0 && reserved) {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Keep components usable on filesystems with a 255-byte name limit. The marker
// cannot occur in an escaped literal name.
func archiveShortName(name, original string, limit int) string {
	if len(name) <= limit {
		return name
	}
	suffix := "%~" + hash([]byte(original))[:16]
	cut := limit - len(suffix)
	for !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + suffix
}

type archiveStream struct {
	out  *zip.Writer
	load objectfmt.Load
}

func archiveFileMethod(name string) uint16 {
	switch strings.ToLower(path.Ext(name)) {
	case ".zip", ".gz", ".bz2", ".xz", ".zst", ".7z", ".rar", ".png", ".jpg", ".jpeg", ".webp", ".heic", ".pdf", ".mp3", ".aac", ".flac", ".mp4", ".m4v", ".mov", ".mkv", ".webm", ".docx", ".xlsx", ".pptx", ".apk", ".dmg":
		return zip.Store
	default:
		return zip.Deflate
	}
}

func (a archiveStream) directory(ctx context.Context, prefix string, entries []objectfmt.Entry) error {
	// Only shortened names can collide. Original names are unique in the
	// snapshot tree, and the shortening marker cannot occur in an encoded name.
	var shortened map[string]bool
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := archiveComponent(entry.Name)
		if len(name) > 255 {
			if shortened == nil {
				shortened = map[string]bool{}
			}
			name = archiveShortName(name, entry.Name, 245)
			base := name
			for i := 2; shortened[name]; i++ {
				name = base + "-" + strconv.Itoa(i)
			}
			shortened[name] = true
		}
		h := &zip.FileHeader{Name: prefix + name}
		if name != entry.Name {
			h.Comment = "Original name: " + entry.Name
		}
		if entry.MtimeNS != math.MinInt64 {
			h.Modified = time.Unix(0, entry.MtimeNS).UTC()
		}
		switch entry.Kind {
		case "directory":
			h.Name += "/"
			h.SetMode(os.ModeDir | 0755)
			if _, err := a.out.CreateHeader(h); err != nil {
				return err
			}
			children, err := objectfmt.ReadDir(entry.Digest, a.load)
			if err != nil {
				return err
			}
			if err := a.directory(ctx, h.Name, children); err != nil {
				return err
			}
		case "symlink":
			// A link target is user data; a ZIP extractor must never follow it.
			h.Comment = strings.TrimSpace(h.Comment + "\nSymbolic link target (stored as plain text)")
			h.SetMode(0644)
			w, err := a.out.CreateHeader(h)
			if err != nil {
				return err
			}
			if _, err = io.WriteString(w, entry.Target); err != nil {
				return err
			}
		case "file":
			info, err := objectfmt.ReadFile(entry.Digest, a.load)
			if err != nil {
				return err
			}
			h.Method = archiveFileMethod(name)
			h.SetMode(0644)
			if entry.Executable {
				h.SetMode(0755)
			}
			w, err := a.out.CreateHeader(h)
			if err != nil {
				return err
			}
			if err := objectfmt.WriteRange(info, 0, info.Size, w, a.load); err != nil {
				return err
			}
		default:
			return invalid("unknown archive entry kind")
		}
	}
	return nil
}

// A stalled HTTP client must release its download slot and the GC read fence.
// Refresh the deadline on progress so large, healthy downloads can run for hours.
type archiveOutput struct {
	http.ResponseWriter
	deadline time.Time
}

func (w *archiveOutput) Write(b []byte) (int, error) {
	now := time.Now()
	if w.deadline.Sub(now) < time.Minute {
		w.deadline = now.Add(2 * time.Minute)
		_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(w.deadline)
	}
	return w.ResponseWriter.Write(b)
}

func (s *Store) archive(w http.ResponseWriter, r *http.Request, dev string) error {
	ctx := r.Context()
	unit, snapshot := r.PathValue("unitId"), r.URL.Query().Get("snapshotId")
	if err := s.access(unit, dev, false); err != nil {
		return err
	}
	if !validID.MatchString(snapshot) {
		return bad("selected snapshotId required")
	}
	select {
	case s.archiveSlots <- struct{}{}:
		defer func() { <-s.archiveSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		return &problem{429, "ARCHIVE_BUSY", "ZIP downloads busy", "retry", nil}
	}
	s.archiveUse.RLock()
	defer s.archiveUse.RUnlock()
	var unitName, root string
	err := s.db.QueryRowContext(ctx, `SELECT u.name,v.root FROM units u JOIN visible_snapshots v ON v.unit=u.id WHERE u.id=? AND v.id=?`, unit, snapshot).Scan(&unitName, &root)
	if err != nil {
		return err
	}
	load := func(ref objectfmt.Ref) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return s.read(ref)
	}
	entries, err := objectfmt.ReadDir(root, load)
	if err != nil {
		return err
	}
	name := archiveShortName(archiveComponent(unitName), unitName, 230) + "-" + snapshot[len(snapshot)-10:]
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	w.Header().Set("Accept-Ranges", "none")
	if r.Method == http.MethodHead {
		return nil
	}
	// A streamed ZIP has no byte offsets until it is built. A Range request
	// receives the complete 200 representation, as permitted for unsupported ranges.
	output := &archiveOutput{ResponseWriter: w}
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	z := zip.NewWriter(output)
	// Entries are written serially. Reuse one fast compressor per stream so
	// downloads of code trees leave CPU time for the upload path.
	var compressor *flate.Writer
	z.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		if compressor == nil {
			var err error
			compressor, err = flate.NewWriter(w, flate.BestSpeed)
			return compressor, err
		}
		compressor.Reset(w)
		return compressor, nil
	})
	header := &zip.FileHeader{Name: name + "/"}
	header.SetMode(os.ModeDir | 0755)
	if _, err = z.CreateHeader(header); err == nil {
		err = z.Flush()
	}
	if err == nil {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		err = (archiveStream{out: z, load: load}).directory(ctx, header.Name, entries)
	}
	if err == nil {
		err = z.Close()
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("ZIP snapshot %s: %v", snapshot, err)
		}
		// Never send JSON or a successful central directory after a partial ZIP.
		panic(http.ErrAbortHandler)
	}
	return nil
}
