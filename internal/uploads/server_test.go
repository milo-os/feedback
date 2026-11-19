package uploads

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/attachments/attachmentstest"
	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// fakeMilo knows three tokens: "ada" (user u1), "grace" (user u2) and
// "staff". Ada and Grace may act in their own contexts; staff may get
// submissions in the platform context.
type fakeMilo struct{}

func (fakeMilo) Authenticate(_ context.Context, token string) (authenticationv1.UserInfo, bool, error) {
	switch token {
	case "ada":
		return authenticationv1.UserInfo{Username: "ada@example.com", Extra: map[string]authenticationv1.ExtraValue{
			identity.EmailVerifiedExtraKey: {"true"}}}, true, nil
	case "grace":
		return authenticationv1.UserInfo{Username: "grace@example.com", Extra: map[string]authenticationv1.ExtraValue{
			identity.EmailVerifiedExtraKey: {"true"}}}, true, nil
	case "staff":
		return authenticationv1.UserInfo{Username: "staff@datum.net"}, true, nil
	}
	return authenticationv1.UserInfo{}, false, nil
}

func (fakeMilo) Allowed(_ context.Context, u authenticationv1.UserInfo, attrs authorizationv1.ResourceAttributes, inUserContext string) (bool, error) {
	switch u.Username {
	case "ada@example.com":
		return inUserContext == "u1", nil
	case "grace@example.com":
		return inUserContext == "u2", nil
	case "staff@datum.net":
		return inUserContext == "" && attrs.Resource == "submissions" && attrs.Verb == "get", nil
	}
	return false, nil
}

func newServer(t *testing.T) (*Server, http.Handler) { return newServerAt(t, "mem://", 1024) }

func newServerAt(t *testing.T, bucketURL string, maxSize int) (*Server, http.Handler) {
	t.Helper()
	files, err := attachments.Open(context.Background(), bucketURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	files.MaxSize = maxSize
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	s := &Server{Files: files, Authn: fakeMilo{}, Authz: fakeMilo{}, Objects: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Metrics: NewMetrics(prometheus.NewRegistry())}
	return s, s.Handler()
}

func do(h http.Handler, method, path, token, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUploadAndDownload(t *testing.T) {
	s, h := newServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name, path, token, ct string
		body                  []byte
		want                  int
	}{
		{"no token", "/users/u1/uploads/submissions?name=a.txt", "", "text/plain", []byte("hi"), http.StatusUnauthorized},
		{"bad token", "/users/u1/uploads/submissions?name=a.txt", "nope", "text/plain", []byte("hi"), http.StatusUnauthorized},
		{"someone else's context", "/users/u2/uploads/submissions?name=a.txt", "ada", "text/plain", []byte("hi"), http.StatusNotFound},
		{"not a feedback resource", "/users/u1/uploads/votes?name=a.txt", "ada", "text/plain", []byte("hi"), http.StatusNotFound},
		{"html", "/users/u1/uploads/submissions?name=a.html", "ada", "text/html", []byte("<b>"), http.StatusBadRequest},
		{"too big", "/users/u1/uploads/submissions?name=a.txt", "ada", "text/plain", bytes.Repeat([]byte("x"), 2000), http.StatusRequestEntityTooLarge},
	} {
		if rec := do(h, http.MethodPost, tc.path, tc.token, tc.ct, tc.body); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body, tc.want)
		}
	}

	rec := do(h, http.MethodPost, "/users/u1/uploads/submissions?name=notes.txt", "ada", "text/plain; charset=utf-8", []byte("hello"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var up Uploaded
	if err := json.NewDecoder(rec.Body).Decode(&up); err != nil || up.Name != "notes.txt" || up.ContentType != "text/plain" {
		t.Fatalf("reply %+v, %v", up, err)
	}

	sub := &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: "sub-1"}, Spec: v1.SubmissionSpec{
		UserRef: v1.UserReference{Name: "u1"}, Attachments: []v1.AttachmentReference{{UploadRef: up.UploadRef}}}}
	theirs := &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: "sub-2"}, Spec: v1.SubmissionSpec{
		UserRef: v1.UserReference{Name: "u2"}, Attachments: []v1.AttachmentReference{{UploadRef: up.UploadRef}}}}
	for _, o := range []*v1.Submission{sub, theirs} {
		if err := s.Objects.(client.Client).Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	get := func(path, token string) *httptest.ResponseRecorder {
		return do(h, http.MethodGet, path, token, "", nil)
	}
	rec = get("/users/u1/submissions/sub-1/attachments/"+up.UploadRef, "ada")
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("own download: %d %s", rec.Code, rec.Body)
	}
	if h := rec.Header(); !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", h)
	}
	for _, tc := range []struct{ name, path, token string }{
		{"another user's submission in my context", "/users/u1/submissions/sub-2/attachments/" + up.UploadRef, "ada"},
		{"a ref the submission doesn't have", "/users/u1/submissions/sub-1/attachments/att-other", "ada"},
		{"platform context without staff access", "/submissions/sub-1/attachments/" + up.UploadRef, "ada"},
		{"missing submission", "/users/u1/submissions/sub-9/attachments/" + up.UploadRef, "ada"},
	} {
		if rec := get(tc.path, tc.token); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", tc.name, rec.Code)
		}
	}
	if rec := get("/submissions/sub-1/attachments/"+up.UploadRef, "staff"); rec.Code != http.StatusOK {
		b, _ := io.ReadAll(rec.Body)
		t.Errorf("staff download: %d %s", rec.Code, b)
	}
}

// attach records a submission by owner with refs attached.
func attach(t *testing.T, s *Server, name, owner string, refs ...string) {
	t.Helper()
	sub := &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: owner}}}
	for _, ref := range refs {
		sub.Spec.Attachments = append(sub.Spec.Attachments, v1.AttachmentReference{UploadRef: ref})
	}
	if err := s.Objects.(client.Client).Create(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
}

func uploadRef(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var up Uploaded
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if err := json.NewDecoder(rec.Body).Decode(&up); err != nil {
		t.Fatal(err)
	}
	return up.UploadRef
}

// TestStreamingDownload uploads and downloads a file several times larger
// than the copy buffers, over a real HTTP server with the service's
// timeouts, from each kind of bucket.
func TestStreamingDownload(t *testing.T) {
	for backend, bucketURL := range attachmentstest.Backends(t) {
		t.Run(backend, func(t *testing.T) {
			const maxSize = 4 << 20
			s, h := newServerAt(t, bucketURL, maxSize)
			srv := httptest.NewUnstartedServer(nil)
			srv.Config = NewHTTPServer("", h)
			srv.Start()
			t.Cleanup(srv.Close)

			data := make([]byte, maxSize)
			copy(data, "%PDF-1.7\n")
			_, _ = rand.Read(data[16:])
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/users/u1/uploads/submissions?name="+url.QueryEscape("my report \"final\".pdf"), bytes.NewReader(data))
			req.Header.Set("Authorization", "Bearer ada")
			req.Header.Set("Content-Type", "application/pdf")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var up Uploaded
			err = json.NewDecoder(res.Body).Decode(&up)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusCreated || err != nil {
				t.Fatalf("upload: %d, %v", res.StatusCode, err)
			}
			attach(t, s, "sub-1", "u1", up.UploadRef)

			req, _ = http.NewRequest(http.MethodGet, srv.URL+"/users/u1/submissions/sub-1/attachments/"+up.UploadRef, nil)
			req.Header.Set("Authorization", "Bearer ada")
			res, err = srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			got, err := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusOK || err != nil {
				t.Fatalf("download: %d, %v", res.StatusCode, err)
			}
			if !bytes.Equal(got, data) {
				t.Errorf("downloaded %d bytes that differ from the %d uploaded", len(got), len(data))
			}
			for k, want := range map[string]string{
				"Content-Type":           "application/pdf",
				"Content-Length":         strconv.Itoa(len(data)),
				"Content-Disposition":    `attachment; filename="my report \"final\".pdf"`,
				"X-Content-Type-Options": "nosniff",
				"Cache-Control":          "private, no-store",
			} {
				if got := res.Header.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if res.ContentLength != int64(len(data)) {
				t.Errorf("ContentLength = %d", res.ContentLength)
			}
		})
	}
}

// TestTransferOutlivesReadTimeout checks a transfer isn't cut short by the
// server's ReadTimeout, which cancels a request's context once its
// background read of the connection times out.
func TestTransferOutlivesReadTimeout(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := transfer(w, r, 1<<20)
		defer cancel()
		time.Sleep(300 * time.Millisecond)
		if err := ctx.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	res, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if b, err := io.ReadAll(res.Body); err != nil || string(b) != "ok" {
		t.Fatalf("%d %q, %v", res.StatusCode, b, err)
	}
}

func TestBusy(t *testing.T) {
	s, h := newServer(t)
	s.Uploads, s.Downloads = NewSlots(1), NewSlots(1)
	ref := uploadRef(t, do(h, http.MethodPost, "/users/u1/uploads/submissions?name=a.txt", "ada", "text/plain", []byte("hello")))
	attach(t, s, "sub-1", "u1", ref)
	download := func() *httptest.ResponseRecorder {
		return do(h, http.MethodGet, "/users/u1/submissions/sub-1/attachments/"+ref, "ada", "", nil)
	}
	upload := func() *httptest.ResponseRecorder {
		return do(h, http.MethodPost, "/users/u1/uploads/submissions?name=b.txt", "ada", "text/plain", []byte("again"))
	}

	// Fill every slot, as transfers in progress would.
	if !s.Uploads.take() || !s.Downloads.take() {
		t.Fatal("slots should be free")
	}
	for name, try := range map[string]func() *httptest.ResponseRecorder{"upload": upload, "download": download} {
		rec := try()
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
			t.Errorf("%s while busy: %d, Retry-After %q", name, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	// Busy replies come after the access checks, so they say nothing to
	// someone who may not see the file.
	if rec := do(h, http.MethodGet, "/users/u1/submissions/sub-1/attachments/"+ref, "nope", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token while busy: %d", rec.Code)
	}

	s.Uploads.give()
	s.Downloads.give()
	if rec := upload(); rec.Code != http.StatusCreated {
		t.Errorf("upload once free: %d %s", rec.Code, rec.Body)
	}
	if rec := download(); rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Errorf("download once free: %d %s", rec.Code, rec.Body)
	}
	// Finished transfers gave their slots back.
	if len(s.Uploads) != 0 || len(s.Downloads) != 0 {
		t.Errorf("slots in use after transfers finished: %d uploads, %d downloads", len(s.Uploads), len(s.Downloads))
	}
}

// TestOneUserCannotTakeEverySlot holds two of Ada's uploads open, as slow
// clients would, and checks she can't start a third while Grace still can.
func TestOneUserCannotTakeEverySlot(t *testing.T) {
	s, h := newServer(t)
	s.Uploads, s.UploadsPerUser = NewSlots(4), NewUserSlots(2)
	const ada = "ada@example.com"

	var wg sync.WaitGroup
	var bodies []*io.PipeWriter
	codes := make(chan int, 2)
	for range 2 {
		r, w := io.Pipe()
		bodies = append(bodies, w)
		req := httptest.NewRequest(http.MethodPost, "/users/u1/uploads/submissions?name=slow.txt", r)
		req.Header.Set("Authorization", "Bearer ada")
		req.Header.Set("Content-Type", "text/plain")
		wg.Go(func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes <- rec.Code
		})
	}
	defer func() {
		for _, w := range bodies {
			_ = w.Close()
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); s.UploadsPerUser.inUse(ada) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("slow uploads hold %d of Ada's slots, want 2", s.UploadsPerUser.inUse(ada))
		}
	}

	rec := do(h, http.MethodPost, "/users/u1/uploads/submissions?name=third.txt", "ada", "text/plain", []byte("third"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "5" {
		t.Errorf("Ada's third upload: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := do(h, http.MethodPost, "/users/u2/uploads/submissions?name=g.txt", "grace", "text/plain", []byte("hi")); rec.Code != http.StatusCreated {
		t.Errorf("Grace's upload while Ada's are in progress: %d %s", rec.Code, rec.Body)
	}
	if len(s.Uploads) != 2 {
		t.Errorf("replica slots in use: %d, want Ada's 2", len(s.Uploads))
	}

	for _, w := range bodies {
		_, _ = w.Write([]byte("slow"))
		_ = w.Close()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusCreated {
			t.Errorf("slow upload finished with %d", code)
		}
	}
	if len(s.Uploads) != 0 || s.UploadsPerUser.inUse(ada) != 0 {
		t.Errorf("slots in use after uploads finished: %d replica, %d Ada's", len(s.Uploads), s.UploadsPerUser.inUse(ada))
	}
	if rec := do(h, http.MethodPost, "/users/u1/uploads/submissions?name=again.txt", "ada", "text/plain", []byte("again")); rec.Code != http.StatusCreated {
		t.Errorf("Ada's upload once hers finished: %d %s", rec.Code, rec.Body)
	}
}

func TestUserDownloadCap(t *testing.T) {
	s, h := newServer(t)
	s.Downloads, s.DownloadsPerUser = NewSlots(4), NewUserSlots(1)
	ref := uploadRef(t, do(h, http.MethodPost, "/users/u1/uploads/submissions?name=a.txt", "ada", "text/plain", []byte("hello")))
	attach(t, s, "sub-1", "u1", ref)
	download := func() *httptest.ResponseRecorder {
		return do(h, http.MethodGet, "/users/u1/submissions/sub-1/attachments/"+ref, "ada", "", nil)
	}

	// As a download in progress would.
	if !s.DownloadsPerUser.take("ada@example.com") {
		t.Fatal("Ada's slot should be free")
	}
	if rec := download(); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "5" {
		t.Errorf("download at Ada's cap: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if len(s.Downloads) != 0 {
		t.Errorf("a refused download kept a replica slot")
	}
	s.DownloadsPerUser.give("ada@example.com")
	if rec := download(); rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Errorf("download once Ada's slot is free: %d %s", rec.Code, rec.Body)
	}
}

func TestUploadSizeLimit(t *testing.T) {
	_, h := newServer(t) // MaxSize 1024
	for _, tc := range []struct {
		name    string
		size    int
		chunked bool
		want    int
	}{
		{"at the limit", 1024, false, http.StatusCreated},
		{"over the limit", 1025, false, http.StatusRequestEntityTooLarge},
		{"at the limit, no Content-Length", 1024, true, http.StatusCreated},
		{"over the limit, no Content-Length", 1025, true, http.StatusRequestEntityTooLarge},
		{"far over the limit, no Content-Length", 1 << 20, true, http.StatusRequestEntityTooLarge},
	} {
		var body io.Reader = bytes.NewReader(bytes.Repeat([]byte("x"), tc.size))
		if tc.chunked {
			body = io.MultiReader(body) // hides the length
		}
		req := httptest.NewRequest(http.MethodPost, "/users/u1/uploads/submissions?name=a.txt", body)
		if tc.chunked {
			req.ContentLength = -1
		}
		req.Header.Set("Authorization", "Bearer ada")
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
}
