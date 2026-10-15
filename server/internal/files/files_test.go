package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSniff(t *testing.T) {
	cases := map[string][]byte{
		"jpg":  {0xFF, 0xD8, 0xFF, 0xE0},
		"png":  {0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'},
		"webp": []byte("RIFF\x00\x00\x00\x00WEBPVP8 "),
		"heic": []byte("\x00\x00\x00\x18ftypheic\x00\x00"),
		"":     []byte("<html><script>"),
	}
	for want, head := range cases {
		if got := SniffImage(head); got != want {
			t.Errorf("SniffImage(%q) = %q, want %q", head, got, want)
		}
	}
}

func TestTempCommitAndSweep(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	tmp, _ := s.NewTemp()
	tmp.Write([]byte("hello"))
	dest := s.PageOrig("abcdefghijkl", 1, "jpg")
	if err := tmp.Commit(dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "hello" || tmp.Size() != 5 {
		t.Fatalf("got %q", b)
	}
	leftover, _ := s.NewTemp()
	leftover.Write([]byte("x"))
	if _, err := New(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover.Path()); !os.IsNotExist(err) {
		t.Fatal("tmp not swept")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "tmp")); len(entries) != 0 {
		t.Fatal("tmp not empty")
	}
}

func TestBadPIDPanics(t *testing.T) {
	s, _ := New(t.TempDir())
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	s.Audio("../../etc", 1)
}
