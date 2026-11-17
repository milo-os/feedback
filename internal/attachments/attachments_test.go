package attachments

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"

	"go.miloapis.com/feedback/internal/attachments/attachmentstest"
)

var png = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))

func open(t *testing.T) *Store { return openURL(t, "mem://") }

func openURL(t *testing.T, bucketURL string) *Store {
	t.Helper()
	s, err := Open(context.Background(), bucketURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCheck(t *testing.T) {
	s := open(t)
	s.MaxSize = 64
	for _, tc := range []struct {
		name, ct string
		data     []byte
		ok       bool
	}{
		{"shot.png", "image/png", png, true},
		{"notes.txt", "text/plain", []byte("hello"), true},
		{"page.html", "text/html", []byte("<b>"), false},
		{"shot.png", "image/png", []byte("not a png"), false},
		{"a/b.png", "image/png", png, false},
		{"big.txt", "text/plain", []byte(strings.Repeat("x", 65)), false},
		{"bad.txt", "text/plain", []byte{0xff, 0xfe}, false},
		{"empty.txt", "text/plain", nil, false},
	} {
		if msg := s.Check(tc.name, tc.ct, tc.data); (msg == "") != tc.ok {
			t.Errorf("Check(%s, %s) = %q", tc.name, tc.ct, msg)
		}
	}
}

func TestUploadAttachGetDelete(t *testing.T) {
	for name, bucketURL := range attachmentstest.Backends(t) {
		t.Run(name, func(t *testing.T) { testUploadAttachGetDelete(t, openURL(t, bucketURL)) })
	}
}

func testUploadAttachGetDelete(t *testing.T, s *Store) {
	ctx := context.Background()
	ref, sum, err := s.Upload(ctx, Submission, "u1", "shot.png", "image/png", png)
	if err != nil || !strings.HasPrefix(ref, "att-") || len(sum) != 64 {
		t.Fatalf("Upload = %q %q %v", ref, sum, err)
	}

	got, err := s.Lookup(ctx, Submission, "u1", []string{ref})
	if err != nil || len(got) != 1 || got[0].Name != "shot.png" || got[0].ContentType != "image/png" {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	for _, tc := range []struct {
		p     Purpose
		owner string
	}{{Submission, "u2"}, {SecurityReport, "u1"}} {
		if _, err := s.Lookup(ctx, tc.p, tc.owner, []string{ref}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Lookup as %s for %s: %v, want ErrNotFound", tc.owner, tc.p, err)
		}
	}

	// Before attaching, Get falls back to the owner's upload.
	if f, err := s.Get(ctx, Submission, "sub-1", "u1", ref); err != nil || f.Name != "shot.png" {
		t.Fatalf("Get before attach = %+v, %v", f, err)
	}
	for range 2 { // attaching again is harmless
		if err := s.Attach(ctx, Submission, "u1", "sub-1", []string{ref}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Attach(ctx, Submission, "u2", "sub-2", []string{ref}); !errors.Is(err, ErrNotFound) {
		t.Errorf("attaching someone else's upload: %v", err)
	}
	f, err := s.Get(ctx, Submission, "sub-1", "", ref)
	if err != nil || f.Name != "shot.png" || f.ContentType != "image/png" || f.Size != int64(len(png)) {
		t.Fatalf("Get after attach = %+v, %v", f, err)
	}
	if got := readAll(t, f); !bytes.Equal(got, png) {
		t.Fatalf("content after attach = %q", got)
	}

	if err := s.CopyParent(ctx, Submission, SecurityReport, "sub-1", "sec-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, SecurityReport, "sec-1", "", ref); err != nil {
		t.Errorf("copied report attachment: %v", err)
	}

	if err := s.DeleteParent(ctx, Submission, "sub-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, Submission, "sub-1", "", ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if _, err := f.Open(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("opening a file deleted since Get: %v", err)
	}
}

// TestStreamLargeFile reads a file bigger than the read buffers in pieces,
// as a download does.
func TestStreamLargeFile(t *testing.T) {
	for name, bucketURL := range attachmentstest.Backends(t) {
		t.Run(name, func(t *testing.T) {
			s := openURL(t, bucketURL)
			ctx := context.Background()
			data := make([]byte, 3<<20+7)
			_, _ = rand.Read(data)
			ref, _, err := s.Upload(ctx, Submission, "u1", "big.bin", "application/pdf", data)
			if err != nil {
				t.Fatal(err)
			}
			f, err := s.Get(ctx, Submission, "sub-1", "u1", ref)
			if err != nil || f.Size != int64(len(data)) {
				t.Fatalf("Get = %+v, %v", f, err)
			}
			if got := readAll(t, f); !bytes.Equal(got, data) {
				t.Fatalf("read %d bytes, want the %d uploaded", len(got), len(data))
			}
		})
	}
}

func readAll(t *testing.T, f *File) []byte {
	t.Helper()
	r, err := f.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPurposeOf(t *testing.T) {
	if p, ok := PurposeOf("submissions"); !ok || p != Submission {
		t.Error("submissions")
	}
	if p, ok := PurposeOf("securityreports"); !ok || p != SecurityReport {
		t.Error("securityreports")
	}
	if _, ok := PurposeOf("votes"); ok {
		t.Error("votes")
	}
}
