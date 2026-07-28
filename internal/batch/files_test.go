package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	content := jsonlFile(3, "m1")

	f, err := h.svc.UploadFile(ctx, UploadRequest{
		Filename: "in.jsonl", Purpose: PurposeBatch, OwnerKeyID: "key-1",
		Content: strings.NewReader(content), ExpiresAfter: time.Hour, Authorize: allowAllModels,
	})
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if f.Object != ObjectFile || f.Purpose != PurposeBatch || f.Status != FileProcessed {
		t.Errorf("file object = %+v", f)
	}
	if f.Bytes != int64(len(content)) {
		t.Errorf("bytes = %d, want %d", f.Bytes, len(content))
	}
	if f.ExpiresAt == 0 || f.CreatedAt == 0 {
		t.Errorf("timestamps = created %d expires %d", f.CreatedAt, f.ExpiresAt)
	}

	got, err := h.svc.RetrieveFile(ctx, f.ID, "key-1")
	if err != nil || got.ID != f.ID {
		t.Fatalf("RetrieveFile = %+v, %v", got, err)
	}
	if _, err := h.svc.RetrieveFile(ctx, f.ID, "other-key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another key retrieved the file: %v", err)
	}

	rd, rec, err := h.svc.FileContent(ctx, f.ID, "key-1")
	if err != nil {
		t.Fatalf("FileContent: %v", err)
	}
	body, _ := io.ReadAll(rd)
	rd.Close()
	if string(body) != content {
		t.Errorf("content round trip mismatch")
	}
	if rec.SHA256 == "" {
		t.Error("no digest recorded")
	}

	list, err := h.svc.ListFiles(ctx, FileQuery{OwnerKeyID: "key-1", Purpose: PurposeBatch, Limit: 10})
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != f.ID {
		t.Fatalf("list = %+v", list.Data)
	}

	del, err := h.svc.DeleteFile(ctx, f.ID, "key-1")
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if !del.Deleted || del.Object != ObjectFile {
		t.Errorf("delete response = %+v", del)
	}
	if _, err := h.svc.RetrieveFile(ctx, f.ID, "key-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("file still retrievable after delete: %v", err)
	}
	if h.blobs.count() != 0 {
		t.Errorf("%d blobs left after delete", h.blobs.count())
	}
}

// batch_output is a purpose dorang produces. Letting a client upload with it
// would let arbitrary content masquerade as another caller's results.
func TestUploadRejectsReservedPurpose(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Purpose: PurposeBatchOutput, Content: strings.NewReader("{}"), Authorize: allowAllModels,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// Only batch input files are parsed. Anything else is bytes.
func TestNonBatchPurposeIsNotValidated(t *testing.T) {
	h := newHarness(t, nil)
	f, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Purpose: PurposeUserData, Filename: "notes.txt",
		Content: strings.NewReader("this is not JSONL at all\n"),
	})
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if f.Purpose != PurposeUserData {
		t.Errorf("purpose = %q", f.Purpose)
	}
}

func TestCreateRejectsWrongPurposeAndUnknownFile(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	f, err := h.svc.UploadFile(ctx, UploadRequest{
		Purpose: PurposeUserData, OwnerKeyID: "key-1", Content: strings.NewReader("hello"),
	})
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	_, err = h.svc.Create(ctx, CreateRequest{
		InputFileID: f.ID, Endpoint: "/v1/chat/completions", OwnerKeyID: "key-1",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("create with a user_data file = %v, want ErrInvalidRequest", err)
	}

	_, err = h.svc.Create(ctx, CreateRequest{
		InputFileID: "file-nope", Endpoint: "/v1/chat/completions",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("create with an unknown file = %v, want ErrNotFound", err)
	}

	fid := h.upload(jsonlFile(1, "m1"))
	_, err = h.svc.Create(ctx, CreateRequest{
		InputFileID: fid, Endpoint: "/v1/audio/speech", OwnerKeyID: "key-1",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("create with a non-batch endpoint = %v, want ErrInvalidRequest", err)
	}

	_, err = h.svc.Create(ctx, CreateRequest{
		InputFileID: fid, Endpoint: "/v1/chat/completions", OwnerKeyID: "key-1",
		CompletionWindow: "1s",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("create with a 1s window = %v, want ErrInvalidRequest", err)
	}

	long := strings.Repeat("v", DefaultMaxMetadataValChars+1)
	_, err = h.svc.Create(ctx, CreateRequest{
		InputFileID: fid, Endpoint: "/v1/chat/completions", OwnerKeyID: "key-1",
		Metadata: map[string]string{"k": long},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("create with oversized metadata = %v, want ErrInvalidRequest", err)
	}
}

func TestMetadataAndWindowRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	fid := h.upload(jsonlFile(2, "m1"))
	b, err := h.svc.Create(context.Background(), CreateRequest{
		InputFileID: fid, Endpoint: "/v1/chat/completions", OwnerKeyID: "key-1",
		CompletionWindow: "24h", Metadata: map[string]string{"job": "nightly"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.CompletionWindow != "24h" {
		t.Errorf("completion_window = %q, want 24h", b.CompletionWindow)
	}
	if b.Metadata["job"] != "nightly" {
		t.Errorf("metadata = %v", b.Metadata)
	}
	if b.ExpiresAt == 0 || b.ExpiresAt <= b.CreatedAt {
		t.Errorf("expires_at = %d, created_at = %d", b.ExpiresAt, b.CreatedAt)
	}
	final := h.await(b.ID, StatusCompleted)
	if final.Metadata["job"] != "nightly" {
		t.Errorf("metadata lost: %v", final.Metadata)
	}
}

// DiskBlobs is the shipped implementation, so it gets the same round trip plus
// the properties the scheduler depends on: seekable reads and a ref that cannot
// escape the root.
func TestDiskBlobs(t *testing.T) {
	root := t.TempDir()
	d, err := NewDiskBlobs(root)
	if err != nil {
		t.Fatalf("NewDiskBlobs: %v", err)
	}
	ctx := context.Background()

	n, err := d.Put(ctx, "file-abc", strings.NewReader("hello world"))
	if err != nil || n != 11 {
		t.Fatalf("Put = %d, %v", n, err)
	}
	rd, err := d.Open(ctx, "file-abc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := rd.Seek(6, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(rd, buf); err != nil || string(buf) != "world" {
		t.Errorf("read at offset = %q, %v", buf, err)
	}
	rd.Close()

	// Replacing content is atomic, and no temporary files are left behind.
	if _, err := d.Put(ctx, "file-abc", strings.NewReader("second")); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("root holds %v, want just the one blob", names)
	}

	for _, bad := range []string{"../escape", "a/b", "", ".."} {
		if _, err := d.Open(ctx, bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Open(%q) = %v, want ErrInvalidRequest", bad, err)
		}
	}
	if _, err := d.Open(ctx, "file-missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open of a missing blob = %v, want ErrNotFound", err)
	}
	if err := d.Remove(ctx, "file-missing"); err != nil {
		t.Errorf("Remove of a missing blob = %v, want nil", err)
	}
	if err := d.Remove(ctx, "file-abc"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "file-abc")); !os.IsNotExist(err) {
		t.Errorf("blob still on disk: %v", err)
	}
}

// The whole service works against the shipped disk implementation, not only the
// in-memory one the other tests use.
func TestEndToEndOnDisk(t *testing.T) {
	root := t.TempDir()
	d, err := NewDiskBlobs(root)
	if err != nil {
		t.Fatalf("NewDiskBlobs: %v", err)
	}
	h := newHarness(t, func(c *Config) { c.Blobs = d })

	fid := h.upload(jsonlFile(25, "m1"))
	b := h.create(fid)
	final := h.await(b.ID, StatusCompleted)
	if final.RequestCounts != (RequestCounts{Total: 25, Completed: 25}) {
		t.Fatalf("counts = %+v", final.RequestCounts)
	}
	if got := len(h.lines(final.OutputFileID)); got != 25 {
		t.Errorf("output file has %d lines, want 25", got)
	}
}

// An unowned record is not everybody's.
//
// ownedBy(recordOwner, owner) returned true whenever recordOwner was "", so a
// file or batch created with an empty OwnerKeyID was readable, usable and
// deletable by every key in the deployment. A master-credential upload produced
// exactly such a record: the master has no api_keys row, so its key id is "".
//
// The write path now records a real owner for the master (internal/app's
// MasterOwnerID) and this is the read side: an empty owner on a record is no
// longer a wildcard.
func TestAnUnownedRecordIsNotVisibleToEveryKey(t *testing.T) {
	cases := []struct {
		name               string
		recordOwner, owner string
		want               bool
	}{
		{"a key sees its own", "key-a", "key-a", true},
		{"a key does not see another's", "key-a", "key-b", false},
		{"a key does not see an unowned record", "", "key-a", false},
		{"an administrative caller sees an owned record", "key-a", "", true},
		{"an administrative caller sees an unowned record", "", "", true},
	}
	for _, c := range cases {
		if got := ownedBy(c.recordOwner, c.owner); got != c.want {
			t.Errorf("%s: ownedBy(%q, %q) = %v, want %v",
				c.name, c.recordOwner, c.owner, got, c.want)
		}
	}
}
