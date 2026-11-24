package githubsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"go.miloapis.com/feedback/internal/github"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// sweepSource creates a FeedbackSource for an org unique to the test, so the
// sweep's look at every post of the org sees only this test's posts.
func sweepSource(t *testing.T, c client.Client) *v1.FeedbackSource {
	t.Helper()
	org := uniq(t)
	src := testSource()
	src.Name = org
	src.Spec.GitHub.Org = org
	src.Spec.GitHub.Project.Owner = org
	if err := c.Create(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	return src
}

func sweepIssue(t *testing.T, src *v1.FeedbackSource, repo string, n int32) *github.Issue {
	is := testIssue(t, n)
	is.Org, is.Repo = src.Spec.GitHub.Org, repo
	is.ID = key(is.Org, repo, n)
	is.ProjectItems = []github.ProjectItem{{Owner: src.Spec.GitHub.Org, Number: 22, Status: "Planning"}}
	return is
}

func reconcileSource(t *testing.T, r *SourceReconciler, src *v1.FeedbackSource) (ctrl.Result, *v1.FeedbackSource) {
	t.Helper()
	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: src.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got v1.FeedbackSource
	if err := r.Client.Get(ctx, types.NamespacedName{Name: src.Name}, &got); err != nil {
		t.Fatal(err)
	}
	return res, &got
}

func TestSweep(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	src := sweepSource(t, c)
	s := newSyncer(c, gh)
	r := &SourceReconciler{Client: c, Syncer: s, Interval: 7 * time.Minute}
	reset := time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)
	gh.rate = &github.RateLimit{Limit: 5000, Remaining: 4710, ResetAt: reset}

	matching := sweepIssue(t, src, "enhancements", 1)
	excludedRepo := sweepIssue(t, src, "infra", 2)
	notMatching := sweepIssue(t, src, "enhancements", 3)
	notMatching.IssueType, notMatching.Labels = "Task", nil
	unindexed := sweepIssue(t, src, "enhancements", 4)
	deleted := sweepIssue(t, src, "enhancements", 5)
	stopsMatching := sweepIssue(t, src, "enhancements", 6)
	for _, is := range []*github.Issue{matching, excludedRepo, notMatching, unindexed, deleted, stopsMatching} {
		gh.put(is)
	}
	// Posts from before: one whose issue search no longer returns but still
	// matches, one whose issue was deleted, one whose issue stopped matching.
	for _, is := range []*github.Issue{unindexed, deleted, stopsMatching} {
		syncIssue(t, s, src, is, Created)
	}
	gh.unindexed[key(unindexed.Org, unindexed.Repo, unindexed.Number)] = true
	gh.mu.Lock()
	delete(gh.issues, key(deleted.Org, deleted.Repo, deleted.Number))
	gh.mu.Unlock()
	stopsMatching.Labels = append(stopsMatching.Labels, "feedback:hide")
	gh.put(stopsMatching)
	// Another org's post with a name that starts the same way.
	other := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: src.Spec.GitHub.Org + "-other.enhancements.1"}}
	if err := c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	res, got := reconcileSource(t, r, src)
	if res.RequeueAfter != 7*time.Minute {
		t.Errorf("RequeueAfter = %v, want the interval", res.RequeueAfter)
	}
	if gh.searches != 2 {
		t.Errorf("searches = %d, want one for issue types and one for labels", gh.searches)
	}
	name := func(is *github.Issue) string { return v1.PostName(is.Org, is.Repo, is.Number) }
	if p := getPost(t, c, name(matching)); p.Status.State != v1.PostStatePlanning {
		t.Errorf("matching post state = %s", p.Status.State)
	}
	getPost(t, c, name(unindexed))
	getPost(t, c, other.Name)
	wantGone(t, c, name(excludedRepo))
	wantGone(t, c, name(notMatching))
	wantGone(t, c, name(deleted))
	wantGone(t, c, name(stopsMatching))

	ready := meta.FindStatusCondition(got.Status.Conditions, v1.FeedbackSourceConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != v1.FeedbackSourceReasonSynced || ready.ObservedGeneration != got.Generation {
		t.Errorf("Ready = %+v", ready)
	}
	if lr := got.Status.LastReconcile; lr == nil || lr.PostsSynced != 2 || lr.CompletedAt.IsZero() {
		t.Errorf("lastReconcile = %+v, want 2 posts synced", lr)
	}
	if rl := got.Status.RateLimit; rl == nil || rl.Remaining != 4710 || !rl.ResetAt.Time.Equal(reset) {
		t.Errorf("rateLimit = %+v", rl)
	}
	for _, m := range got.ManagedFields {
		if m.Subresource == "status" && m.Manager != FieldManager {
			t.Errorf("status written by %q", m.Manager)
		}
	}

	// The search fails next time: Ready goes False, the posts and the last
	// good reconcile stay.
	gh.searchErr = errors.New("GitHub GraphQL: 502 Bad Gateway")
	r.Retry = 45 * time.Second
	res, got = reconcileSource(t, r, src)
	if res.RequeueAfter != 45*time.Second {
		t.Errorf("RequeueAfter = %v, want the retry", res.RequeueAfter)
	}
	ready = meta.FindStatusCondition(got.Status.Conditions, v1.FeedbackSourceConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1.FeedbackSourceReasonSyncFailed || !strings.Contains(ready.Message, "502") {
		t.Errorf("Ready after a failed search = %+v", ready)
	}
	if lr := got.Status.LastReconcile; lr == nil || lr.PostsSynced != 2 {
		t.Errorf("lastReconcile after a failed search = %+v, want the last good one", lr)
	}
	getPost(t, c, name(matching))
	getPost(t, c, name(unindexed))
}

// TestSweepKeepsPostsOnErrors: a post search didn't return is only deleted
// when GitHub says its issue is gone, never because a read failed.
func TestSweepKeepsPostsOnErrors(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	src := sweepSource(t, c)
	s := newSyncer(c, gh)
	r := &SourceReconciler{Client: c, Syncer: s}
	is := sweepIssue(t, src, "enhancements", 1)
	gh.put(is)
	syncIssue(t, s, src, is, Created)

	k := key(is.Org, is.Repo, is.Number)
	gh.unindexed[k] = true
	gh.fail[k] = errors.New("GitHub GraphQL: 502 Bad Gateway")
	_, got := reconcileSource(t, r, src)
	getPost(t, c, v1.PostName(is.Org, is.Repo, is.Number))
	ready := meta.FindStatusCondition(got.Status.Conditions, v1.FeedbackSourceConditionReady)
	if ready == nil || ready.Reason != v1.FeedbackSourceReasonSyncFailed {
		t.Errorf("Ready = %+v, want SyncFailed", ready)
	}
}

func TestSweepWaitsForTheRateLimit(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	src := sweepSource(t, c)
	s := newSyncer(c, gh)
	now := s.Now()
	gh.rate = &github.RateLimit{Limit: 5000, Remaining: 20, ResetAt: now.Add(10 * time.Minute)}
	r := &SourceReconciler{Client: c, Syncer: s}

	res, got := reconcileSource(t, r, src)
	if gh.searches != 0 {
		t.Errorf("searched %d times while rate limited", gh.searches)
	}
	if res.RequeueAfter < 10*time.Minute || res.RequeueAfter > 11*time.Minute {
		t.Errorf("RequeueAfter = %v, want just after the reset", res.RequeueAfter)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1.FeedbackSourceConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1.FeedbackSourceReasonRateLimited {
		t.Errorf("Ready = %+v", ready)
	}
	if rl := got.Status.RateLimit; rl == nil || rl.Remaining != 20 {
		t.Errorf("rateLimit = %+v", rl)
	}

	// After the reset it sweeps.
	gh.rate.ResetAt = now.Add(-time.Second)
	reconcileSource(t, r, src)
	if gh.searches == 0 {
		t.Error("didn't sweep once the limit reset")
	}
}

// TestSweepBacksOffWhenRateLimited: a rate limit from search, or from
// re-reading a post's issue, stops the sweep until GitHub's retry time.
func TestSweepBacksOffWhenRateLimited(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	src := sweepSource(t, c)
	s := newSyncer(c, gh)
	r := &SourceReconciler{Client: c, Syncer: s}
	is := sweepIssue(t, src, "enhancements", 1)
	gh.put(is)
	syncIssue(t, s, src, is, Created)

	check := func(what string) {
		t.Helper()
		res, got := reconcileSource(t, r, src)
		if res.RequeueAfter < 3*time.Minute || res.RequeueAfter > 4*time.Minute {
			t.Errorf("%s: RequeueAfter = %v, want just after GitHub's retry time", what, res.RequeueAfter)
		}
		ready := meta.FindStatusCondition(got.Status.Conditions, v1.FeedbackSourceConditionReady)
		if ready == nil || ready.Reason != v1.FeedbackSourceReasonRateLimited || !strings.Contains(ready.Message, "secondary") {
			t.Errorf("%s: Ready = %+v, want RateLimited", what, ready)
		}
		getPost(t, c, v1.PostName(is.Org, is.Repo, is.Number))
	}
	limited := &github.RateLimitError{RetryAt: s.Now().Add(3 * time.Minute), Message: "You have exceeded a secondary rate limit"}
	gh.searchErr = limited
	check("search")

	gh.searchErr = nil
	k := key(is.Org, is.Repo, is.Number)
	gh.unindexed[k] = true
	gh.fail[k] = fmt.Errorf("reading: %w", limited)
	check("re-reading a post's issue")
}

// TestSweepWithRefusedSearchResults: when GitHub refuses some search
// results, the rest are synced and the source reports the failure.
func TestSweepWithRefusedSearchResults(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	src := sweepSource(t, c)
	s := newSyncer(c, gh)
	r := &SourceReconciler{Client: c, Syncer: s}
	is := sweepIssue(t, src, "enhancements", 1)
	gh.put(is)
	gh.searchErr = &github.IncompleteError{Missing: 1}

	_, got := reconcileSource(t, r, src)
	getPost(t, c, v1.PostName(is.Org, is.Repo, is.Number))
	ready := meta.FindStatusCondition(got.Status.Conditions, v1.FeedbackSourceConditionReady)
	if ready == nil || ready.Reason != v1.FeedbackSourceReasonSyncFailed || !strings.Contains(ready.Message, "refused") {
		t.Errorf("Ready = %+v, want SyncFailed naming the refusal", ready)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo", 2); got != "h…" {
		t.Errorf("truncate split a rune: %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(short) = %q", got)
	}
}

// TestSourceReconcilerInAManager runs the sweep the way feedback-sync does:
// in a manager, reading posts and sources from its cache.
func TestSourceReconcilerInAManager(t *testing.T) {
	c := milo(t)
	mgr, err := ctrl.NewManager(envCfg.Config, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Controller names repeat with -count.
		Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub()
	r := &SourceReconciler{
		Client:   mgr.GetClient(),
		Syncer:   &Syncer{Client: mgr.GetClient(), GitHub: gh},
		Interval: time.Hour,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	src := testSource()
	src.Name = uniq(t)
	src.Spec.GitHub.Org, src.Spec.GitHub.Project.Owner = src.Name, src.Name
	is := sweepIssue(t, src, "enhancements", 1)
	gh.put(is)
	if err := c.Create(ctx, src); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the source is swept", func() error {
		var got v1.FeedbackSource
		if err := c.Get(ctx, types.NamespacedName{Name: src.Name}, &got); err != nil {
			return err
		}
		if !meta.IsStatusConditionTrue(got.Status.Conditions, v1.FeedbackSourceConditionReady) {
			return errors.New("not Ready yet")
		}
		return nil
	})
	name := v1.PostName(is.Org, is.Repo, is.Number)
	rv := getPost(t, c, name).ResourceVersion

	// A spec change sweeps again straight away. Nothing about the issue
	// changed, so its post isn't written.
	update := func(change func(*v1.FeedbackSource)) {
		t.Helper()
		var got v1.FeedbackSource
		if err := c.Get(ctx, types.NamespacedName{Name: src.Name}, &got); err != nil {
			t.Fatal(err)
		}
		change(&got)
		if err := c.Update(ctx, &got); err != nil {
			t.Fatal(err)
		}
	}
	update(func(s *v1.FeedbackSource) { s.Spec.CategoryMapping["Docs"] = "docs" })
	// The status is written last, with the generation the sweep saw.
	eventually(t, "a second sweep", func() error {
		var got v1.FeedbackSource
		if err := c.Get(ctx, types.NamespacedName{Name: src.Name}, &got); err != nil {
			return err
		}
		if ready := meta.FindStatusCondition(got.Status.Conditions, v1.FeedbackSourceConditionReady); ready == nil || ready.ObservedGeneration != got.Generation {
			return errors.New("not swept again yet")
		}
		return nil
	})
	if got := getPost(t, c, name).ResourceVersion; got != rv {
		t.Errorf("a second sweep of an unchanged issue wrote its post: %s -> %s", rv, got)
	}

	// The issue's repository is now excluded, so its post goes.
	update(func(s *v1.FeedbackSource) { s.Spec.GitHub.Repositories.Exclude = []string{"enhancements"} })
	eventually(t, "the excluded repository's post is deleted", func() error {
		var p v1.Post
		err := c.Get(ctx, types.NamespacedName{Name: v1.PostName(is.Org, is.Repo, is.Number)}, &p)
		if err == nil {
			return errors.New("post still there")
		}
		return client.IgnoreNotFound(err)
	})
}

var skipNameValidation = true

func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
