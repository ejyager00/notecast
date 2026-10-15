// Package files owns the on-disk layout under DATA_DIR (ARCHITECTURE §4.3).
// Every write is atomic: temp file in DATA_DIR/tmp, fsync, rename.
package files

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

type Store struct{ root string }

// New prepares the layout and empties tmp/ (leftovers from a crash).
func New(root string) (*Store, error) {
	for _, d := range []string{"pages", "audio", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			return nil, err
		}
	}
	tmp := filepath.Join(root, "tmp")
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		os.RemoveAll(filepath.Join(tmp, e.Name()))
	}
	return &Store{root: root}, nil
}

var pidRe = regexp.MustCompile(`^[a-z0-9]{12}$`)

func checkPID(pid string) {
	// pids come from our own database, but a path built from anything else
	// is a traversal bug waiting to happen; fail loudly.
	if !pidRe.MatchString(pid) {
		panic("files: bad note pid " + pid)
	}
}

func (s *Store) PageOrig(pid string, page int64, ext string) string {
	checkPID(pid)
	return filepath.Join(s.root, "pages", pid, fmt.Sprintf("%d.orig.%s", page, ext))
}

func (s *Store) PageDisplay(pid string, page int64) string {
	checkPID(pid)
	return filepath.Join(s.root, "pages", pid, fmt.Sprintf("%d.display.jpg", page))
}

func (s *Store) PageThumb(pid string, page int64) string {
	checkPID(pid)
	return filepath.Join(s.root, "pages", pid, fmt.Sprintf("%d.thumb.jpg", page))
}

func (s *Store) Audio(pid string, version int64) string {
	checkPID(pid)
	return filepath.Join(s.root, "audio", pid, fmt.Sprintf("%d.mp3", version))
}

func (s *Store) Chapters(pid string, version int64) string {
	checkPID(pid)
	return filepath.Join(s.root, "audio", pid, fmt.Sprintf("%d.chapters.json", version))
}

// RemoveNote deletes everything stored for a note.
func (s *Store) RemoveNote(pid string) error {
	checkPID(pid)
	return errors.Join(
		os.RemoveAll(filepath.Join(s.root, "pages", pid)),
		os.RemoveAll(filepath.Join(s.root, "audio", pid)),
	)
}

// Temp is a file being written in tmp/, published by Commit.
type Temp struct {
	f      *os.File
	h      hash
	n      int64
	closed bool
}

type hash interface {
	io.Writer
	Sum([]byte) []byte
}

func (s *Store) NewTemp() (*Temp, error) {
	f, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "up-*")
	if err != nil {
		return nil, err
	}
	return &Temp{f: f, h: sha256.New()}, nil
}

func (t *Temp) Write(p []byte) (int, error) {
	n, err := t.f.Write(p)
	t.h.Write(p[:n])
	t.n += int64(n)
	return n, err
}

func (t *Temp) Size() int64    { return t.n }
func (t *Temp) SHA256() string { return hex.EncodeToString(t.h.Sum(nil)) }
func (t *Temp) Path() string   { return t.f.Name() }

// Commit fsyncs and renames into place, creating the parent directory.
func (t *Temp) Commit(dest string) error {
	if err := t.f.Sync(); err != nil {
		t.Discard()
		return err
	}
	if err := t.f.Close(); err != nil {
		t.closed = true
		t.Discard()
		return err
	}
	t.closed = true
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		os.Remove(t.f.Name())
		return err
	}
	return os.Rename(t.f.Name(), dest)
}

// Discard removes the temp file; safe after Commit (no-op on the moved file).
func (t *Temp) Discard() {
	if !t.closed {
		t.f.Close()
		t.closed = true
	}
	os.Remove(t.f.Name())
}

// WriteFile atomically writes data to dest.
func (s *Store) WriteFile(dest string, data []byte) error {
	t, err := s.NewTemp()
	if err != nil {
		return err
	}
	if _, err := t.Write(data); err != nil {
		t.Discard()
		return err
	}
	return t.Commit(dest)
}

// SniffImage identifies an upload by magic bytes. It returns the extension
// we store it under, or "" if it isn't an allowed image type. The server
// never decodes images; this only decides what we accept and how we label it.
func SniffImage(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg"
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}):
		return "png"
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "webp"
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")):
		switch string(head[8:12]) {
		case "heic", "heix", "hevc", "hevx", "mif1", "msf1":
			return "heic"
		}
	}
	return ""
}

// ContentType for a stored extension; only ever one of SniffImage's results.
func ContentType(ext string) string {
	switch ext {
	case "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "heic":
		return "image/heic"
	}
	return "application/octet-stream"
}

// IsJPEG and IsMP3 validate worker output before it is stored.
func IsJPEG(b []byte) bool { return bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}) }

func IsMP3(b []byte) bool {
	return bytes.HasPrefix(b, []byte("ID3")) || (len(b) > 1 && b[0] == 0xFF && b[1]&0xE0 == 0xE0)
}
