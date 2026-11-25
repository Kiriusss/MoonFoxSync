//go:build darwin && cgo

package engine

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// The writer is deliberately uncooperative (no flock). mmap-close explicitly
// disposes of its only descriptor while retaining a shared writable mapping.
func TestWorkspaceNativeFileHolder(t *testing.T) {
	if path := os.Getenv("MOONFOX_TEST_HOLDER_FILE"); path != "" {
		flags := os.O_RDWR
		mode := os.Getenv("MOONFOX_TEST_HOLDER_MODE")
		if mode == "reader" {
			flags = os.O_RDONLY
		}
		f, err := os.OpenFile(path, flags, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var mapping []byte
		if mode == "mmap-close" {
			mapping, err = unix.Mmap(int(f.Fd()), 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			defer unix.Munmap(mapping)
		}
		if _, err := os.Stdout.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		runtime.KeepAlive(f)
		runtime.KeepAlive(mapping)
		return
	}

	for _, mode := range []string{"reader", "writer", "mmap-close"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			active := filepath.Join(dir, "original")
			stash := filepath.Join(dir, "private-staging")
			if err := os.WriteFile(active, make([]byte, 4096), 0600); err != nil {
				t.Fatal(err)
			}
			own, err := os.Open(active)
			if err != nil {
				t.Fatal(err)
			}
			defer own.Close()
			if held, err := workspaceHeldByAnother(active); held || err != nil {
				t.Fatalf("own fd: held=%v error=%v", held, err)
			}
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWorkspaceNativeFileHolder$")
			child.Env = append(os.Environ(), "MOONFOX_TEST_HOLDER_FILE="+active, "MOONFOX_TEST_HOLDER_MODE="+mode)
			input, err := child.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				input.Close()
				if child.ProcessState == nil {
					child.Wait()
				}
			}()
			var ready [1]byte
			if _, err = io.ReadFull(output, ready[:]); err != nil || ready[0] != 1 {
				t.Fatalf("child startup: %v", err)
			}
			if err := os.Rename(active, stash); err != nil {
				t.Fatal(err)
			}
			if held, err := workspaceHeldByAnother(stash); !held || err != nil {
				t.Fatalf("renamed inode with %s: held=%v error=%v", mode, held, err)
			}
			input.Close()
			if err := child.Wait(); err != nil {
				t.Fatal(err)
			}
			if held, err := workspaceHeldByAnother(stash); held || err != nil {
				t.Fatalf("closed child: held=%v error=%v", held, err)
			}
			if _, err := workspaceHeldByAnother(active); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing source: %v", err)
			}
		})
	}
}

func TestWorkspaceNativeCopyMetadata(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	if err := os.WriteFile(src, []byte("source bytes"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("destination bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	xattr := []byte("fixture")
	if err := unix.Setxattr(src, "com.moonfox.test", xattr, 0); err != nil {
		t.Fatal(err)
	}
	a, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := workspaceCopyMetadata(a, b); err != nil {
		t.Fatal(err)
	}
	info, err := b.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ai, err := a.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != ai.Mode().Perm() {
		t.Fatalf("mode %v != %v", info.Mode(), ai.Mode())
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "destination bytes" {
		t.Fatalf("changed file data: %q %v", data, err)
	}
	out := make([]byte, len(xattr))
	n, err := unix.Getxattr(dst, "com.moonfox.test", out)
	if err != nil || n != len(xattr) || !bytes.Equal(xattr, out) {
		t.Fatalf("xattr %q %v", out, err)
	}
}
