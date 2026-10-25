package parseguard

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type countingReader struct{ n int64 }

func (r *countingReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 'x'
	}
	r.n += int64(len(b))
	return len(b), nil
}

func TestReadStopsAtLimit(t *testing.T) {
	r := &countingReader{}
	b, err := Read(r, 1024)
	if b != nil || !errors.Is(err, ErrSize) || r.n != 1025 {
		t.Fatalf("body=%d err=%v consumed=%d", len(b), err, r.n)
	}
	if b, err := Read(strings.NewReader("four"), 4); err != nil || string(b) != "four" {
		t.Fatal(string(b), err)
	}
}

func TestOversizedSparseFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge.json")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := ReadFile(p); !errors.Is(err, ErrSize) {
		t.Fatal(err)
	}
	if _, err := ReadFile(t.TempDir()); err == nil {
		t.Fatal("accepted a directory")
	}
}

func TestJSONDepth(t *testing.T) {
	for _, n := range []int{1, MaxDepth, MaxDepth + 1, 10000} {
		b := []byte(strings.Repeat("[", n) + "0" + strings.Repeat("]", n))
		err := JSONDepth(b)
		if (err != nil) != (n > MaxDepth) {
			t.Fatalf("depth %d: %v", n, err)
		}
	}
	if err := JSONDepth([]byte(`{"text":"[[[\"[[[\\"}`)); err != nil {
		t.Fatal(err)
	}
	if b, err := Read(strings.NewReader(""), 1); err != nil && err != io.EOF || len(b) != 0 {
		t.Fatal(err)
	}
}
