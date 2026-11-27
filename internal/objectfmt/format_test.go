package objectfmt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

type memoryStore map[Ref][]byte

func (m memoryStore) put(ref Ref, b []byte) error {
	m[ref] = b
	return nil
}
func (m memoryStore) get(ref Ref) ([]byte, error) {
	raw, ok := m[ref]
	if !ok {
		return nil, fmt.Errorf("missing %v", ref)
	}
	return raw, nil
}

func TestInteroperabilityVectors(t *testing.T) {
	m := memoryStore{}
	const emptyChecksum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	const abcChecksum = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if Digest(nil) != emptyChecksum || Digest([]byte("abc")) != abcChecksum {
		t.Fatal("raw vectors mismatch")
	}
	emptyFile, err := BuildFile(strings.NewReader(""), m.put)
	if err != nil || emptyFile != "15d856b25212a2fb5adad44e2979514fa3f96f2bc022ecab3be8ba21bc334938" {
		t.Fatal(emptyFile, err)
	}
	f, err := BuildFile(strings.NewReader("abc"), m.put)
	if err != nil || f != "93767d9a46cd34f2b0f34b2bcb221cec09e560014712d3f84d343e2eb03529db" {
		t.Fatal(f, err)
	}
	emptyDir, err := BuildDir(nil, m.put)
	if err != nil || emptyDir != "7326cd7e8bcace983896e6eb726579fb3bb3c038cc1e20968b0cafb8178422f1" {
		t.Fatal(emptyDir, err)
	}
	a := Entry{Name: "a.txt", Kind: "file", Digest: f}
	root, err := BuildDir([]Entry{a}, m.put)
	if err != nil || root != "4c20096fc8f960f04aa7211e6a2d28b83d68c2490b96ea8e632dc4975a416204" {
		t.Fatal(root, err)
	}
	root, err = BuildDir([]Entry{{Name: "empty", Kind: "directory", Digest: emptyDir}, a}, m.put)
	if err != nil || root != "1b3d5dd676cd805d5cf3e579682eb2901dd37c3c30d6ce82d368f9cbdc4d9d75" {
		t.Fatal(root, err)
	}
	if err := VerifyTree(root, m.get); err != nil {
		t.Fatal(err)
	}
	m[Ref{"chunk", abcChecksum}] = []byte("bad")
	if err := VerifyTree(root, m.get); !errors.Is(err, ErrInvalid) {
		t.Fatalf("corruption: %v", err)
	}
}

func TestCrossBlockRead(t *testing.T) {
	m := memoryStore{}
	input := bytes.Repeat([]byte("x"), ChunkSize)
	input = append(input, 'y', 'z')
	f, err := BuildFile(bytes.NewReader(input), m.put)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ReadFile(f, m.get)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != ChunkSize+2 || info.SHA256 != Digest(input) || len(info.Chunks) != 2 {
		t.Fatal(info)
	}
	var out bytes.Buffer
	if err := WriteRange(info, ChunkSize-1, 3, &out, m.get); err != nil || out.String() != "xyz" {
		t.Fatal(out.String(), err)
	}
	if err := WriteRange(info, info.Size, 1, io.Discard, m.get); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := VerifyFile(f, m.get); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalPagesAndLink(t *testing.T) {
	m := memoryStore{}
	const count = 257
	files := make([]Entry, count)
	for i := range files {
		files[i] = Entry{Name: fmt.Sprintf("%05d", count-1-i), Kind: "symlink", Target: "../../outside", MtimeNS: -979}
	}
	root, err := BuildDir(files, m.put)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := ReadDir(root, m.get)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(files)
	if !slices.Equal(actual, files) {
		t.Fatal("paged entries differ")
	}
	if err = VerifyTree(root, m.get); err != nil {
		t.Fatal(err)
	}
	rootRaw := m[Ref{"tree", root}]
	// A short page before the final child makes the tree noncanonical.
	bad := append([]byte{}, rootRaw[:9]...)
	bad = append(bad, rootRaw[41:]...)
	bad = append(bad, rootRaw[9:41]...)
	id, _ := put("tree", bad, m.put)
	if _, err := ReadDir(id, m.get); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	var small []string
	for i := 0; i < count; i++ {
		small = append(small, Digest([]byte(fmt.Sprint(i))))
	}
	root, err = storeFileRoot(ChunkSize*(count-1)+1, make([]byte, 32), small, m.put)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ReadFile(root, m.get)
	if err != nil || len(info.Chunks) != count {
		t.Fatal(err)
	}
	for i, item := range info.Chunks {
		if item.Digest != small[i] {
			t.Fatalf("wrong chunk %d", i)
		}
	}
}

func TestRejectInvalidManifest(t *testing.T) {
	m := memoryStore{}
	for _, entry := range []Entry{
		{Name: "..", Kind: "directory", Digest: Digest(nil)},
		{Name: "x", Kind: "file", Digest: ""},
		{Name: "link", Kind: "symlink", Target: "a\x00b"},
	} {
		if _, err := BuildDir([]Entry{entry}, m.put); !errors.Is(err, ErrInvalid) {
			t.Fatal(entry, err)
		}
	}
	for _, raw := range [][]byte{nil, []byte("KDTL"), append(header("KDTL", 0), 0), header("KDTL", 257)} {
		id, _ := put("tree", raw, m.put)
		if _, err := ReadDir(id, m.get); !errors.Is(err, ErrInvalid) {
			t.Fatal(raw, err)
		}
	}
	file, err := BuildFile(strings.NewReader("abc"), m.put)
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte{}, m[Ref{"file", file}]...)
	raw[12] ^= 0xff
	id, _ := put("file", raw, m.put)
	if err = VerifyFile(id, m.get); !errors.Is(err, ErrInvalid) {
		t.Fatal("whole digest mismatch", err)
	}
	binary.BigEndian.PutUint64(raw[4:12], 1<<60)
	id, _ = put("file", raw, m.put)
	if _, err = ReadFile(id, m.get); !errors.Is(err, ErrInvalid) {
		t.Fatal("file length limit", err)
	}
}

func TestFileBuilderMatchesImmutableBlocks(t *testing.T) {
	var builder FileBuilder
	content := bytes.Repeat([]byte("a"), ChunkSize+7)
	copy(content[ChunkSize:], "newtail")
	for _, size := range []int{0, 23, ChunkSize, len(content), 1} {
		src := content[:size]
		store := memoryStore{}
		id, err := builder.Build(bytes.NewReader(src), func(ref Ref, data []byte) error {
			return store.put(ref, bytes.Clone(data))
		})
		if err != nil {
			t.Fatal(err)
		}
		want, err := BuildFile(bytes.NewReader(src), (memoryStore{}).put)
		if err != nil || id != want {
			t.Fatalf("size %d: %s != %s (%v)", size, id, want, err)
		}
		if err := VerifyFile(id, store.get); err != nil {
			t.Fatal(err)
		}
	}
}
