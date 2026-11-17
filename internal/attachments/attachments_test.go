package attachments

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var png = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), "mem://")
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
	ctx := context.Background()
	s := open(t)
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
