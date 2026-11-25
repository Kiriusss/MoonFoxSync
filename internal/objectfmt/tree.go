package objectfmt

import (
	"encoding/binary"
	"sort"
	"strings"
)

func appendEntry(buf []byte, entry Entry) ([]byte, error) {
	if !validName(entry.Name) {
		return nil, invalid("invalid directory entry name")
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(entry.Name)))
	buf = append(buf, entry.Name...)
	var typ byte
	switch entry.Kind {
	case "file":
		typ = 1
	case "directory":
		typ = 2
	case "symlink":
		typ = 3
	default:
		return nil, invalid("unknown entry kind")
	}
	buf = append(buf, typ)
	buf = binary.BigEndian.AppendUint64(buf, uint64(entry.MtimeNS))
	switch typ {
	case 1:
		if entry.Executable {
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
	case 3:
		if len(entry.Target) == 0 || len(entry.Target) > 4095 || strings.ContainsRune(entry.Target, 0) {
			return nil, invalid("invalid symbolic link target")
		}
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(entry.Target)))
		return append(buf, entry.Target...), nil
	}
	b, err := rawDigest(entry.Digest)
	if err != nil {
		return nil, err
	}
	return append(buf, b...), nil
}

// BuildDir orders direct children by raw UTF-8 name and emits canonical tree pages.
func BuildDir(entries []Entry, store Store) (string, error) {
	if len(entries) > maxEntries {
		return "", invalid("directory entry limit exceeded")
	}
	if len(entries) == 0 {
		return put("tree", header("KDTL", 0), store)
	}
	ordered := append([]Entry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	var pages []string
	for i := 0; i < len(ordered); i += fanout {
		batch := ordered[i:min(i+fanout, len(ordered))]
		raw := header("KDTL", len(batch))
		for j, e := range batch {
			if i+j > 0 && ordered[i+j-1].Name == e.Name {
				return "", invalid("duplicate directory name")
			}
			var err error
			raw, err = appendEntry(raw, e)
			if err != nil {
				return "", err
			}
		}
		id, err := put("tree", raw, store)
		if err != nil {
			return "", err
		}
		pages = append(pages, id)
	}
	id, _, err := buildIndex("tree", "KDTI", pages, store)
	return id, err
}

func readEntry(d *decoder) (Entry, error) {
	var e Entry
	e.Name = string(d.take(d.u16()))
	if !validName(e.Name) {
		return e, invalid("invalid entry name")
	}
	typ := d.u8()
	e.MtimeNS = int64(d.u64())
	switch typ {
	case 1:
		e.Kind = "file"
		flag := d.u8()
		if flag > 1 {
			return e, invalid("invalid executable bit")
		}
		e.Executable = flag == 1
		e.Digest = d.hash()
	case 2:
		e.Kind = "directory"
		e.Digest = d.hash()
	case 3:
		e.Kind = "symlink"
		e.Target = string(d.take(d.u16()))
		if len(e.Target) < 1 || len(e.Target) > 4095 || strings.ContainsRune(e.Target, 0) {
			return e, invalid("invalid symbolic link target")
		}
	default:
		return e, invalid("invalid entry kind")
	}
	return e, d.err
}

// ReadDir expands the immediate entries, including directory index pages.
// It does not follow child directory, file or link entries.
func ReadDir(id string, load Load) ([]Entry, error) {
	var out []Entry
	err := readTreePage(id, load, -1, true, &out)
	return out, err
}

func readTreePage(id string, load Load, wantHeight int, rightmost bool, out *[]Entry) error {
	raw, err := get(Ref{"tree", id}, load)
	if err != nil {
		return err
	}
	d := decoder{rest: raw}
	root := wantHeight < 0
	switch string(d.take(4)) {
	case "KDTL":
		n := int(d.u32())
		if wantHeight > 0 || n > fanout || (!root && n == 0) || (!rightmost && n != fanout) {
			return invalid("noncanonical tree leaf")
		}
		if len(*out)+n > maxEntries {
			return invalid("directory entry limit exceeded")
		}
		for i := 0; i < n; i++ {
			e, err := readEntry(&d)
			if err != nil {
				return err
			}
			if len(*out) > 0 && (*out)[len(*out)-1].Name >= e.Name {
				return invalid("unordered directory entries")
			}
			*out = append(*out, e)
		}
	case "KDTI":
		height := int(d.u8())
		n := int(d.u32())
		// With maxEntries and fanout a directory needs at most two index levels.
		if height < 1 || height > 2 || (!root && height != wantHeight) || n < 1 || n > fanout || (root && n < 2) || (!rightmost && n != fanout) {
			return invalid("noncanonical tree index")
		}
		if len(d.rest) != n*32 {
			return invalid("invalid tree index length")
		}
		for i := 0; i < n; i++ {
			if err := readTreePage(d.hash(), load, height-1, rightmost && i == n-1, out); err != nil {
				return err
			}
		}
	default:
		return invalid("expected directory page")
	}
	return d.done()
}

type treeSize struct {
	entries int
	depth   int // number of nested directories below this root
}

// VerifyTree verifies the referenced graph and computes logical entry/depth
// limits even for DAGs that repeatedly mount the same immutable subtree.
func VerifyTree(id string, load Load) error {
	verifiedDirs := map[string]treeSize{}
	verifiedFiles := map[string]bool{}
	var walk func(string, int) (treeSize, error)
	walk = func(id string, depth int) (treeSize, error) {
		var sz treeSize
		if depth > maxDirDepth {
			return sz, invalid("snapshot nesting limit exceeded")
		}
		if hit, ok := verifiedDirs[id]; ok {
			if depth+hit.depth > maxDirDepth {
				return sz, invalid("snapshot nesting limit exceeded")
			}
			return hit, nil
		}
		entries, err := ReadDir(id, load)
		if err != nil {
			return sz, err
		}
		sz.entries = len(entries)
		for _, entry := range entries {
			switch entry.Kind {
			case "file":
				if !verifiedFiles[entry.Digest] {
					if err := VerifyFile(entry.Digest, load); err != nil {
						return sz, err
					}
					verifiedFiles[entry.Digest] = true
				}
			case "directory":
				child, err := walk(entry.Digest, depth+1)
				if err != nil {
					return sz, err
				}
				if child.entries > maxEntries-sz.entries {
					return sz, invalid("snapshot entry limit exceeded")
				}
				sz.entries += child.entries
				sz.depth = max(sz.depth, child.depth+1)
			}
		}
		verifiedDirs[id] = sz
		return sz, nil
	}
	_, err := walk(id, 0)
	return err
}
