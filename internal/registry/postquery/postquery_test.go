package postquery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
	restclient "k8s.io/client-go/rest"

	"go.miloapis.com/feedback/internal/embedding"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/ratelimit"
	"go.miloapis.com/feedback/internal/registry/post"
	"go.miloapis.com/feedback/internal/storage/pg"
	"go.miloapis.com/feedback/internal/storage/pg/pgtest"
	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var (
	ids     = identity.Identities{Sync: []string{"sync"}, Controller: []string{"controller"}}
	syncCtx = pgtest.AsPlatform("sync")
	staff   = pgtest.AsPlatform("staff@datum.net")
	alice   = pgtest.AsUser("alice")
)

type seed struct {
	org      string
	n        int32
	title    string
	body     string
	state    v1.PostState
	cats     []string
	votes    int32
	created  string
	hidden   bool
	stage    v1.DeliveryStage
	stagedAt string
}

// The fixture: five visible posts and one hidden one.
var fixture = []seed{
	{org: "datum-cloud", n: 1, title: "Wildcard hostnames on ALBs", body: "<p>Route <b>*.example.com</b> to one load balancer</p>",
		state: v1.PostStateOnDeck, cats: []string{"deliver"}, votes: 12, created: "2026-01-01T00:00:00Z",
		stage: v1.DeliveryStagePickedUp, stagedAt: "2026-10-01T00:00:00Z"},
	{org: "datum-cloud", n: 2, title: "DNS zone import", body: "<p>Import BIND zone files</p>",
		state: v1.PostStatePlanning, cats: []string{"connect"}, votes: 30, created: "2026-03-01T00:00:00Z",
		stage: v1.DeliveryStagePlanned, stagedAt: "2026-09-01T00:00:00Z"},
	{org: "datum-cloud", n: 3, title: "Usage-based billing alerts", body: "<p>Email me before I go over budget</p>",
		state: v1.PostStateBacklog, cats: []string{"platform"}, votes: 5, created: "2026-05-01T00:00:00Z"},
	{org: "Milo-OS", n: 4, title: "Search across projects", body: "<p>One search box for every project</p>",
		state: v1.PostStateBacklog, cats: []string{"platform", "build"}, votes: 8, created: "2026-02-01T00:00:00Z"},
	{org: "milo-os", n: 5, title: "Hostnames for workloads", body: "<p>Give each workload a hostname</p>",
		state: v1.PostStateInProgress, cats: []string{"build"}, votes: 1, created: "2026-04-01T00:00:00Z"},
	{org: "datum-cloud", n: 6, title: "Hidden wildcard hostnames duplicate", state: v1.PostStateBacklog,
		cats: []string{"deliver"}, votes: 99, created: "2026-06-01T00:00:00Z", hidden: true},
}

func name(s seed) string { return v1.PostName(s.org, "enhancements", s.n) }

type env struct {
	db    *pg.DB
	posts *post.REST
}

func setup(t *testing.T) env {
	t.Helper()
	db := pgtest.Require(t, "posts", "votes", "subscriptions", "post_embeddings")
	scheme := runtime.NewScheme()
	install.Install(scheme)
	posts, err := post.NewREST(db, scheme, ids)
	if err != nil {
		t.Fatal(err)
	}
	e := env{db: db, posts: posts}
	for _, s := range fixture {
		e.add(t, s)
	}
	return e
}

func (e env) add(t *testing.T, s seed) {
	t.Helper()
	created, _ := time.Parse(time.RFC3339, s.created)
	p := &v1.Post{
		ObjectMeta: metav1.ObjectMeta{Name: name(s)},
		Status: v1.PostStatus{Title: s.title, BodyHTML: s.body, State: s.state, Categories: s.cats,
			Votes: v1.PostVotes{GitHub: s.votes},
			GitHub: &v1.GitHubIssue{Org: s.org, Repository: "enhancements", Number: s.n, State: "open",
				CreatedAt: metav1.NewTime(created)}},
	}
	obj, err := e.posts.Create(syncCtx, p, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p = obj.(*v1.Post)
	if s.stage != "" {
		at, _ := time.Parse(time.RFC3339, s.stagedAt)
		p.Status.Delivery = &v1.DeliveryStatus{Stage: s.stage,
			History: []v1.DeliveryEvent{{Stage: s.stage, At: metav1.NewTime(at)}}}
		obj, _, err = e.posts.Status.Update(pgtest.AsPlatform("controller"), p.Name, rest.DefaultUpdatedObjectInfo(p), nil, nil, false, &metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		p = obj.(*v1.Post)
	}
	if s.hidden {
		p.Spec.Hidden = true
		if _, _, err := e.posts.Update(staff, p.Name, rest.DefaultUpdatedObjectInfo(p), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func (e env) insert(t *testing.T, table, user, post string) {
	t.Helper()
	_, err := e.db.Pool.Exec(context.Background(), `INSERT INTO `+table+`
		(scope_type, scope_name, name, uid, resource_version, created_at, object, post_name)
		VALUES ('User', $1, $2, gen_random_uuid(), nextval('resource_version_seq'), now(), '{}', $2)`, user, post)
	if err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, r *REST, ctx context.Context, spec v1.PostQuerySpec) *v1.PostQuery {
	t.Helper()
	out, err := r.Create(ctx, &v1.PostQuery{Spec: spec}, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("query %+v: %v", spec, err)
	}
	return out.(*v1.PostQuery)
}

func names(q *v1.PostQuery) []string {
	var out []string
	for _, r := range q.Status.Results {
		out = append(out, r.Name)
	}
	return out
}

func ns(nums ...int32) []string {
	var out []string
	for _, n := range nums {
		for _, s := range fixture {
			if s.n == n {
				out = append(out, name(s))
			}
		}
	}
	return out
}

func want(t *testing.T, q *v1.PostQuery, nums ...int32) {
	t.Helper()
	if got, w := names(q), ns(nums...); !slices.Equal(got, w) {
		t.Fatalf("results %v, want %v", got, w)
	}
}

func TestSortsAndPaging(t *testing.T) {
	e := setup(t)
	r := NewREST(e.db, Options{})

	want(t, run(t, r, alice, v1.PostQuerySpec{}), 2, 1, 4, 3, 5) // top by default; hidden #6 never shows
	want(t, run(t, r, alice, v1.PostQuerySpec{Sort: v1.PostSortNewest}), 3, 5, 2, 4, 1)
	want(t, run(t, r, alice, v1.PostQuerySpec{Sort: v1.PostSortRecentlyChanged}), 1, 2, 3, 4, 5) // undelivered posts last, by name

	q := run(t, r, alice, v1.PostQuerySpec{Limit: 2})
	want(t, q, 2, 1)
	if q.Status.Continue == "" {
		t.Fatal("no continue on first page")
	}
	q = run(t, r, alice, v1.PostQuerySpec{Limit: 2, Continue: q.Status.Continue})
	want(t, q, 4, 3)
	q = run(t, r, alice, v1.PostQuerySpec{Limit: 2, Continue: q.Status.Continue})
	want(t, q, 5)
	if q.Status.Continue != "" {
		t.Fatal("continue on last page")
	}

	first := run(t, r, alice, v1.PostQuerySpec{Limit: 2})
	_, err := r.Create(alice, &v1.PostQuery{Spec: v1.PostQuerySpec{Limit: 2, Sort: v1.PostSortNewest, Continue: first.Status.Continue}}, nil, nil)
	if !apierrors.IsInvalid(err) {
		t.Fatalf("token reused with another query: got %v, want Invalid", err)
	}

	res := run(t, r, alice, v1.PostQuerySpec{Limit: 1}).Status.Results[0]
	if res.Title != "DNS zone import" || res.State != v1.PostStatePlanning || res.Votes.Total != 30 ||
		res.DeliveryStage != v1.DeliveryStagePlanned || res.Score != nil || !slices.Equal(res.Categories, []string{"connect"}) {
		t.Fatalf("result fields: %+v", res)
	}
}

func TestFiltersAndFacets(t *testing.T) {
	e := setup(t)
	r := NewREST(e.db, Options{})

	want(t, run(t, r, alice, v1.PostQuerySpec{Filter: v1.PostQueryFilter{State: []v1.PostState{v1.PostStateBacklog}}}), 4, 3)
	want(t, run(t, r, alice, v1.PostQuerySpec{Filter: v1.PostQueryFilter{Categories: []string{"build", "connect"}}}), 2, 4, 5)
	want(t, run(t, r, alice, v1.PostQuerySpec{Filter: v1.PostQueryFilter{Orgs: []string{"MILO-OS"}}}), 4, 5)
	want(t, run(t, r, alice, v1.PostQuerySpec{Filter: v1.PostQueryFilter{
		DeliveryStage: []v1.DeliveryStage{v1.DeliveryStagePickedUp}}}), 1)
	want(t, run(t, r, alice, v1.PostQuerySpec{Filter: v1.PostQueryFilter{
		Orgs: []string{"datum-cloud"}, Categories: []string{"deliver", "platform"}}}), 1, 3)

	q := run(t, r, alice, v1.PostQuerySpec{Limit: 1, Facets: []string{"state", "categories", "orgs", "deliveryStage"},
		Filter: v1.PostQueryFilter{State: []v1.PostState{v1.PostStateBacklog, v1.PostStateOnDeck, v1.PostStateInProgress}}})
	f := q.Status.Facets
	if f["state"]["backlog"] != 2 || f["state"]["on-deck"] != 1 || f["state"]["in-progress"] != 1 || len(f["state"]) != 3 {
		t.Errorf("state facet: %v", f["state"])
	}
	if f["categories"]["build"] != 2 || f["categories"]["platform"] != 2 || f["categories"]["deliver"] != 1 || len(f["categories"]) != 3 {
		t.Errorf("categories facet (hidden post must not count): %v", f["categories"])
	}
	if f["orgs"]["milo-os"] != 2 || f["orgs"]["datum-cloud"] != 2 {
		t.Errorf("orgs facet: %v", f["orgs"])
	}
	if f["deliveryStage"]["picked-up"] != 1 || len(f["deliveryStage"]) != 1 {
		t.Errorf("deliveryStage facet: %v", f["deliveryStage"])
	}
	if len(q.Status.Results) != 1 {
		t.Errorf("facets changed the page size")
	}
}

func TestMine(t *testing.T) {
	e := setup(t)
	r := NewREST(e.db, Options{})
	e.insert(t, "votes", "alice", ns(4)[0])
	e.insert(t, "votes", "alice", ns(6)[0]) // hidden
	e.insert(t, "votes", "bob", ns(2)[0])
	e.insert(t, "subscriptions", "alice", ns(3)[0])
	e.insert(t, "subscriptions", "alice", ns(4)[0])

	q := run(t, r, alice, v1.PostQuerySpec{Filter: v1.PostQueryFilter{VotedByMe: true}})
	want(t, q, 4)
	if !q.Status.Results[0].VotedByMe {
		t.Error("votedByMe not set on result")
	}
	want(t, run(t, r, alice, v1.PostQuerySpec{Filter: v1.PostQueryFilter{FollowedByMe: true}}), 4, 3)
	want(t, run(t, r, alice, v1.PostQuerySpec{Filter: v1.PostQueryFilter{FollowedByMe: true, VotedByMe: true}}), 4)

	for _, res := range run(t, r, alice, v1.PostQuerySpec{}).Status.Results {
		if res.VotedByMe != (res.Name == ns(4)[0]) {
			t.Errorf("%s votedByMe=%v", res.Name, res.VotedByMe)
		}
	}
	for _, res := range run(t, r, staff, v1.PostQuerySpec{}).Status.Results {
		if res.VotedByMe {
			t.Errorf("votedByMe set in the platform context on %s", res.Name)
		}
	}
	_, err := r.Create(staff, &v1.PostQuery{Spec: v1.PostQuerySpec{Filter: v1.PostQueryFilter{VotedByMe: true}}}, nil, nil)
	if !apierrors.IsInvalid(err) {
		t.Fatalf("votedByMe from the platform context: got %v, want Invalid", err)
	}
}

func TestKeywordText(t *testing.T) {
	e := setup(t)
	r := NewREST(e.db, Options{})

	q := run(t, r, alice, v1.PostQuerySpec{Text: "wildcard hostnames"})
	// #1 has both words in the title; #5 has one. Hidden #6 never shows.
	want(t, q, 1, 5)
	if s := q.Status.Results; s[0].Score == nil || s[1].Score == nil || *s[0].Score <= *s[1].Score {
		t.Errorf("scores not descending: %v %v", s[0].Score, s[1].Score)
	}
	want(t, run(t, r, alice, v1.PostQuerySpec{Text: "hostnames", Sort: v1.PostSortTop}), 1, 5)
	want(t, run(t, r, alice, v1.PostQuerySpec{Text: "BIND"}), 2) // body text
	want(t, run(t, r, alice, v1.PostQuerySpec{Text: "hostnames", Filter: v1.PostQueryFilter{Orgs: []string{"milo-os"}}}), 5)
	want(t, run(t, r, alice, v1.PostQuerySpec{Text: "the and of"})) // stop words only
}

// fakeSearch records queries and returns fixed hits.
type fakeSearch struct {
	hits []Hit
	err  error
	got  string
}

func (f *fakeSearch) Search(_ context.Context, text string, _ int) ([]Hit, error) {
	f.got = text
	return f.hits, f.err
}

func TestSearchServiceText(t *testing.T) {
	e := setup(t)
	s := &fakeSearch{hits: []Hit{{ns(5)[0], 0.9}, {ns(6)[0], 0.8}, {ns(3)[0], 0.7}, {"gone.post.1", 0.6}, {ns(2)[0], 0.5}}}
	r := NewREST(e.db, Options{Search: s})

	q := run(t, r, alice, v1.PostQuerySpec{Text: "  anything  "})
	if s.got != "anything" {
		t.Errorf("search got %q", s.got)
	}
	want(t, q, 5, 3, 2) // search's order; hidden and unknown posts dropped
	if *q.Status.Results[0].Score != 0.9 {
		t.Errorf("score %v, want search's 0.9", *q.Status.Results[0].Score)
	}
	want(t, run(t, r, alice, v1.PostQuerySpec{Text: "x", Sort: v1.PostSortTop}), 2, 3, 5)
	want(t, run(t, r, alice, v1.PostQuerySpec{Text: "x", Filter: v1.PostQueryFilter{State: []v1.PostState{v1.PostStateBacklog}}}), 3)

	s.hits = nil
	want(t, run(t, r, alice, v1.PostQuerySpec{Text: "x"}))

	s.err = errors.New("down")
	if _, err := r.Create(alice, &v1.PostQuery{Spec: v1.PostQuerySpec{Text: "x"}}, nil, nil); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("search down: got %v, want ServiceUnavailable", err)
	}
	// Browsing still works without search.
	want(t, run(t, r, alice, v1.PostQuerySpec{Limit: 1}), 2)
}

// fakeEmbedder maps text onto three topics by keyword, so similarity is
// predictable.
type fakeEmbedder struct {
	model string
	err   error
	calls int
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) Embed(_ context.Context, in []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(in))
	for i, s := range in {
		s = strings.ToLower(s)
		v := []float32{0.01, 0.01, 0.01}
		for j, words := range [][]string{{"hostname", "alb", "load balancer"}, {"dns", "zone"}, {"billing", "budget"}} {
			for _, w := range words {
				if strings.Contains(s, w) {
					v[j]++
				}
			}
		}
		out[i] = v
	}
	return out, nil
}

func TestSimilarTo(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	emb := &fakeEmbedder{model: "m1"}
	r := NewREST(e.db, Options{Embedder: emb})

	// Before anything is embedded, similarTo falls back to keywords without
	// calling the gateway.
	want(t, run(t, r, alice, v1.PostQuerySpec{SimilarTo: "zone files please"}), 2)
	if emb.calls != 0 {
		t.Fatalf("gateway called with no embeddings stored")
	}

	n, err := embedding.Refresh(ctx, e.db, emb, 2)
	if err != nil || n != 5 {
		t.Fatalf("refresh embedded %d (want 5 visible posts): %v", n, err)
	}
	if n, _ := embedding.Refresh(ctx, e.db, emb, 2); n != 0 {
		t.Fatalf("second refresh embedded %d, want 0", n)
	}

	q := run(t, r, alice, v1.PostQuerySpec{SimilarTo: "I want one ALB for many hostnames", Limit: 2})
	want(t, q, 1, 5)
	if s := q.Status.Results[0].Score; s == nil || *s < 0.9 {
		t.Errorf("similarity score %v", s)
	}
	want(t, run(t, r, alice, v1.PostQuerySpec{SimilarTo: "warn me about my billing budget", Limit: 1}), 3)

	// Gateway down: keyword fallback.
	emb.err = errors.New("gateway down")
	want(t, run(t, r, alice, v1.PostQuerySpec{SimilarTo: "zone import"}), 2)
	emb.err = nil

	// Changing a post's text re-embeds just that post; changing model
	// re-embeds everything and old vectors are ignored meanwhile.
	p, err := e.posts.Get(syncCtx, ns(3)[0], &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pp := p.(*v1.Post)
	pp.Status.Summary = "dns too"
	if _, _, err := e.posts.Status.Update(syncCtx, pp.Name, rest.DefaultUpdatedObjectInfo(pp), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if n, err := embedding.Refresh(ctx, e.db, emb, 10); n != 1 || err != nil {
		t.Fatalf("summary change re-embedded %d: %v", n, err)
	}
	emb2 := &fakeEmbedder{model: "m2"}
	r2 := NewREST(e.db, Options{Embedder: emb2})
	if _, err := r2.Create(alice, &v1.PostQuery{Spec: v1.PostQuerySpec{SimilarTo: "alb"}}, nil, nil); err != nil || emb2.calls != 0 {
		t.Fatalf("new model before refresh: err=%v calls=%d (want keyword fallback)", err, emb2.calls)
	}
	if n, _ := embedding.Refresh(ctx, e.db, emb2, 10); n != 5 {
		t.Fatalf("model change re-embedded %d, want 5", n)
	}
}

func TestSimilarToRateLimit(t *testing.T) {
	e := setup(t)
	r := NewREST(e.db, Options{SimilarLimit: ratelimit.NewLimiter(1, 1)})
	run(t, r, alice, v1.PostQuerySpec{SimilarTo: "dns"})
	_, err := r.Create(alice, &v1.PostQuery{Spec: v1.PostQuerySpec{SimilarTo: "dns"}}, nil, nil)
	if !apierrors.IsTooManyRequests(err) {
		t.Fatalf("second similarTo: got %v, want TooManyRequests", err)
	}
	if d, ok := apierrors.SuggestsClientDelay(err); !ok || d < 1 {
		t.Errorf("no Retry-After: %d %v", d, ok)
	}
	run(t, r, pgtest.AsUser("bob"), v1.PostQuerySpec{SimilarTo: "dns"}) // other users unaffected
	run(t, r, alice, v1.PostQuerySpec{Text: "dns"})                     // text isn't limited
}

func TestValidation(t *testing.T) {
	cases := map[string]v1.PostQuerySpec{
		"both":       {Text: "a", SimilarTo: "b"},
		"relevance":  {Sort: v1.PostSortRelevance},
		"sort":       {Sort: "best"},
		"limit":      {Limit: 101},
		"negative":   {Limit: -1},
		"state":      {Filter: v1.PostQueryFilter{State: []v1.PostState{"done"}}},
		"stage":      {Filter: v1.PostQueryFilter{DeliveryStage: []v1.DeliveryStage{"soon"}}},
		"facet":      {Facets: []string{"status"}},
		"dup facet":  {Facets: []string{"state", "state"}},
		"text":       {Text: strings.Repeat("a", maxTextLen+1)},
		"similarTo":  {SimilarTo: strings.Repeat("a", embedding.MaxInputChars+1)},
		"continue":   {Continue: "nope"},
		"followedBy": {Filter: v1.PostQueryFilter{FollowedByMe: true}},
	}
	for name, spec := range cases {
		user := "u"
		if name == "followedBy" {
			user = ""
		}
		if _, errs := parse(spec, user); len(errs) != 1 {
			t.Errorf("%s: want 1 error, got %v", name, errs)
		}
	}
	q, errs := parse(v1.PostQuerySpec{SimilarTo: " x "}, "")
	if len(errs) > 0 || q.sort != v1.PostSortRelevance || q.limit != defaultLimit {
		t.Errorf("defaults: %+v %v", q, errs)
	}
}

func TestSearchServiceClient(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/apis/search.miloapis.com/v1alpha1/resourcesearchqueries" {
			http.Error(w, "wrong endpoint "+req.URL.Path, http.StatusNotFound)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(req.Body).Decode(&b)
		bodies = append(bodies, b)
		spec := b["spec"].(map[string]any)
		switch spec["query"] {
		case "denied":
			_, _ = io.WriteString(w, `{"status":{"deniedTargetResources":[{"group":"feedback.miloapis.com","version":"v1alpha1","kind":"Post"}]}}`)
		case "error":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"kind":"Status","message":"nope"}`)
		default:
			if spec["continue"] == nil {
				_, _ = io.WriteString(w, `{"status":{"results":[{"resource":{"metadata":{"name":"a"}},"relevanceScore":0.9}],"continue":"p2"}}`)
			} else {
				_, _ = io.WriteString(w, `{"status":{"results":[{"resource":{"metadata":{"name":"b"}},"relevanceScore":0.5}]}}`)
			}
		}
	}))
	defer srv.Close()
	s, err := NewSearchServiceForConfig(&restclient.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(context.Background(), "hostnames", 10)
	if err != nil || len(hits) != 2 || hits[0] != (Hit{"a", 0.9}) || hits[1] != (Hit{"b", 0.5}) {
		t.Fatalf("hits %v, err %v", hits, err)
	}
	spec := bodies[0]["spec"].(map[string]any)
	target := spec["targetResources"].([]any)[0].(map[string]any)
	if spec["limit"].(float64) != searchPageSize || target["kind"] != "Post" || target["group"] != v1.GroupName {
		t.Errorf("request spec: %v", spec)
	}
	if hits, _ := s.Search(context.Background(), "hostnames", 1); len(hits) != 1 || len(bodies) != 3 {
		t.Errorf("max not respected: %v after %d requests", hits, len(bodies))
	}
	if _, err := s.Search(context.Background(), "denied", 10); err == nil {
		t.Error("denied posts index: want error")
	}
	if _, err := s.Search(context.Background(), "error", 10); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error status: %v", err)
	}
}
