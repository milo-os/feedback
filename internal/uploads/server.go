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
//
// Memory and time are bounded per replica. Downloads stream from the bucket
// and never hold a file. Uploads buffer at most one file each, and Slots cap
// how many uploads and downloads run at once; a full replica answers 503 with
// Retry-After rather than queueing. UserSlots cap each user's share of them,
// so one user can't take every slot; a user at their cap gets 429. NewHTTPServer's timeouts bound every
// request without buffering replies, and a transfer gets a deadline sized for
// its file (TransferTimeout).
package uploads

import (
	"bytes"
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
	"sync"
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
	// Uploads and Downloads cap concurrent transfers on this replica. Nil
	// doesn't cap.
	Uploads, Downloads Slots
	// UploadsPerUser and DownloadsPerUser cap one user's concurrent
	// transfers on this replica, below Uploads and Downloads, so slow
	// requests from one user can't keep everyone else busy. Nil doesn't cap.
	UploadsPerUser, DownloadsPerUser *UserSlots
	// Metrics records requests, refusals and bucket errors. Nil doesn't
	// record.
	Metrics *Metrics
}

// Default transfer caps, for the 256Mi container limit in
// config/base/uploads and the default 10 MiB attachment-max-size.
//
// Memory budget per replica:
//
//   - An upload holds its body, up to MaxSize (10 MiB). When the client
//     doesn't send Content-Length, growing the buffer can briefly need twice
//     that. The bucket client then buffers up to about 16 MiB more while it
//     sends the file (GCS uploads in 16 MiB chunks, S3 in 8 MiB parts). Call
//     it 36 MiB at worst, so 4 uploads need about 144 MiB.
//   - A download streams through a 32 KiB copy buffer plus the bucket
//     client's and the connection's buffers, well under 1 MiB, so 32
//     downloads need a few MiB.
//   - The process itself (runtime, Milo and bucket clients) needs about 40
//     MiB.
//
// That totals about 190 MiB, leaving headroom under 256Mi; GOMEMLIMIT in the
// Deployment makes the GC work harder before the limit rather than letting
// garbage push the process over it. Raising --attachment-max-size means
// lowering --max-concurrent-uploads to match.
//
// One user may hold half the uploads and a quarter of the downloads, so
// others always have slots left.
const (
	DefaultMaxUploads          = 4
	DefaultMaxDownloads        = 32
	DefaultMaxUploadsPerUser   = 2
	DefaultMaxDownloadsPerUser = 8
)

// Slots is a semaphore that caps concurrent transfers. A nil Slots has no
// cap.
type Slots chan struct{}

// NewSlots returns n slots, or nil, for no cap, if n <= 0.
func NewSlots(n int) Slots {
	if n <= 0 {
		return nil
	}
	return make(Slots, n)
}

// take takes a slot if one is free. It never waits: callers answer 503
// instead, so a busy replica can't build an unbounded queue of waiting
// requests and their connections.
func (s Slots) take() bool {
	if s == nil {
		return true
	}
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s Slots) give() {
	if s != nil {
		<-s
	}
}

// UserSlots caps how many transfers each user runs at once. A nil UserSlots
// has no cap.
type UserSlots struct {
	max  int
	mu   sync.Mutex
	held map[string]int
}

// NewUserSlots returns a cap of n transfers per user, or nil, for no cap, if
// n <= 0.
func NewUserSlots(n int) *UserSlots {
	if n <= 0 {
		return nil
	}
	return &UserSlots{max: n, held: map[string]int{}}
}

// take takes one of user's slots if they have one free. Like Slots.take, it
// never waits.
func (u *UserSlots) take(user string) bool {
	if u == nil {
		return true
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.held[user] >= u.max {
		return false
	}
	u.held[user]++
	return true
}

func (u *UserSlots) give(user string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.held[user]--; u.held[user] <= 0 {
		delete(u.held, user)
	}
}

// inUse reports how many of user's slots are taken.
func (u *UserSlots) inUse(user string) int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.held[user]
}

// claim takes a transfer slot on route for user: one of theirs, then one of
// the replica's. If either is full it answers the request, 429 for the
// user's cap and 503 for the replica's, and returns ok false. Otherwise the
// caller must call release when the transfer ends.
func (s *Server) claim(w http.ResponseWriter, route string, perUser *UserSlots, replica Slots, user string) (release func(), ok bool) {
	if !perUser.take(user) {
		s.Metrics.refuse(route, reasonUserBusy)
		w.Header().Set("Retry-After", strconv.Itoa(int(RetryAfter.Seconds())))
		fail(w, http.StatusTooManyRequests, "you have too many transfers in progress; try again when one finishes")
		return nil, false
	}
	if !replica.take() {
		perUser.give(user)
		s.Metrics.refuse(route, reasonReplicaBusy)
		busy(w)
		return nil, false
	}
	done := s.Metrics.transferring(route)
	return func() {
		done()
		replica.give()
		perUser.give(user)
	}, true
}

// RequestTimeout bounds each request apart from moving a file: reading it,
// asking Milo, and writing a short reply. It is the server's ReadTimeout and
// WriteTimeout.
const RequestTimeout = 30 * time.Second

// MinTransferRate is the slowest average rate, in bytes per second, at which
// a client may send or receive a file; slower, and the deadline cuts it off.
const MinTransferRate = 64 << 10

// RetryAfter is how long a client is told to wait when every slot, or every
// one of their own, is taken.
const RetryAfter = 5 * time.Second

// TransferTimeout is how long moving a file of size bytes may take:
// RequestTimeout, plus size at MinTransferRate. A 10 MiB file gets 3m10s.
func TransferTimeout(size int64) time.Duration {
	return RequestTimeout + time.Duration(size)*time.Second/MinTransferRate
}

// NewHTTPServer returns an HTTP server for h with timeouts that, unlike
// http.TimeoutHandler, don't buffer replies: they are deadlines on the
// connection. Transfers extend them for their own request (see transfer).
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       RequestTimeout,
		WriteTimeout:      RequestTimeout,
		IdleTimeout:       2 * time.Minute,
	}
}

// transfer moves this request's read and write deadlines to the time a file
// of size bytes may take, and returns a context that ends then too, so a
// stalled client or bucket can't hold a slot past it.
//
// The read deadline matters for downloads as well as uploads: once a request
// has no body left, net/http reads the connection in the background to notice
// the client leaving, and if that read hits the server's ReadTimeout it
// cancels the request's context, which would cut a long download short.
func transfer(w http.ResponseWriter, r *http.Request, size int64) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(TransferTimeout(size))
	rc := http.NewResponseController(w)
	for _, set := range []func(time.Time) error{rc.SetReadDeadline, rc.SetWriteDeadline} {
		// Only test recorders don't support deadlines.
		if err := set(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.FromContext(r.Context()).Error(err, "Setting a transfer deadline failed")
		}
	}
	return context.WithDeadline(r.Context(), deadline)
}

// busy answers a request that found every slot taken.
func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(int(RetryAfter.Seconds())))
	fail(w, http.StatusServiceUnavailable, "the service is busy; try again shortly")
}

// Handler returns the service's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	upload := s.Metrics.instrument(routeUpload, s.upload)
	download := s.Metrics.instrument(routeDownload, s.download)
	mux.HandleFunc("POST /users/{user}/uploads/{resource}", upload)
	mux.HandleFunc("GET /users/{user}/{resource}/{name}/attachments/{ref}", download)
	mux.HandleFunc("GET /{resource}/{name}/attachments/{ref}", download)
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
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		fail(w, http.StatusBadRequest, "set Content-Type to the file's media type")
		return
	}
	maxSize := int64(s.Files.MaxSize)
	tooBig := fmt.Sprintf("the file is larger than %d bytes", maxSize)
	if r.ContentLength > maxSize {
		fail(w, http.StatusRequestEntityTooLarge, tooBig)
		return
	}
	release, ok := s.claim(w, routeUpload, s.UploadsPerUser, s.Uploads, caller.Username)
	if !ok {
		return
	}
	defer release()
	// After taking a slot, so a busy replica doesn't spend the user's quota.
	if ok, wait := s.Limiter.Allow(user); !ok {
		s.Metrics.refuse(routeUpload, reasonRateLimited)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		fail(w, http.StatusTooManyRequests, "too many uploads; try again shortly")
		return
	}
	size := maxSize
	if r.ContentLength >= 0 {
		size = r.ContentLength
	}
	ctx, cancel := transfer(w, r, size)
	defer cancel()
	// The upload is buffered, up to MaxSize, because Check needs all of it to
	// verify its type and the stored SHA-256 covers it. MaxBytesReader stops
	// reading past MaxSize, and the upload slot bounds how many buffers exist
	// at once. With a Content-Length the buffer is allocated once.
	var body bytes.Buffer
	if r.ContentLength >= 0 {
		body.Grow(int(r.ContentLength) + bytes.MinRead)
	}
	_, err = body.ReadFrom(http.MaxBytesReader(w, r.Body, maxSize))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		fail(w, http.StatusRequestEntityTooLarge, tooBig)
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "reading the file failed")
		return
	}
	data := body.Bytes()
	name := r.URL.Query().Get("name")
	if msg := s.Files.Check(name, contentType, data); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	ref, sum, err := s.Files.Upload(ctx, purpose, user, name, contentType, data)
	if err != nil {
		s.Metrics.bucketError(opUpload)
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
	release, ok := s.claim(w, routeDownload, s.DownloadsPerUser, s.Downloads, caller.Username)
	if !ok {
		return
	}
	defer release()
	f, err := s.Files.Get(ctx, purpose, name, owner, ref)
	if err != nil {
		s.unreadable(ctx, w, opGet, err)
		return
	}
	ctx, cancel := transfer(w, r, f.Size)
	defer cancel()
	body, err := f.Open(ctx)
	if err != nil {
		s.unreadable(ctx, w, opOpen, err)
		return
	}
	defer func() { _ = body.Close() }()
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	if _, err := io.Copy(w, body); err != nil {
		// The status has been sent, so all that's left is to stop short of
		// Content-Length, which tells the client the file is incomplete.
		log.FromContext(ctx).V(1).Info("Sending an attachment failed", "error", err.Error())
	}
}

// unreadable answers a download whose file couldn't be read by op.
func (s *Server) unreadable(ctx context.Context, w http.ResponseWriter, op string, err error) {
	if errors.Is(err, attachments.ErrNotFound) {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	s.Metrics.bucketError(op)
	log.FromContext(ctx).Error(err, "Reading an attachment failed")
	fail(w, http.StatusServiceUnavailable, "try again")
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
