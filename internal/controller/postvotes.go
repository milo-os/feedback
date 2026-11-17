package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// trendingRefresh is how often a post with a trending score is re-tallied, so
// the score decays as votes age.
const trendingRefresh = time.Hour

// PostVotesReconciler keeps a post's vote totals and trending score current.
// feedback-sync owns status.votes.github and githubReactions; this owns
// total, datum, linked and trendingScore, applied with server-side apply.
type PostVotesReconciler struct {
	client.Client
	Options
}

// Reconcile implements reconcile.Reconciler.
func (r *PostVotesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var post v1.Post
	if err := r.Get(ctx, req.NamespacedName, &post); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var votes v1.VoteList
	if err := r.List(ctx, &votes, client.MatchingFields{postIndex: post.Name}); err != nil {
		return ctrl.Result{}, err
	}
	voters := make([]voter, 0, len(votes.Items))
	for _, v := range votes.Items {
		if v.DeletionTimestamp == nil {
			voters = append(voters, voter{at: v.CreationTimestamp.Time, login: v.Status.GitHubLogin})
		}
	}
	want := tally(post.Status.Votes, voters, r.now())
	cur := post.Status.Votes
	if cur.Total != want.Total || cur.Datum != want.Datum || cur.Linked != want.Linked || cur.TrendingScore != want.TrendingScore {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1.SchemeGroupVersion.String(),
			"kind":       "Post",
			"metadata":   map[string]any{"name": post.Name},
			"status": map[string]any{"votes": map[string]any{
				"total":         int64(want.Total),
				"datum":         int64(want.Datum),
				"linked":        int64(want.Linked),
				"trendingScore": want.TrendingScore,
			}},
		}}
		if err := r.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}
	}
	if want.TrendingScore > 0 {
		return ctrl.Result{RequeueAfter: trendingRefresh}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler for posts and their votes.
func (r *PostVotesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("post-votes").
		For(&v1.Post{}).
		Watches(&v1.Vote{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.(*v1.Vote).Spec.PostRef.Name}}}
		})).
		Complete(r)
}
