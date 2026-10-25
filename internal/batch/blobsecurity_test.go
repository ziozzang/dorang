package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlobRejectsEscapesAndSymlinksAndClearsSpecialModes(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	ctx := context.Background()
	secret := filepath.Join(outside, "private")
	if err := os.WriteFile(secret, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := NewDiskBlobs(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"../private", "..", "/etc/passwd", "link"} {
		if f, err := d.Open(ctx, ref); err == nil {
			f.Close()
			t.Fatalf("accepted %q", ref)
		}
	}
	// Uploads replace the link itself using a fresh mode-0600 file. They must
	// never write through the link or preserve setuid/setgid permission bits.
	if _, err := d.Put(ctx, "link", strings.NewReader("upload")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(secret)
	if string(b) != "outside" {
		t.Fatal("upload wrote outside storage")
	}
	info, _ := os.Lstat(filepath.Join(root, "link"))
	if info.Mode() != 0600 {
		t.Fatalf("mode %v", info.Mode())
	}
	f, err := d.Open(ctx, "link")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ = io.ReadAll(f)
	if string(b) != "upload" {
		t.Fatal(string(b))
	}
}
