package uploads

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// fakeMilo knows two tokens: "ada" (user u1) and "staff". Ada may act in
// her own context; staff may get submissions in the platform context.
type fakeMilo struct{}

func (fakeMilo) Authenticate(_ context.Context, token string) (authenticationv1.UserInfo, bool, error) {
	switch token {
	case "ada":
		return authenticationv1.UserInfo{Username: "ada@example.com", Extra: map[string]authenticationv1.ExtraValue{
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
	case "staff@datum.net":
		return inUserContext == "" && attrs.Resource == "submissions" && attrs.Verb == "get", nil
	}
	return false, nil
}

func newServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	files, err := attachments.Open(context.Background(), "mem://")
	if err != nil {
		t.Fatal(err)
	}
	files.MaxSize = 1024
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	s := &Server{Files: files, Authn: fakeMilo{}, Authz: fakeMilo{}, Objects: fake.NewClientBuilder().WithScheme(scheme).Build()}
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

	get := func(path, token string) *httptest.ResponseRecorder { return do(h, http.MethodGet, path, token, "", nil) }
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
