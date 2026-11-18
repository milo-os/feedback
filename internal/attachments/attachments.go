// Package attachments stores the files attached to submissions and security
// reports in a private bucket. feedback-uploads writes uploads and serves
// files to people Milo says may read them; the operator copies attached
// uploads under their parent and deletes them with it.
//
// Layout:
//
//	uploads/{purpose}/{owner}/{ref}   an upload, attached or not
//	submissions/{name}/{ref}          attached to a submission
//	security-reports/{name}/{ref}     attached to a security report
//
// The bucket's lifecycle rule deletes uploads/ objects after a day. A file
// is safe once the operator has copied it under its parent, which it does as
// soon as the parent is created.
package attachments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob" // file:// for local development
	_ "gocloud.dev/blob/gcsblob"  // gs:// for Google Cloud Storage
	_ "gocloud.dev/blob/memblob"  // mem:// for tests
	_ "gocloud.dev/blob/s3blob"   // s3:// for Amazon S3 and S3-compatible stores
	"gocloud.dev/gcerrors"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// DefaultMaxSize is the largest file accepted by default.
const DefaultMaxSize = 10 << 20

// AllowedTypes are the media types that can be uploaded. Nothing that a
// browser would run (HTML, SVG) is allowed.
var AllowedTypes = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp",
	"application/pdf", "text/plain", "application/json",
}

// Purpose is what an upload is for. Uploads for one can't be attached to the
// other.
type Purpose string

const (
	Submission     Purpose = "submission"
	SecurityReport Purpose = "security-report"
)

// PurposeOf returns the purpose for a feedback resource, e.g. "submissions".
func PurposeOf(resource string) (Purpose, bool) {
	switch resource {
	case "submissions":
		return Submission, true
	case "securityreports":
		return SecurityReport, true
	}
	return "", false
}

// ErrNotFound is returned for an upload or attachment that doesn't exist,
// or belongs to someone else.
var ErrNotFound = errors.New("attachment not found")

// Store is the attachments bucket.
type Store struct {
	bucket  *blob.Bucket
	MaxSize int
}

// Open opens the bucket at bucketURL:
//
//   - gs://BUCKET for Google Cloud Storage, with the workload's credentials.
//   - s3://BUCKET?region=REGION for Amazon S3.
//   - s3://BUCKET?endpoint=https://HOST&region=REGION&use_path_style=true for
//     an S3-compatible store (Cloudflare R2, MinIO, Ceph and others). Add
//     request_checksum_calculation=when_required for stores that reject the
//     AWS SDK's default checksums.
//   - file:///PATH for local development and mem:// for tests.
//
// S3 credentials come from the AWS SDK's usual sources, normally the
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables. See
// gocloud.dev/blob/s3blob and gocloud.dev/aws for every URL parameter.
func Open(ctx context.Context, bucketURL string) (*Store, error) {
	b, err := blob.OpenBucket(ctx, bucketURL)
	if err != nil {
		return nil, fmt.Errorf("open attachments bucket: %w", err)
	}
	return &Store{bucket: b, MaxSize: DefaultMaxSize}, nil
}

// Close closes the bucket.
func (s *Store) Close() error { return s.bucket.Close() }

// ParentPrefix is where a parent's attachments live.
func ParentPrefix(p Purpose, parent string) string {
	if p == SecurityReport {
		return "security-reports/" + url.PathEscape(parent) + "/"
	}
	return "submissions/" + url.PathEscape(parent) + "/"
}

func uploadKey(p Purpose, owner, ref string) string {
	return "uploads/" + string(p) + "/" + url.PathEscape(owner) + "/" + url.PathEscape(ref)
}

// Check validates a file before it is stored. It returns a message for the
// caller, or "".
func (s *Store) Check(name, contentType string, data []byte) string {
	switch {
	case name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\\x00"):
		return "the file name must be at most 255 characters, without slashes"
	case !slices.Contains(AllowedTypes, contentType):
		return "Content-Type must be one of " + strings.Join(AllowedTypes, ", ")
	case len(data) == 0:
		return "the file is empty"
	case len(data) > s.MaxSize:
		return fmt.Sprintf("the file is larger than %d bytes", s.MaxSize)
	}
	// The declared type must match the content, so a file served back to
	// staff is what it claims to be.
	switch {
	case strings.HasPrefix(contentType, "text/") || contentType == "application/json":
		if !utf8.Valid(data) {
			return contentType + " must be UTF-8 text"
		}
	default:
		if got := http.DetectContentType(data); got != contentType {
			return fmt.Sprintf("the content looks like %s, not %s", got, contentType)
		}
	}
	return ""
}

// Upload stores data as an upload by owner and returns its ref and SHA-256.
// Call Check first.
func (s *Store) Upload(ctx context.Context, p Purpose, owner, name, contentType string, data []byte) (ref, sum string, err error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	ref = "att-" + hex.EncodeToString(b)
	h := sha256.Sum256(data)
	sum = hex.EncodeToString(h[:])
	err = s.bucket.WriteAll(ctx, uploadKey(p, owner, ref), data, &blob.WriterOptions{
		ContentType: contentType,
		Metadata:    map[string]string{"name": name, "sha256": sum},
	})
	return ref, sum, err
}

// Lookup returns references, with their stored names and types, for owner's
// uploads. A ref that isn't owner's upload for this purpose is ErrNotFound.
func (s *Store) Lookup(ctx context.Context, p Purpose, owner string, refs []string) ([]v1.AttachmentReference, error) {
	out := make([]v1.AttachmentReference, 0, len(refs))
	for _, ref := range refs {
		attrs, err := s.bucket.Attributes(ctx, uploadKey(p, owner, ref))
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v1.AttachmentReference{Name: attrs.Metadata["name"], ContentType: attrs.ContentType, UploadRef: ref})
	}
	return out, nil
}

// Attach copies owner's uploads under parent. Copying again is harmless.
func (s *Store) Attach(ctx context.Context, p Purpose, owner, parent string, refs []string) error {
	for _, ref := range refs {
		dst := ParentPrefix(p, parent) + url.PathEscape(ref)
		if ok, err := s.bucket.Exists(ctx, dst); err != nil {
			return err
		} else if ok {
			continue
		}
		err := s.bucket.Copy(ctx, dst, uploadKey(p, owner, ref), nil)
		if gcerrors.Code(err) == gcerrors.NotFound {
			return fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// CopyParent copies a parent's attachments to another parent, e.g. when a
// submission becomes a security report.
func (s *Store) CopyParent(ctx context.Context, from, to Purpose, fromParent, toParent string) error {
	src := ParentPrefix(from, fromParent)
	it := s.bucket.List(&blob.ListOptions{Prefix: src})
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.bucket.Copy(ctx, ParentPrefix(to, toParent)+strings.TrimPrefix(obj.Key, src), obj.Key, nil); err != nil {
			return err
		}
	}
}

// File is an attached file. Get returns its name, type and size; Open
// streams its content, so serving a file never holds it in memory.
type File struct {
	Name        string
	ContentType string
	Size        int64

	bucket *blob.Bucket
	key    string
}

// Get finds an attachment of parent. Until the operator has copied it, it is
// read from owner's uploads.
func (s *Store) Get(ctx context.Context, p Purpose, parent, owner, ref string) (*File, error) {
	f, err := s.stat(ctx, ParentPrefix(p, parent)+url.PathEscape(ref))
	if errors.Is(err, ErrNotFound) && owner != "" {
		return s.stat(ctx, uploadKey(p, owner, ref))
	}
	return f, err
}

func (s *Store) stat(ctx context.Context, key string) (*File, error) {
	attrs, err := s.bucket.Attributes(ctx, key)
	if gcerrors.Code(err) == gcerrors.NotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &File{
		Name: attrs.Metadata["name"], ContentType: attrs.ContentType, Size: attrs.Size,
		bucket: s.bucket, key: key,
	}, nil
}

// Open returns a reader for the file's content. ctx bounds the whole read,
// not just opening it. The caller must close the reader. A file deleted since
// Get is ErrNotFound.
func (f *File) Open(ctx context.Context) (io.ReadCloser, error) {
	r, err := f.bucket.NewReader(ctx, f.key, nil)
	if gcerrors.Code(err) == gcerrors.NotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// DeleteParent deletes every attachment of a parent.
func (s *Store) DeleteParent(ctx context.Context, p Purpose, parent string) error {
	it := s.bucket.List(&blob.ListOptions{Prefix: ParentPrefix(p, parent)})
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.bucket.Delete(ctx, obj.Key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			return err
		}
	}
}
