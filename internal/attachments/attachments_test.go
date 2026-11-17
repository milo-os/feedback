package attachments

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
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

// fakeS3 serves an S3-compatible API with one bucket and returns its bucket
// URL, as an S3-compatible store outside AWS would be configured.
func fakeS3(t *testing.T) string {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("attachments"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	q := url.Values{
		"endpoint":       {srv.URL},
		"region":         {"auto"},
		"use_path_style": {"true"},
	}
	return "s3://attachments?" + q.Encode()
}

// backends returns the bucket URLs every store test runs against: memory, a
// fake S3-compatible store, and FEEDBACK_TEST_S3_URL when it is set, to check
// a real store such as MinIO or R2 (with its credentials in the AWS_*
// environment variables).
func backends(t *testing.T) map[string]string {
	t.Helper()
	b := map[string]string{"mem": "mem://", "s3-compatible": fakeS3(t)}
	if u := os.Getenv("FEEDBACK_TEST_S3_URL"); u != "" {
		b["FEEDBACK_TEST_S3_URL"] = u
	}
	return b
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
	for name, bucketURL := range backends(t) {
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
	if f, err := s.Get(ctx, Submission, "sub-1", "", ref); err != nil || string(f.Data) != string(png) {
		t.Fatalf("Get after attach = %v", err)
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
