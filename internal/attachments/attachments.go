// Package attachments stores the files attached to submissions and security
// reports in a private bucket. Only the apiserver reads or writes the
// bucket; files reach people through the API, which checks who is asking.
//
// Layout:
//
//	uploads/{purpose}/{owner}/{ref}   an upload not yet attached
//	submissions/{name}/{ref}          attached to a submission
//	security-reports/{name}/{ref}     attached to a security report
//
// Attaching copies an upload under its parent and leaves the upload for the
// bucket's lifecycle rule to delete, so a create that rolls back loses
// nothing. The rule should delete uploads/ objects after a day.
package attachments

import (
	"bytes"
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
	_ "gocloud.dev/blob/gcsblob"  // gs:// in clusters
	_ "gocloud.dev/blob/memblob"  // mem:// for tests
	"gocloud.dev/gcerrors"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// DefaultMaxSize keeps a base64-encoded upload under the apiserver's 3 MiB
// request body limit.
const DefaultMaxSize = 2 << 20

// AllowedTypes are the media types that can be uploaded. Nothing that a
// browser would run (HTML, SVG) is allowed.
var AllowedTypes = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp",
	"application/pdf", "text/plain", "application/json",
}

// ErrNotFound is returned for an upload or attachment that doesn't exist,
// or belongs to someone else.
var ErrNotFound = errors.New("attachment not found")

// Store is the attachments bucket.
type Store struct {
	bucket  *blob.Bucket
	MaxSize int
}

// Open opens the bucket at url, e.g. gs://feedback-attachments, file:///tmp/a
// or mem://.
func Open(ctx context.Context, bucketURL string) (*Store, error) {
	b, err := blob.OpenBucket(ctx, bucketURL)
	if err != nil {
		return nil, fmt.Errorf("open attachments bucket: %w", err)
	}
	return &Store{bucket: b, MaxSize: DefaultMaxSize}, nil
}

// Close closes the bucket.
func (s *Store) Close() error { return s.bucket.Close() }

func purposeDir(p v1.AttachmentPurpose) string {
	if p == v1.AttachmentPurposeSecurityReport {
		return "security-report"
	}
	return "submission"
}

// ParentPrefix is where a parent's attachments live.
func ParentPrefix(p v1.AttachmentPurpose, parent string) string {
	if p == v1.AttachmentPurposeSecurityReport {
		return "security-reports/" + url.PathEscape(parent) + "/"
	}
	return "submissions/" + url.PathEscape(parent) + "/"
}

func uploadKey(p v1.AttachmentPurpose, owner, ref string) string {
	return "uploads/" + purposeDir(p) + "/" + url.PathEscape(owner) + "/" + url.PathEscape(ref)
}

// Check validates a file before it is stored. It returns a message for the
// caller, or "".
func (s *Store) Check(name, contentType string, data []byte) string {
	switch {
	case name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\\x00"):
		return "name must be a file name of at most 255 characters"
	case !slices.Contains(AllowedTypes, contentType):
		return "contentType must be one of " + strings.Join(AllowedTypes, ", ")
	case len(data) == 0:
		return "data is empty"
	case len(data) > s.MaxSize:
		return fmt.Sprintf("data is larger than %d bytes", s.MaxSize)
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
			return fmt.Sprintf("content looks like %s, not %s", got, contentType)
		}
	}
	return ""
}

// Upload stores data as an upload by owner and returns its ref and SHA-256.
// Call Check first.
func (s *Store) Upload(ctx context.Context, purpose v1.AttachmentPurpose, owner, name, contentType string, data []byte) (ref, sum string, err error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	ref = "att-" + hex.EncodeToString(b)
	h := sha256.Sum256(data)
	sum = hex.EncodeToString(h[:])
	err = s.bucket.WriteAll(ctx, uploadKey(purpose, owner, ref), data, &blob.WriterOptions{
		ContentType: contentType,
		Metadata:    map[string]string{"name": name, "sha256": sum},
	})
	return ref, sum, err
}

// Attach copies owner's uploads under the parent and returns references
// with their stored names and types. A ref that isn't owner's upload for
// this purpose is ErrNotFound.
func (s *Store) Attach(ctx context.Context, purpose v1.AttachmentPurpose, owner, parent string, refs []string) ([]v1.AttachmentReference, error) {
	out := make([]v1.AttachmentReference, 0, len(refs))
	for _, ref := range refs {
		src := uploadKey(purpose, owner, ref)
		attrs, err := s.bucket.Attributes(ctx, src)
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		if err != nil {
			return nil, err
		}
		if err := s.bucket.Copy(ctx, ParentPrefix(purpose, parent)+url.PathEscape(ref), src, nil); err != nil {
			return nil, err
		}
		out = append(out, v1.AttachmentReference{Name: attrs.Metadata["name"], ContentType: attrs.ContentType, UploadRef: ref})
	}
	return out, nil
}

// CopyParent copies a parent's attachments to another parent, e.g. when a
// submission becomes a security report. Deleting the old parent deletes the
// originals.
func (s *Store) CopyParent(ctx context.Context, from, to v1.AttachmentPurpose, fromParent, toParent string) error {
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

// File is an attached file.
type File struct {
	Name        string
	ContentType string
	Data        []byte
}

// Get reads an attachment of a parent.
func (s *Store) Get(ctx context.Context, purpose v1.AttachmentPurpose, parent, ref string) (*File, error) {
	key := ParentPrefix(purpose, parent) + url.PathEscape(ref)
	r, err := s.bucket.NewReader(ctx, key, nil)
	if gcerrors.Code(err) == gcerrors.NotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	attrs, err := s.bucket.Attributes(ctx, key)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(r, int64(s.MaxSize)+1)); err != nil {
		return nil, err
	}
	return &File{Name: attrs.Metadata["name"], ContentType: attrs.ContentType, Data: buf.Bytes()}, nil
}

// DeleteParent deletes every attachment of a parent.
func (s *Store) DeleteParent(ctx context.Context, purpose v1.AttachmentPurpose, parent string) error {
	it := s.bucket.List(&blob.ListOptions{Prefix: ParentPrefix(purpose, parent)})
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.bucket.Delete(ctx, obj.Key); err != nil {
			return err
		}
	}
}
