// Package attachmentstest provides attachment buckets for tests.
package attachmentstest

import (
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// FakeS3 serves an S3-compatible API with one bucket, in process, and returns
// its bucket URL, as an S3-compatible store outside AWS would be configured.
func FakeS3(t testing.TB) string {
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

// Backends returns the bucket URLs store tests run against: memory, a fake
// S3-compatible store, and FEEDBACK_TEST_S3_URL when it is set, to check a
// real store such as MinIO or R2 (with its credentials in the AWS_*
// environment variables).
func Backends(t testing.TB) map[string]string {
	t.Helper()
	b := map[string]string{"mem": "mem://", "s3-compatible": FakeS3(t)}
	if u := os.Getenv("FEEDBACK_TEST_S3_URL"); u != "" {
		b["FEEDBACK_TEST_S3_URL"] = u
	}
	return b
}
