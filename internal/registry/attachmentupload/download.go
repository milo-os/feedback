package attachmentupload

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/attachments"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// DownloadREST serves GET {resource}/{name}/attachment?ref=: one attached
// file, to whoever can read the parent.
type DownloadREST struct {
	// Get reads the parent as the caller and returns its attachments. It
	// must apply the parent's own access rules (scope, redaction).
	Get     func(ctx context.Context, name string) ([]v1.AttachmentReference, error)
	NewFunc func() runtime.Object
	Files   *attachments.Store
	Purpose v1.AttachmentPurpose
	// Resource names the subresource in NotFound errors.
	Resource schema.GroupResource
}

var _ rest.Connecter = &DownloadREST{}

func (r *DownloadREST) New() runtime.Object { return r.NewFunc() }
func (r *DownloadREST) Destroy()            {}

func (r *DownloadREST) NewConnectOptions() (runtime.Object, bool, string) {
	return &v1.AttachmentOptions{}, false, ""
}

func (r *DownloadREST) ConnectMethods() []string { return []string{http.MethodGet} }

// Connect checks the caller can read the parent and that ref is attached to
// it, then returns a handler that sends the file.
func (r *DownloadREST) Connect(ctx context.Context, name string, opts runtime.Object, _ rest.Responder) (http.Handler, error) {
	ref := opts.(*v1.AttachmentOptions).Ref
	if ref == "" {
		return nil, apierrors.NewBadRequest("set the ref query parameter to an attachment's uploadRef")
	}
	refs, err := r.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(refs, func(a v1.AttachmentReference) bool { return a.UploadRef == ref }) {
		return nil, apierrors.NewNotFound(r.Resource, ref)
	}
	if r.Files == nil {
		return nil, apierrors.NewServiceUnavailable("Attachments are not available.")
	}
	f, err := r.Files.Get(ctx, r.Purpose, name, ref)
	if errors.Is(err, attachments.ErrNotFound) {
		return nil, apierrors.NewNotFound(r.Resource, ref)
	}
	if err != nil {
		klog.ErrorS(err, "Reading attachment failed", "resource", r.Resource, "name", name, "ref", ref)
		return nil, apierrors.NewServiceUnavailable("Attachments are not available.")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveFile(w, f) }), nil
}

// serveFile sends a file as a download that browsers won't render or run.
func serveFile(w http.ResponseWriter, f *attachments.File) {
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(f.Data)))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(f.Data)
}
