package milo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/rest"
)

func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sel := r.URL.Query().Get("fieldSelector")
		switch r.URL.Path {
		case "/apis/resourcemanager.miloapis.com/v1alpha1/organizationmemberships":
			if sel != "spec.userRef.name=u1" {
				http.Error(w, "bad selector "+sel, http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"items":[{"spec":{"organizationRef":{"name":"zeta"}}},{"spec":{"organizationRef":{"name":"acme"}}},{"spec":{"organizationRef":{"name":"acme"}}}]}`)
		case "/apis/identity.miloapis.com/v1alpha1/useridentities":
			if sel != "status.userUID=u1" {
				_, _ = io.WriteString(w, `{"items":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"items":[{"status":{"providerName":"Email","username":"a@b.c"}},{"status":{"providerName":"GitHub","username":"octo"}}]}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `forbidden`)
		}
	}))
	defer srv.Close()
	c, err := NewClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	orgs, err := c.Organizations(ctx, "u1")
	if err != nil || len(orgs) != 2 || orgs[0] != "acme" || orgs[1] != "zeta" {
		t.Errorf("orgs %v %v", orgs, err)
	}
	if login, err := c.GitHubLogin(ctx, "u1"); login != "octo" || err != nil {
		t.Errorf("login %q %v", login, err)
	}
	if login, err := c.GitHubLogin(ctx, "u2"); login != "" || err != nil {
		t.Errorf("unlinked user: %q %v", login, err)
	}
	if _, err := c.Organizations(ctx, "u2"); err == nil {
		t.Error("error status: want error")
	}
}
