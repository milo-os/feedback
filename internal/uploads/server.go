// Package uploads is the feedback-uploads HTTP service. CRDs can't take file
// uploads, so clients send files here, with the same bearer token they use
// for Milo, and reference them from submissions and security reports.
//
// Milo decides who may do what: a TokenReview says who the caller is, and a
// SubjectAccessReview says whether they may create (to upload) or get (to
// download) the resource the file belongs to, in the context they name. For
// downloads in a user's own context the service also checks the object is
// theirs, which Milo's owner filter does for API requests but an access
// review can't.
//
// Routes:
//
//	POST /users/{user}/uploads/{resource}?name={file name}
//	GET  /users/{user}/{resource}/{name}/attachments/{ref}
//	GET  /{resource}/{name}/attachments/{ref}
//
// {resource} is submissions or securityreports. Uploads send the file as the
// body with its Content-Type. The reply is the uploadRef to put in
// spec.attachments.
package uploads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/ratelimit"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// Authenticator says who a bearer token belongs to.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (authenticationv1.UserInfo, bool, error)
}

// Authorizer asks Milo whether user may do attrs. A non-empty inUserContext
// asks as if the request were made in that user's own context.
type Authorizer interface {
	Allowed(ctx context.Context, user authenticationv1.UserInfo, attrs authorizationv1.ResourceAttributes, inUserContext string) (bool, error)
}

// Server serves uploads and downloads.
type Server struct {
	Files *attachments.Store
	Authn Authenticator
	Authz Authorizer
	// Objects reads submissions and security reports from Milo.
	Objects client.Reader
	// Limiter limits uploads per user. Nil doesn't limit.
	Limiter *ratelimit.Limiter
}

// Handler returns the service's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users/{user}/uploads/{resource}", s.upload)
	mux.HandleFunc("GET /users/{user}/{resource}/{name}/attachments/{ref}", s.download)
	mux.HandleFunc("GET /{resource}/{name}/attachments/{ref}", s.download)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	return mux
}

// Uploaded is the reply to an upload.
type Uploaded struct {
	UploadRef   string `json:"uploadRef"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	SHA256      string `json:"sha256"`
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resource, user := r.PathValue("resource"), r.PathValue("user")
	purpose, ok := attachments.PurposeOf(resource)
	if !ok {
		fail(w, http.StatusNotFound, "uploads are for submissions or securityreports")
		return
	}
	caller, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if !s.authorize(w, r, caller, authorizationv1.ResourceAttributes{Group: v1.GroupName, Resource: resource, Verb: "create"}, user) {
		return
	}
	if identity.EmailUnverified(caller) {
		fail(w, http.StatusForbidden, "verify your email address to send files")
		return
	}
	if ok, wait := s.Limiter.Allow(user); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		fail(w, http.StatusTooManyRequests, "too many uploads; try again shortly")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		fail(w, http.StatusBadRequest, "set Content-Type to the file's media type")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(s.Files.MaxSize)+1))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the file is larger than %d bytes", s.Files.MaxSize))
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "reading the file failed")
		return
	}
	name := r.URL.Query().Get("name")
	if msg := s.Files.Check(name, contentType, data); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	ref, sum, err := s.Files.Upload(ctx, purpose, user, name, contentType, data)
	if err != nil {
		log.FromContext(ctx).Error(err, "Storing an upload failed")
		fail(w, http.StatusServiceUnavailable, "storing the file failed; try again")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(Uploaded{UploadRef: ref, Name: name, ContentType: contentType, SHA256: sum})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resource, name, ref, user := r.PathValue("resource"), r.PathValue("name"), r.PathValue("ref"), r.PathValue("user")
	purpose, ok := attachments.PurposeOf(resource)
	if !ok {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	caller, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if !s.authorize(w, r, caller, authorizationv1.ResourceAttributes{Group: v1.GroupName, Resource: resource, Verb: "get", Name: name}, user) {
		return
	}
	owner, refs, err := s.attachmentsOf(ctx, resource, name)
	switch {
	case apierrors.IsNotFound(err):
		fail(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		log.FromContext(ctx).Error(err, "Reading the attachment's parent failed")
		fail(w, http.StatusServiceUnavailable, "try again")
		return
	}
	// In a user's own context the object must be theirs; staff ask in the
	// platform context.
	if (user != "" && owner != user) || !slices.Contains(refs, ref) {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	f, err := s.Files.Get(ctx, purpose, name, owner, ref)
	if errors.Is(err, attachments.ErrNotFound) {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "Reading an attachment failed")
		fail(w, http.StatusServiceUnavailable, "try again")
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(f.Data)
}

// attachmentsOf returns the owner and attachment refs of a submission or
// report.
func (s *Server) attachmentsOf(ctx context.Context, resource, name string) (string, []string, error) {
	key := types.NamespacedName{Name: name}
	var owner string
	var atts []v1.AttachmentReference
	switch resource {
	case "submissions":
		var o v1.Submission
		if err := s.Objects.Get(ctx, key, &o); err != nil {
			return "", nil, err
		}
		owner, atts = o.Spec.UserRef.Name, o.Spec.Attachments
	default:
		var o v1.SecurityReport
		if err := s.Objects.Get(ctx, key, &o); err != nil {
			return "", nil, err
		}
		if o.Spec.UserRef != nil {
			owner = o.Spec.UserRef.Name
		}
		atts = o.Spec.Attachments
	}
	refs := make([]string, len(atts))
	for i, a := range atts {
		refs[i] = a.UploadRef
	}
	return owner, refs, nil
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (authenticationv1.UserInfo, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		fail(w, http.StatusUnauthorized, "send your Milo token as a bearer token")
		return authenticationv1.UserInfo{}, false
	}
	u, ok, err := s.Authn.Authenticate(r.Context(), token)
	if err != nil {
		log.FromContext(r.Context()).Error(err, "TokenReview failed")
		fail(w, http.StatusServiceUnavailable, "try again")
		return u, false
	}
	if !ok {
		fail(w, http.StatusUnauthorized, "the token isn't valid")
		return u, false
	}
	return u, true
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, u authenticationv1.UserInfo, attrs authorizationv1.ResourceAttributes, inUserContext string) bool {
	ok, err := s.Authz.Allowed(r.Context(), u, attrs, inUserContext)
	if err != nil {
		log.FromContext(r.Context()).Error(err, "SubjectAccessReview failed")
		fail(w, http.StatusServiceUnavailable, "try again")
		return false
	}
	if !ok {
		// Don't say whether the object exists.
		fail(w, http.StatusNotFound, "not found")
	}
	return ok
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// RequestTimeout bounds one request.
const RequestTimeout = 30 * time.Second
