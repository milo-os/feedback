package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		testKey = k
	})
	return testKey
}

func pemKey(t *testing.T, pkcs8 bool) []byte {
	k := key(t)
	if pkcs8 {
		b, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// fakeGitHub serves the token exchange and hands GraphQL requests to graphql.
type fakeGitHub struct {
	t         *testing.T
	mu        sync.Mutex
	exchanges int
	tokens    map[string]bool // valid tokens
	graphql   func(w http.ResponseWriter, query string, vars map[string]any)
	requests  int
}

func newFake(t *testing.T) (*fakeGitHub, *App) {
	f := &fakeGitHub{t: t, tokens: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	app, err := NewApp(42, pemKey(t, false), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return f, app
}

// checkJWT verifies the app JWT against the test key.
func (f *fakeGitHub) checkJWT(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if !ok || len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	return claims.Iss == "42" && claims.Exp-claims.Iat <= 600
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
		if !f.checkJWT(r) {
			http.Error(w, "bad JWT", http.StatusUnauthorized)
			return
		}
		f.exchanges++
		tok := fmt.Sprintf("tok-%d", f.exchanges)
		f.tokens[tok] = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour)})
	case r.Method == http.MethodGet && r.URL.Path == "/orgs/datum-cloud/installation":
		if !f.checkJWT(r) {
			http.Error(w, "bad JWT", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		if !f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		f.requests++
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(5000-f.requests))
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		f.graphql(w, body.Query, body.Variables)
	default:
		http.NotFound(w, r)
	}
}

func respond(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func TestNewAppKeys(t *testing.T) {
	if _, err := NewApp(42, pemKey(t, true)); err != nil {
		t.Errorf("PKCS #8 key: %v", err)
	}
	if _, err := NewApp(42, []byte("not a key")); err == nil {
		t.Error("garbage key: want an error")
	}
	if _, err := NewApp(0, pemKey(t, false)); err == nil {
		t.Error("no app ID: want an error")
	}
}

func TestTokenIsCached(t *testing.T) {
	f, app := newFake(t)
	now := time.Now()
	app.now = func() time.Time { return now }
	ctx := context.Background()
	for range 3 {
		if tok, err := app.Token(ctx, 7); err != nil || tok != "tok-1" {
			t.Fatalf("Token() = %q, %v", tok, err)
		}
	}
	if f.exchanges != 1 {
		t.Errorf("exchanges = %d, want 1", f.exchanges)
	}
	// Within tokenSlack of expiry, a new token is fetched.
	now = now.Add(time.Hour - tokenSlack + time.Second)
	if tok, err := app.Token(ctx, 7); err != nil || tok != "tok-2" {
		t.Fatalf("Token() near expiry = %q, %v", tok, err)
	}
	if _, err := app.Token(ctx, 8); err == nil {
		t.Error("unknown installation: want an error")
	}
}

func TestInstallationID(t *testing.T) {
	_, app := newFake(t)
	id, err := app.InstallationID(context.Background(), "Datum-Cloud")
	if err == nil {
		t.Fatalf("the fake only knows datum-cloud by its exact login; got %d", id)
	}
	if id, err = app.InstallationID(context.Background(), "datum-cloud"); err != nil || id != 7 {
		t.Fatalf("InstallationID() = %d, %v", id, err)
	}
	// Cached, ignoring case.
	if id, err = app.InstallationID(context.Background(), "Datum-Cloud"); err != nil || id != 7 {
		t.Fatalf("InstallationID() cached = %d, %v", id, err)
	}
	// Looked up again after an hour, in case the app was reinstalled.
	now := time.Now().Add(installationTTL + time.Minute)
	app.now = func() time.Time { return now }
	if _, err = app.InstallationID(context.Background(), "Datum-Cloud"); err == nil {
		t.Fatal("expected a fresh lookup once the cached ID expired")
	}
}

// TestProjectItemsAnotherOrgCantRead: GitHub refuses a project the
// installation can't read but still returns the issue.
func TestProjectItemsAnotherOrgCantRead(t *testing.T) {
	f, app := newFake(t)
	created := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	forbidden := []any{map[string]any{
		"type": "FORBIDDEN", "message": "Resource not accessible by integration",
		"path": []any{"repository", "issue", "projectItems", "nodes", 0, "project"},
	}}
	issue := func() map[string]any {
		is := issueJSON(9, created)
		is["projectItems"] = map[string]any{"nodes": []any{map[string]any{"project": nil, "status": nil}}}
		return is
	}
	f.graphql = func(w http.ResponseWriter, q string, _ map[string]any) {
		if strings.Contains(q, "search(") {
			respond(w, map[string]any{"data": map[string]any{"search": map[string]any{
				"issueCount": 1, "pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{issue()},
			}}, "errors": forbidden})
			return
		}
		respond(w, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": issue()}}, "errors": forbidden})
	}
	is, err := app.Client(7).Issue(context.Background(), "milo-os", "x", 9, "")
	if err != nil || is.Number != 9 || len(is.ProjectItems) != 0 {
		t.Fatalf("Issue() = %+v, %v; want the issue without the unreadable item", is, err)
	}
	found, err := app.Client(7).SearchIssues(context.Background(), "org:milo-os", "", time.Now())
	if err != nil || len(found) != 1 {
		t.Fatalf("SearchIssues() = %d issues, %v", len(found), err)
	}
}

// TestForgetTokenKeepsANewerOne: a request that got a 401 on an old token
// doesn't drop a token another request has since stored.
func TestForgetTokenKeepsANewerOne(t *testing.T) {
	_, app := newFake(t)
	ctx := context.Background()
	old, err := app.Token(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	app.forgetToken(7, old)
	newer, err := app.Token(ctx, 7)
	if err != nil || newer == old {
		t.Fatalf("Token() after forgetting = %q, %v", newer, err)
	}
	app.forgetToken(7, old)
	if got, _ := app.Token(ctx, 7); got != newer {
		t.Errorf("forgetting a stale token dropped the newer one: got %q, want %q", got, newer)
	}
}

func issueJSON(n int, created time.Time) map[string]any {
	return map[string]any{
		"id": fmt.Sprintf("I_%d", n), "number": n, "url": fmt.Sprintf("https://github.com/datum-cloud/enhancements/issues/%d", n),
		"title": "T", "bodyHTML": "<p>b</p>", "bodyText": "b", "state": "CLOSED", "stateReason": "NOT_PLANNED",
		"createdAt": created.Format(time.RFC3339), "updatedAt": created.Format(time.RFC3339),
		"issueType":  map[string]any{"name": "Enhancement"},
		"repository": map[string]any{"name": "enhancements", "visibility": "PUBLIC", "owner": map[string]any{"login": "datum-cloud"}},
		"labels":     map[string]any{"nodes": []any{map[string]any{"name": "Build"}, map[string]any{"name": "feedback:hide"}}},
		"projectItems": map[string]any{"nodes": []any{
			map[string]any{"project": map[string]any{"number": 22, "owner": map[string]any{"login": "datum-cloud"}}, "status": map[string]any{"name": "Done"}},
			map[string]any{"project": map[string]any{"number": 60, "owner": map[string]any{"login": "datum-cloud"}}, "status": nil},
		}},
	}
}

func TestIssue(t *testing.T) {
	f, app := newFake(t)
	c := app.Client(7)
	created := time.Date(2026, 9, 30, 14, 12, 0, 0, time.UTC)
	f.graphql = func(w http.ResponseWriter, q string, vars map[string]any) {
		if vars["statusField"] != "Status" {
			t.Errorf("statusField = %v, want the default", vars["statusField"])
		}
		switch vars["number"] {
		case float64(912):
			respond(w, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": issueJSON(912, created)}}})
		case float64(1):
			respond(w, map[string]any{
				"data":   map[string]any{"repository": map[string]any{"issue": nil}},
				"errors": []any{map[string]any{"type": "NOT_FOUND", "path": []any{"repository", "issue"}, "message": "Could not resolve to an Issue"}},
			})
		case float64(2):
			respond(w, map[string]any{
				"data":   map[string]any{"repository": nil},
				"errors": []any{map[string]any{"type": "NOT_FOUND", "path": []any{"repository"}, "message": "Could not resolve to a Repository"}},
			})
		default:
			respond(w, map[string]any{"data": nil, "errors": []any{map[string]any{"type": "FORBIDDEN", "message": "nope"}}})
		}
	}
	ctx := context.Background()
	is, err := c.Issue(ctx, "datum-cloud", "enhancements", 912, "")
	if err != nil {
		t.Fatal(err)
	}
	if is.ID != "I_912" || is.Org != "datum-cloud" || is.Repo != "enhancements" || is.Number != 912 ||
		is.State != "closed" || is.StateReason != "not_planned" || is.Visibility != "public" ||
		is.IssueType != "Enhancement" || !is.CreatedAt.Equal(created) || len(is.Labels) != 2 {
		t.Errorf("Issue() = %+v", is)
	}
	if len(is.ProjectItems) != 2 || is.ProjectItems[0] != (ProjectItem{Owner: "datum-cloud", Number: 22, Status: "Done"}) ||
		is.ProjectItems[1] != (ProjectItem{Owner: "datum-cloud", Number: 60}) {
		t.Errorf("ProjectItems = %+v", is.ProjectItems)
	}
	for _, n := range []int32{1, 2} {
		if _, err := c.Issue(ctx, "datum-cloud", "enhancements", n, "Status"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Issue(%d) error = %v, want ErrNotFound", n, err)
		}
	}
	if _, err := c.Issue(ctx, "datum-cloud", "enhancements", 3, "Status"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Issue(3) error = %v, want a non-NotFound error", err)
	}
	rl := c.RateLimit()
	if rl == nil || rl.Limit != 5000 || rl.Remaining != 4996 || !rl.ResetAt.Equal(time.Unix(1800000000, 0)) {
		t.Errorf("RateLimit() = %+v", rl)
	}
	if app.Client(7) != c {
		t.Error("Client() should return the same client for an installation")
	}
}

func TestRetriesWithANewTokenOn401(t *testing.T) {
	f, app := newFake(t)
	f.graphql = func(w http.ResponseWriter, _ string, _ map[string]any) {
		respond(w, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": issueJSON(1, time.Now())}}})
	}
	ctx := context.Background()
	if _, err := app.Client(7).Issue(ctx, "datum-cloud", "enhancements", 1, ""); err != nil {
		t.Fatal(err)
	}
	// GitHub revokes the token.
	f.mu.Lock()
	f.tokens = map[string]bool{}
	f.mu.Unlock()
	if _, err := app.Client(7).Issue(ctx, "datum-cloud", "enhancements", 1, ""); err != nil {
		t.Fatalf("after revocation: %v", err)
	}
	if f.exchanges != 2 {
		t.Errorf("exchanges = %d, want 2", f.exchanges)
	}
}

var createdRange = regexp.MustCompile(`created:(\S+)\.\.(\S+)`)

// TestSearchIssues pages through results and splits a query GitHub would cut
// off at 1000 results by creation date.
func TestSearchIssues(t *testing.T) {
	f, app := newFake(t)
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 2345
	created := make([]time.Time, total)
	for i := range created {
		created[i] = start.Add(time.Duration(i) * 7 * time.Hour)
	}
	var queries []string
	f.graphql = func(w http.ResponseWriter, _ string, vars map[string]any) {
		q := vars["q"].(string)
		queries = append(queries, q)
		if !strings.HasPrefix(q, "org:datum-cloud is:issue") {
			t.Errorf("query = %q", q)
		}
		from, to := time.Time{}, time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
		if m := createdRange.FindStringSubmatch(q); m != nil {
			from, _ = time.Parse(time.RFC3339, m[1])
			to, _ = time.Parse(time.RFC3339, m[2])
		}
		var hits []int
		for i, c := range created {
			if !c.Before(from) && !c.After(to) {
				hits = append(hits, i)
			}
		}
		offset := 0
		if a, ok := vars["after"].(string); ok {
			offset, _ = strconv.Atoi(a)
		}
		first := int(vars["first"].(float64))
		nodes := []any{}
		if len(hits) <= 1000 {
			for _, i := range hits[offset:min(offset+first, len(hits))] {
				nodes = append(nodes, issueJSON(i+1, created[i]))
			}
			// Search can return a pull request (empty object) or a node the
			// installation can't see (null).
			if offset == 0 {
				nodes = append(nodes, map[string]any{}, nil)
			}
		}
		end := offset + first
		respond(w, map[string]any{"data": map[string]any{"search": map[string]any{
			"issueCount": len(hits),
			"pageInfo":   map[string]any{"hasNextPage": end < len(hits), "endCursor": strconv.Itoa(end)},
			"nodes":      nodes,
		}}})
	}
	issues, err := app.Client(7).SearchIssues(context.Background(), "org:datum-cloud is:issue", "Status", start.Add(total*7*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, is := range issues {
		seen[is.ID] = true
	}
	if len(seen) != total {
		t.Errorf("found %d distinct issues, want %d", len(seen), total)
	}
	if len(issues) != total {
		t.Errorf("SearchIssues() returned %d issues, want each once: %d", len(issues), total)
	}
	if !strings.Contains(strings.Join(queries, "\n"), "created:") {
		t.Error("expected the query to be split by creation date")
	}
	for _, q := range queries {
		if !strings.Contains(q, "sort:created-asc") {
			t.Errorf("query %q isn't sorted by creation", q)
		}
	}

	// A query under the limit isn't split.
	queries = nil
	created = created[:120]
	issues, err = app.Client(7).SearchIssues(context.Background(), "org:datum-cloud is:issue", "Status", time.Now())
	if err != nil || len(issues) != 120 {
		t.Fatalf("SearchIssues() = %d issues, %v", len(issues), err)
	}
	if len(queries) != 3 || strings.Contains(queries[0], "created:") {
		t.Errorf("queries = %q, want three unsplit pages", queries)
	}
}

func TestProjectItems(t *testing.T) {
	f, app := newFake(t)
	var batches []int
	f.graphql = func(w http.ResponseWriter, _ string, vars map[string]any) {
		ids := vars["ids"].([]any)
		batches = append(batches, len(ids))
		nodes := []any{}
		for _, id := range ids {
			if id == "gone" {
				nodes = append(nodes, nil)
				continue
			}
			nodes = append(nodes, map[string]any{"id": id, "projectItems": map[string]any{"nodes": []any{
				map[string]any{"project": map[string]any{"number": 22, "owner": map[string]any{"login": "datum-cloud"}}, "status": map[string]any{"name": "Planning"}},
			}}})
		}
		resp := map[string]any{"data": map[string]any{"nodes": nodes}}
		if slicesContains(ids, "gone") {
			resp["errors"] = []any{map[string]any{"type": "NOT_FOUND", "message": "Could not resolve to a node with the global id of 'gone'"}}
		}
		respond(w, resp)
	}
	ids := []string{"gone"}
	for i := range 150 {
		ids = append(ids, fmt.Sprintf("I_%d", i))
	}
	got, err := app.Client(7).ProjectItems(context.Background(), ids, "Status")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 || got["I_3"][0].Status != "Planning" {
		t.Errorf("ProjectItems() = %d issues, I_3 = %+v", len(got), got["I_3"])
	}
	if len(batches) != 2 || batches[0] != 100 || batches[1] != 51 {
		t.Errorf("batches = %v, want [100 51]", batches)
	}
}

func slicesContains(s []any, v any) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
