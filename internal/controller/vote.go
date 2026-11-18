package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

const (
	// OrphanedAtAnnotation records when a vote's post went missing. Votes on
	// a post that doesn't return within OrphanRetention are deleted.
	OrphanedAtAnnotation = "feedback.miloapis.com/orphaned-at"
	// OrphanRetention is how long votes wait for a deleted post to return.
	OrphanRetention = 30 * 24 * time.Hour
	// lookupTimeout bounds the Milo lookups for one vote.
	lookupTimeout = 5 * time.Second
)

// VoteReconciler follows the post for each new voter, records the voter's
// organizations and linked GitHub username, and deletes votes whose post has
// been gone for OrphanRetention.
type VoteReconciler struct {
	client.Client
	Options
}

// Reconcile implements reconcile.Reconciler.
func (r *VoteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vote v1.Vote
	if err := r.Get(ctx, req.NamespacedName, &vote); err != nil || vote.DeletionTimestamp != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var post v1.Post
	err := r.Get(ctx, types.NamespacedName{Name: vote.Spec.PostRef.Name}, &post)
	switch {
	case apierrors.IsNotFound(err):
		return r.orphaned(ctx, &vote)
	case err != nil:
		return ctrl.Result{}, err
	}
	if _, ok := vote.Annotations[OrphanedAtAnnotation]; ok {
		patch := client.MergeFrom(vote.DeepCopy())
		delete(vote.Annotations, OrphanedAtAnnotation)
		if err := r.Patch(ctx, &vote, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	if vote.Status.Subscribed || post.Status.MergedInto != "" {
		// Done, or MergeReconciler moves it.
		return ctrl.Result{}, nil
	}

	sub := &v1.Subscription{
		ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post.Name, vote.Spec.UserRef.Name)},
		Spec:       v1.SubscriptionSpec{PostRef: vote.Spec.PostRef, UserRef: vote.Spec.UserRef, Reason: v1.SubscriptionReasonVoted},
	}
	if err := r.Create(ctx, sub); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}

	vote.Status.Subscribed = true
	if r.Users != nil {
		r.lookup(ctx, &vote)
	}
	return ctrl.Result{}, r.Status().Update(ctx, &vote)
}

// lookup records the voter's organizations and GitHub username. Failures are
// logged and the vote is recorded without them.
func (r *VoteReconciler) lookup(ctx context.Context, vote *v1.Vote) {
	l := log.FromContext(ctx)
	user := vote.Spec.UserRef.Name
	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	if orgs, err := r.Users.Organizations(lctx, user); err != nil {
		l.Error(err, "Looking up the voter's organizations failed; recording the vote without them")
	} else {
		vote.Status.Organizations = orgs
	}
	if login, err := r.Users.GitHubLogin(lctx, user); err != nil {
		l.Error(err, "Looking up the voter's GitHub username failed; a 👍 from them may be counted twice")
	} else {
		vote.Status.GitHubLogin = login
	}
}

// orphaned handles a vote whose post doesn't exist: it marks when the post
// went missing and deletes the vote once OrphanRetention has passed.
func (r *VoteReconciler) orphaned(ctx context.Context, vote *v1.Vote) (ctrl.Result, error) {
	now := r.now()
	at, err := time.Parse(time.RFC3339, vote.Annotations[OrphanedAtAnnotation])
	if err != nil {
		patch := client.MergeFrom(vote.DeepCopy())
		if vote.Annotations == nil {
			vote.Annotations = map[string]string{}
		}
		vote.Annotations[OrphanedAtAnnotation] = now.UTC().Format(time.RFC3339)
		return ctrl.Result{RequeueAfter: OrphanRetention}, r.Patch(ctx, vote, patch)
	}
	if left := at.Add(OrphanRetention).Sub(now); left > 0 {
		return ctrl.Result{RequeueAfter: left}, nil
	}
	log.FromContext(ctx).Info("Deleting a vote whose post has been gone for the retention period", "post", vote.Spec.PostRef.Name)
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, vote))
}

// SetupWithManager registers the reconciler. Posts appearing or disappearing
// re-check their votes; other post changes don't, so busy posts don't
// re-queue every vote.
func (r *VoteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("vote").
		For(&v1.Vote{}).
		Watches(&v1.Post{}, handler.EnqueueRequestsFromMapFunc(r.votesOf), builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return true },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Complete(r)
}

func (r *VoteReconciler) votesOf(ctx context.Context, post client.Object) []reconcile.Request {
	var votes v1.VoteList
	if err := r.List(ctx, &votes, client.MatchingFields{postIndex: post.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "Listing a post's votes failed", "post", post.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, len(votes.Items))
	for i, v := range votes.Items {
		reqs[i] = reconcile.Request{NamespacedName: types.NamespacedName{Name: v.Name}}
	}
	return reqs
}
