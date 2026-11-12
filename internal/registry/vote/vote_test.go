package vote

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/registry/post"
	"go.miloapis.com/feedback/internal/storage/pg"
	"go.miloapis.com/feedback/internal/storage/pg/pgtest"
	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var (
	ids     = identity.Identities{Sync: []string{"sync"}}
	syncCtx = pgtest.AsPlatform("sync")
	staff   = pgtest.AsPlatform("staff@datum.net")
	alice   = pgtest.AsUser("alice")
	bob     = pgtest.AsUser("bob")
)

// fakeUsers answers Milo lookups from maps.
type fakeUsers struct {
	orgs   map[string][]string
	logins map[string]string
	err    error
}

func (f fakeUsers) Organizations(_ context.Context, u string) ([]string, error) {
	return f.orgs[u], f.err
}
func (f fakeUsers) GitHubLogin(_ context.Context, u string) (string, error) {
	return f.logins[u], f.err
}

type env struct {
	db    *pg.DB
	posts *post.REST
	votes *REST
	subs  *SubscriptionREST
}

func setup(t *testing.T, users fakeUsers) env {
	t.Helper()
	db := pgtest.Require(t, "posts", "votes", "subscriptions", "deleted_posts")
	scheme := runtime.NewScheme()
	install.Install(scheme)
	posts, err := post.NewREST(db, scheme, ids)
	if err != nil {
		t.Fatal(err)
	}
	subs, err := NewSubscriptionREST(db, scheme)
	if err != nil {
		t.Fatal(err)
	}
	votes, err := NewREST(db, scheme, users, subs)
	if err != nil {
		t.Fatal(err)
	}
	return env{db, posts, votes, subs}
}

func (e env) addPost(t *testing.T, n int32, github int32) string {
	t.Helper()
	p := &v1.Post{
		ObjectMeta: metav1.ObjectMeta{Name: v1.PostName("datum-cloud", "enhancements", n)},
		Status: v1.PostStatus{Title: "p", Votes: v1.PostVotes{GitHub: github},
			GitHub: &v1.GitHubIssue{Org: "datum-cloud", Repository: "enhancements", Number: n, State: "open"}},
	}
	if _, err := e.posts.Create(syncCtx, p, nil, &metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return p.Name
}

func (e env) getPost(t *testing.T, name string) *v1.Post {
	t.Helper()
	obj, err := e.posts.Get(staff, name, &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return obj.(*v1.Post)
}

// syncStatus applies f to the post's status as feedback-sync.
func (e env) syncStatus(t *testing.T, name string, f func(*v1.PostStatus)) {
	t.Helper()
	p := e.getPost(t, name)
	f(&p.Status)
	if _, _, err := e.posts.Status.Update(syncCtx, name, rest.DefaultUpdatedObjectInfo(p), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func newVote(post string) *v1.Vote {
	return &v1.Vote{ObjectMeta: metav1.ObjectMeta{Name: post},
		Spec: v1.VoteSpec{PostRef: v1.PostReference{Name: post}, Source: v1.ClientWebsite}}
}

func (e env) vote(t *testing.T, ctx context.Context, post string) *v1.Vote {
	t.Helper()
	obj, err := e.votes.Create(ctx, newVote(post), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("vote on %s: %v", post, err)
	}
	return obj.(*v1.Vote)
}

func (e env) unvote(t *testing.T, ctx context.Context, post string) {
	t.Helper()
	if _, _, err := e.votes.Delete(ctx, post, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func votesOf(t *testing.T, e env, name string) v1.PostVotes {
	t.Helper()
	return e.getPost(t, name).Status.Votes
}

func TestVoteCountsAndSubscribes(t *testing.T) {
	e := setup(t, fakeUsers{})
	p := e.addPost(t, 1, 4)
	before := e.getPost(t, p).ResourceVersion

	e.vote(t, alice, p)
	e.vote(t, bob, p)
	v := votesOf(t, e, p)
	if v.Datum != 2 || v.GitHub != 4 || v.Total != 6 || math.Abs(v.TrendingScore-2) > 0.01 {
		t.Fatalf("after two votes: %+v", v)
	}
	if e.getPost(t, p).ResourceVersion == before {
		t.Error("post resourceVersion not bumped by the vote")
	}

	sub, err := e.subs.Get(alice, p, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("vote didn't subscribe: %v", err)
	}
	if r := sub.(*v1.Subscription).Spec.Reason; r != v1.SubscriptionReasonVoted {
		t.Errorf("reason %q", r)
	}

	if _, err := e.votes.Create(alice, newVote(p), nil, &metav1.CreateOptions{}); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("second vote: got %v, want AlreadyExists", err)
	}

	e.unvote(t, alice, p)
	if v := votesOf(t, e, p); v.Datum != 1 || v.Total != 5 {
		t.Fatalf("after unvote: %+v", v)
	}
	if _, err := e.subs.Get(alice, p, &metav1.GetOptions{}); err != nil {
		t.Errorf("unvote removed the subscription: %v", err)
	}

	// Voting again after following manually keeps the manual subscription.
	e.vote(t, alice, p)
	if v := votesOf(t, e, p); v.Datum != 2 {
		t.Fatalf("revote: %+v", v)
	}
}

func TestVoteRules(t *testing.T) {
	e := setup(t, fakeUsers{})
	p := e.addPost(t, 1, 0)
	hidden := e.addPost(t, 2, 0)
	h := e.getPost(t, hidden)
	h.Spec.Hidden = true
	if _, _, err := e.posts.Update(staff, hidden, rest.DefaultUpdatedObjectInfo(h), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	mismatch := newVote(p)
	mismatch.Spec.PostRef.Name = hidden
	badSource := newVote(p)
	badSource.Spec.Source = v1.ClientEmail
	for name, c := range map[string]struct {
		ctx  context.Context
		v    *v1.Vote
		want func(error) bool
	}{
		"name mismatch":     {alice, mismatch, apierrors.IsInvalid},
		"bad source":        {alice, badSource, apierrors.IsInvalid},
		"missing post":      {alice, newVote("datum-cloud.enhancements.99"), apierrors.IsInvalid},
		"hidden post":       {alice, newVote(hidden), apierrors.IsInvalid},
		"platform context":  {staff, newVote(p), apierrors.IsForbidden},
		"org context":       {pgtest.AsContext("Organization", "acme"), newVote(p), apierrors.IsForbidden},
		"unverified email":  {withEmail("carol", "false"), newVote(p), apierrors.IsForbidden},
		"empty email claim": {withEmail("carol", ""), newVote(p), apierrors.IsForbidden},
		"lower-cased claim": {withExtra("carol", "iam.miloapis.com/emailverified", "false"), newVote(p), apierrors.IsForbidden},
	} {
		if _, err := e.votes.Create(c.ctx, c.v, nil, &metav1.CreateOptions{}); !c.want(err) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	e.vote(t, withEmail("dave", "true"), p)
	e.vote(t, alice, p) // no claim (machine-style identity) passes

	v := e.vote(t, bob, p)
	v.Spec.Source = v1.ClientPortal
	if _, _, err := e.votes.Update(bob, p, rest.DefaultUpdatedObjectInfo(v), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("spec change: got %v, want Invalid", err)
	}
	if v := votesOf(t, e, p); v.Datum != 3 {
		t.Errorf("datum %d, want 3", v.Datum)
	}
}

func withEmail(uid, verified string) context.Context {
	return withExtra(uid, EmailVerifiedExtraKey, verified)
}

func withExtra(uid, key, value string) context.Context {
	return request.WithUser(request.WithNamespace(context.Background(), ""), &user.DefaultInfo{
		Name: uid, UID: uid,
		Extra: map[string][]string{
			pg.ParentKindExtraKey: {"User"}, pg.ParentNameExtraKey: {uid},
			key: {value},
		},
	})
}

func TestVoterLookupsAndStaffViews(t *testing.T) {
	users := fakeUsers{orgs: map[string][]string{"alice": {"acme", "zeta"}}, logins: map[string]string{"alice": "Alice-GH"}}
	e := setup(t, users)
	p := e.addPost(t, 1, 0)
	q := e.addPost(t, 2, 0)

	in := newVote(p)
	in.Status.Organizations = []string{"forged"}
	obj, err := e.votes.Create(alice, in, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*v1.Vote)
	if len(got.Status.Organizations) != 2 || got.Status.Organizations[0] != "acme" || got.Status.GitHubLogin != "Alice-GH" {
		t.Errorf("status: %+v", got.Status)
	}
	if got.Annotations[pg.OwnerAnnotation] != "alice" {
		t.Errorf("owner annotation on create: %v", got.Annotations)
	}
	e.vote(t, bob, p)
	e.vote(t, bob, q)

	list := func(ctx context.Context, sel string) []v1.Vote {
		t.Helper()
		opts := &metainternalversion.ListOptions{}
		if sel != "" {
			opts.FieldSelector = fields.ParseSelectorOrDie(sel)
		}
		obj, err := e.votes.List(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		return obj.(*v1.VoteList).Items
	}
	if n := len(list(bob, "")); n != 2 {
		t.Errorf("bob sees %d votes, want his 2", n)
	}
	all := list(staff, "")
	if len(all) != 3 {
		t.Fatalf("staff see %d votes, want 3", len(all))
	}
	owners := map[string]int{}
	for _, v := range all {
		owners[v.Annotations[pg.OwnerAnnotation]]++
	}
	if owners["alice"] != 1 || owners["bob"] != 2 {
		t.Errorf("owners %v", owners)
	}
	if n := len(list(staff, "spec.postRef.name="+p)); n != 2 {
		t.Errorf("votes on %s: %d, want 2", p, n)
	}

	// A client can't set or change the owner annotation.
	v := list(alice, "")[0]
	v.Annotations[pg.OwnerAnnotation] = "mallory"
	obj, _, err = e.votes.Update(alice, v.Name, rest.DefaultUpdatedObjectInfo(&v), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil || obj.(*v1.Vote).Annotations[pg.OwnerAnnotation] != "alice" {
		t.Errorf("owner after update: %v %v", obj, err)
	}
	var stored []byte
	_ = e.db.Pool.QueryRow(context.Background(), "SELECT object->'metadata' FROM votes WHERE scope_name = 'alice'").Scan(&stored)
	if strings.Contains(string(stored), pg.OwnerAnnotation) {
		t.Errorf("owner annotation stored: %s", stored)
	}
}

func TestLookupFailureStillVotes(t *testing.T) {
	e := setup(t, fakeUsers{err: errors.New("milo down")})
	p := e.addPost(t, 1, 0)
	if v := e.vote(t, alice, p); len(v.Status.Organizations) != 0 || v.Status.GitHubLogin != "" {
		t.Errorf("status %+v", v.Status)
	}
}

func TestGitHubDedupeAndTrending(t *testing.T) {
	e := setup(t, fakeUsers{logins: map[string]string{"alice": "Alice-GH"}})
	p := e.addPost(t, 1, 0)
	e.vote(t, alice, p)
	e.vote(t, bob, p)

	weekAgo := metav1.NewTime(time.Now().Add(-7 * 24 * time.Hour))
	e.syncStatus(t, p, func(st *v1.PostStatus) {
		st.Votes.GitHub = 3
		st.Votes.GitHubReactions = []v1.GitHubReaction{
			{Login: "alice-gh", At: weekAgo}, // alice, linked: counted once
			{Login: "carol", At: weekAgo},    // half weight after a week
			{Login: "dan"},                   // no time: total only
		}
	})
	v := votesOf(t, e, p)
	if v.Datum != 2 || v.GitHub != 3 || v.Linked != 1 || v.Total != 4 {
		t.Fatalf("dedupe: %+v", v)
	}
	if math.Abs(v.TrendingScore-2.5) > 0.01 {
		t.Errorf("trending %v, want ~2.5 (two fresh votes + one week-old 👍)", v.TrendingScore)
	}

	// Sync can't write server-owned fields.
	e.syncStatus(t, p, func(st *v1.PostStatus) { st.Votes.Linked, st.Votes.Total = 0, 99 })
	if v := votesOf(t, e, p); v.Linked != 1 || v.Total != 4 {
		t.Errorf("sync overwrote server votes: %+v", v)
	}

	// Scores decay: age the votes two weeks and refresh.
	if _, err := e.db.Pool.Exec(context.Background(), "UPDATE votes SET created_at = now() - interval '14 days'"); err != nil {
		t.Fatal(err)
	}
	n, err := post.RefreshTrending(context.Background(), e.db, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("refresh changed %d posts: %v", n, err)
	}
	if v := votesOf(t, e, p); math.Abs(v.TrendingScore-1.0) > 0.01 {
		t.Errorf("trending after decay %v, want ~1.0 (2×0.25 + 0.5)", v.TrendingScore)
	}
	if n, _ := post.RefreshTrending(context.Background(), e.db, time.Now()); n != 0 {
		t.Errorf("second refresh rewrote %d posts", n)
	}
}

func TestMergeMovesVotes(t *testing.T) {
	e := setup(t, fakeUsers{})
	from := e.addPost(t, 1, 0)
	to := e.addPost(t, 2, 5)
	e.vote(t, alice, from)
	e.vote(t, bob, from)
	e.vote(t, alice, to)
	if _, err := e.subs.Create(pgtest.AsUser("carol"), &v1.Subscription{ObjectMeta: metav1.ObjectMeta{Name: from},
		Spec: v1.SubscriptionSpec{PostRef: v1.PostReference{Name: from}}}, nil, &metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	e.syncStatus(t, from, func(st *v1.PostStatus) { st.MergedInto = to })

	if v := votesOf(t, e, from); v.Datum != 0 || v.Total != 0 {
		t.Errorf("merged post still has votes: %+v", v)
	}
	if v := votesOf(t, e, to); v.Datum != 2 || v.Total != 7 {
		t.Errorf("target votes: %+v (want alice once, bob, plus 5 GitHub)", v)
	}
	obj, err := e.votes.Get(bob, to, &metav1.GetOptions{})
	if err != nil || obj.(*v1.Vote).Spec.PostRef.Name != to {
		t.Fatalf("bob's moved vote: %v %v", obj, err)
	}
	if _, err := e.votes.Get(bob, from, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("bob's old vote: %v", err)
	}
	if _, err := e.subs.Get(pgtest.AsUser("carol"), to, &metav1.GetOptions{}); err != nil {
		t.Errorf("carol's subscription didn't move: %v", err)
	}
	if _, err := e.votes.Create(pgtest.AsUser("carol"), newVote(from), nil, &metav1.CreateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("vote on merged post: got %v, want Invalid", err)
	}

	self := e.getPost(t, to)
	self.Status.MergedInto = to
	if _, _, err := e.posts.Status.Update(syncCtx, to, rest.DefaultUpdatedObjectInfo(self), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("merge into self: got %v, want Invalid", err)
	}
}

func TestSubscriptions(t *testing.T) {
	e := setup(t, fakeUsers{})
	p := e.addPost(t, 1, 0)

	in := &v1.Subscription{ObjectMeta: metav1.ObjectMeta{Name: p},
		Spec: v1.SubscriptionSpec{PostRef: v1.PostReference{Name: p}, Reason: v1.SubscriptionReasonVoted}}
	obj, err := e.subs.Create(alice, in, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := obj.(*v1.Subscription)
	if s.Spec.Reason != v1.SubscriptionReasonManual {
		t.Errorf("user-chosen reason kept: %q", s.Spec.Reason)
	}
	s.Spec.Reason = v1.SubscriptionReasonSubmitted
	if _, _, err := e.subs.Update(alice, p, rest.DefaultUpdatedObjectInfo(s), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("spec change: got %v", err)
	}
	if _, err := e.subs.Create(alice, &v1.Subscription{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.9"},
		Spec: v1.SubscriptionSpec{PostRef: v1.PostReference{Name: "datum-cloud.enhancements.9"}}}, nil, &metav1.CreateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("missing post: got %v", err)
	}
	if _, _, err := e.subs.Delete(alice, p, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.subs.Get(alice, p, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("after delete: %v", err)
	}
}
