package githubsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"go.miloapis.com/feedback/internal/github"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// Defaults for SourceReconciler.
const (
	DefaultInterval     = 10 * time.Minute
	DefaultRetry        = time.Minute
	DefaultMinRateLimit = 500
)

// maxMessage bounds the error text put in a condition message.
const maxMessage = 1024

var syncedPosts = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "feedback_sync_posts_total",
	Help: "Posts synced from GitHub issues, by source and result (created, updated, unchanged, deleted, skipped, failed).",
}, []string{"source", "result"})

func init() {
	metrics.Registry.MustRegister(syncedPosts)
}

// SourceReconciler sweeps each FeedbackSource on an interval: it searches
// GitHub for the issues the source may include, syncs each one, re-reads the
// source's posts the search didn't return (deleting those that no longer
// match), and reports the result on the source's status.
type SourceReconciler struct {
	// Client reads FeedbackSources and writes their status.
	Client client.Client
	Syncer *Syncer
	// Interval between sweeps of a source. Defaults to DefaultInterval.
	Interval time.Duration
	// Retry is how soon a failed sweep is tried again. Defaults to
	// DefaultRetry.
	Retry time.Duration
	// MinRateLimit is the GitHub rate limit a sweep keeps in hand: with fewer
	// points left, it waits for the limit to reset. Defaults to
	// DefaultMinRateLimit.
	MinRateLimit int

	mu sync.Mutex
	// unwritten holds, by source name, the status of a finished sweep that
	// couldn't be written, so a retry writes it without sweeping again.
	unwritten map[string]unwrittenStatus
}

type unwrittenStatus struct {
	generation int64
	ready      metav1.Condition
	completed  *v1.ReconcileStatus
	// next is when the next sweep is due.
	next time.Time
}

// ItemsFailedError means a sweep finished, but some issues or posts failed
// to sync. Sweeping again straight away is unlikely to fix them, so the
// source waits for its next sweep.
type ItemsFailedError struct{ Err error }

func (e *ItemsFailedError) Error() string { return e.Err.Error() }
func (e *ItemsFailedError) Unwrap() error { return e.Err }

func (r *SourceReconciler) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return DefaultInterval
}

func (r *SourceReconciler) retry() time.Duration {
	if r.Retry > 0 {
		return r.Retry
	}
	return DefaultRetry
}

func (r *SourceReconciler) minRateLimit() int {
	if r.MinRateLimit > 0 {
		return r.MinRateLimit
	}
	return DefaultMinRateLimit
}

// Reconcile implements reconcile.Reconciler.
func (r *SourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var src v1.FeedbackSource
	if err := r.Client.Get(ctx, req.NamespacedName, &src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if src.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	now := r.Syncer.now()
	if u, ok := r.takeUnwritten(src.Name); ok && u.generation == src.Generation {
		return r.finish(ctx, &src, u)
	}

	for _, inst := range r.installations(ctx, &src) {
		if rl := r.Syncer.GitHub.RateLimit(inst.id); rl != nil && rl.Remaining < r.minRateLimit() && now.Before(rl.ResetAt) {
			log.Info("Waiting for the GitHub rate limit to reset", "org", inst.org, "remaining", rl.Remaining, "resetAt", rl.ResetAt)
			return r.waitForRateLimit(ctx, &src, rl.ResetAt,
				fmt.Sprintf("%d GitHub API points left on the %s installation; the next sweep starts after the limit resets at %s", rl.Remaining, inst.org, rl.ResetAt.UTC().Format(time.RFC3339)))
		}
	}

	start := r.Syncer.now()
	synced, sweepErr := r.Sweep(ctx, &src)
	if ctx.Err() != nil {
		return ctrl.Result{}, ctx.Err()
	}
	if rl, ok := errors.AsType[*github.RateLimitError](sweepErr); ok {
		log.Info("GitHub rate limited the sweep", "retryAt", rl.RetryAt, "message", rl.Message, "postsSynced", synced)
		return r.waitForRateLimit(ctx, &src, rl.RetryAt,
			fmt.Sprintf("GitHub rate limited the sweep: %s; the next sweep starts at %s", truncate(rl.Message, maxMessage/2), rl.RetryAt.UTC().Format(time.RFC3339)))
	}
	failed := metav1.Condition{
		Type:   v1.FeedbackSourceConditionReady,
		Status: metav1.ConditionFalse,
		Reason: v1.FeedbackSourceReasonSyncFailed,
	}
	if _, ok := errors.AsType[*ItemsFailedError](sweepErr); ok {
		// The sweep finished; only some items failed. They're retried with
		// the next sweep, not by sweeping the whole source again now.
		log.Error(sweepErr, "Some issues failed to sync", "postsSynced", synced)
		failed.Message = truncate(sweepErr.Error(), maxMessage)
		return r.finish(ctx, &src, unwrittenStatus{generation: src.Generation, ready: failed, next: r.Syncer.now().Add(r.interval())})
	}
	if sweepErr != nil {
		// Search or listing posts failed, so the sweep didn't get far.
		log.Error(sweepErr, "Sweep failed", "postsSynced", synced)
		failed.Message = truncate(sweepErr.Error(), maxMessage)
		r.logStatusError(ctx, r.writeStatus(ctx, &src, failed, nil))
		return ctrl.Result{RequeueAfter: r.retry()}, nil
	}
	log.Info("Swept source", "postsSynced", synced, "took", r.Syncer.now().Sub(start).Round(time.Millisecond))
	return r.finish(ctx, &src, unwrittenStatus{
		generation: src.Generation,
		ready: metav1.Condition{
			Type:    v1.FeedbackSourceConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  v1.FeedbackSourceReasonSynced,
			Message: fmt.Sprintf("%d posts synced from %s", synced, src.Spec.GitHub.Org),
		},
		completed: &v1.ReconcileStatus{CompletedAt: metav1.NewTime(r.Syncer.now().UTC().Truncate(time.Second)), PostsSynced: int32(synced)},
		next:      r.Syncer.now().Add(r.interval()),
	})
}

// finish writes the status of a finished sweep and requeues the source for
// its next sweep. When the write fails, the status is kept and written on a
// retry soon, without sweeping again. Returning the error instead would make
// controller-runtime retry within milliseconds, ignoring RequeueAfter.
func (r *SourceReconciler) finish(ctx context.Context, src *v1.FeedbackSource, st unwrittenStatus) (ctrl.Result, error) {
	if err := r.writeStatus(ctx, src, st.ready, st.completed); err != nil {
		r.logStatusError(ctx, err)
		r.mu.Lock()
		if r.unwritten == nil {
			r.unwritten = map[string]unwrittenStatus{}
		}
		r.unwritten[src.Name] = st
		r.mu.Unlock()
		return ctrl.Result{RequeueAfter: min(r.retry(), max(st.next.Sub(r.Syncer.now()), 0))}, nil
	}
	return ctrl.Result{RequeueAfter: max(st.next.Sub(r.Syncer.now()), time.Second)}, nil
}

func (r *SourceReconciler) takeUnwritten(name string) (unwrittenStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.unwritten[name]
	delete(r.unwritten, name)
	return u, ok
}

type installation struct {
	org string
	id  int64
}

// installations are the installations a sweep of the source spends: its
// own, and the project owner's when the project is in another org.
func (r *SourceReconciler) installations(ctx context.Context, src *v1.FeedbackSource) []installation {
	gh := src.Spec.GitHub
	out := []installation{{org: gh.Org, id: gh.InstallationID}}
	if p := gh.Project; p != nil && !strings.EqualFold(p.Owner, gh.Org) {
		id, err := r.Syncer.GitHub.InstallationID(ctx, p.Owner)
		if err != nil {
			// Reading the project fails the same way; the sweep reports it.
			logf.FromContext(ctx).Error(err, "Finding the project owner's installation", "org", p.Owner)
		} else {
			out = append(out, installation{org: p.Owner, id: id})
		}
	}
	return out
}

// waitForRateLimit reports that the source is waiting for GitHub's rate
// limit and requeues it shortly after until.
func (r *SourceReconciler) waitForRateLimit(ctx context.Context, src *v1.FeedbackSource, until time.Time, message string) (ctrl.Result, error) {
	err := r.writeStatus(ctx, src, metav1.Condition{
		Type:    v1.FeedbackSourceConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  v1.FeedbackSourceReasonRateLimited,
		Message: message,
	}, nil)
	r.logStatusError(ctx, err)
	return ctrl.Result{RequeueAfter: max(until.Sub(r.Syncer.now()), 0) + 5*time.Second}, nil
}

// logStatusError logs a failed status write. Reconcile doesn't return it:
// controller-runtime retries an error within milliseconds, ignoring
// RequeueAfter, and that would run the whole sweep again.
func (r *SourceReconciler) logStatusError(ctx context.Context, err error) {
	if err != nil {
		logf.FromContext(ctx).Error(err, "Writing the FeedbackSource status")
	}
}

// Sweep syncs every issue the source includes and removes posts whose issue
// no longer matches. It returns how many of the source's issues have posts.
//
// Search can miss issues (its index lags, and an issue that stopped matching
// isn't returned at all), so each of the source's posts the searches didn't
// return is re-read on its own before anything is deleted. When GitHub
// refuses some search results, the rest are still synced. Failures of single
// issues or posts are returned as an *ItemsFailedError once the sweep has
// finished. A rate limit stops the sweep where it is. Posts marked merged
// into another (a transferred issue's old post) are left alone.
func (r *SourceReconciler) Sweep(ctx context.Context, src *v1.FeedbackSource) (int, error) {
	gh := src.Spec.GitHub
	count := func(res Result) { syncedPosts.WithLabelValues(src.Name, string(res)).Inc() }

	var issues []*github.Issue
	// itemErrs are failures of single issues or posts.
	var itemErrs []error
	ids := map[string]bool{}
	for _, q := range SearchQueries(&gh) {
		found, err := r.Syncer.GitHub.SearchIssues(ctx, gh.InstallationID, q, statusField(src), r.Syncer.now())
		if _, ok := errors.AsType[*github.IncompleteError](err); ok {
			itemErrs = append(itemErrs, err)
		} else if err != nil {
			return 0, err
		}
		for i := range found {
			if !ids[found[i].ID] {
				ids[found[i].ID] = true
				issues = append(issues, &found[i])
			}
		}
	}

	synced := 0
	seen := map[string]bool{}
	results, err := r.Syncer.Sync(ctx, src, issues)
	if results == nil && err != nil {
		return 0, err
	}
	if err != nil {
		itemErrs = append(itemErrs, err)
	}
	for i, res := range results {
		seen[v1.PostName(issues[i].Org, issues[i].Repo, issues[i].Number)] = true
		if res == "" {
			count("failed")
			continue
		}
		count(res)
		if kept(res) {
			synced++
		}
	}

	var posts v1.PostList
	if err := r.Syncer.reader().List(ctx, &posts); err != nil {
		return synced, fmt.Errorf("listing posts: %w", err)
	}
	prefix := strings.ToLower(gh.Org) + "."
	for i := range posts.Items {
		p := &posts.Items[i]
		if seen[p.Name] || p.DeletionTimestamp != nil || p.Status.MergedInto != "" || !ownedBy(p, gh.Org, prefix) {
			continue
		}
		repo, number, ok := IssueRef(p)
		if !ok {
			logf.FromContext(ctx).Info("Can't tell which issue a post is for; leaving it", "post", p.Name)
			continue
		}
		res, err := r.Syncer.SyncIssue(ctx, src, repo, number)
		if _, ok := errors.AsType[*github.RateLimitError](err); ok {
			count("failed")
			return synced, err
		}
		if err != nil {
			count("failed")
			itemErrs = append(itemErrs, err)
			continue
		}
		count(res)
		if kept(res) {
			synced++
		}
	}
	if len(itemErrs) > 0 {
		return synced, &ItemsFailedError{Err: errors.Join(itemErrs...)}
	}
	return synced, nil
}

// ownedBy reports whether a post is for an issue in org.
func ownedBy(p *v1.Post, org, prefix string) bool {
	if gh := p.Status.GitHub; gh != nil && gh.Org != "" {
		return strings.EqualFold(gh.Org, org)
	}
	return strings.HasPrefix(p.Name, prefix)
}

// sourceStatus is the part of a FeedbackSource's status that sweeps own.
// lastWebhookAt belongs to the webhook consumer, under its own field
// manager.
type sourceStatus struct {
	Conditions    []metav1.Condition  `json:"conditions,omitempty"`
	LastReconcile *v1.ReconcileStatus `json:"lastReconcile,omitempty"`
	RateLimit     *v1.RateLimitStatus `json:"rateLimit,omitempty"`
}

// writeStatus applies the Ready condition, the rate limit and, after a
// successful sweep, lastReconcile. Every field is applied each time: an
// apply drops what its manager applied before and left out.
func (r *SourceReconciler) writeStatus(ctx context.Context, src *v1.FeedbackSource, ready metav1.Condition, completed *v1.ReconcileStatus) error {
	ready.ObservedGeneration = src.Generation
	conds := slices.Clone(src.Status.Conditions)
	meta.SetStatusCondition(&conds, ready)
	st := sourceStatus{
		Conditions:    []metav1.Condition{*meta.FindStatusCondition(conds, v1.FeedbackSourceConditionReady)},
		LastReconcile: src.Status.LastReconcile,
		RateLimit:     src.Status.RateLimit,
	}
	if completed != nil {
		st.LastReconcile = completed
	}
	if rl := r.Syncer.GitHub.RateLimit(src.Spec.GitHub.InstallationID); rl != nil {
		st.RateLimit = &v1.RateLimitStatus{Remaining: int32(rl.Remaining), ResetAt: metav1.NewTime(rl.ResetAt.UTC())}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1.SchemeGroupVersion.String(),
		"kind":       "FeedbackSource",
		"metadata":   map[string]any{"name": src.Name},
		"status":     status,
	}}
	if err := r.Client.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("writing the status of FeedbackSource %s: %w", src.Name, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// SetupWithManager sweeps each FeedbackSource when it is created or its spec
// changes, and then every Interval. Sources are swept one at a time.
func (r *SourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("feedback-source-sweep").
		For(&v1.FeedbackSource{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
