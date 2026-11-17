package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.miloapis.com/feedback/internal/index"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// MergeReconciler moves votes and follows off a post that feedback-sync
// marked as merged into another (status.mergedInto). Each voter gets a vote
// on the target unless they already have one, then loses the old one; the
// same for follows. A target that is itself merged moves them on again.
type MergeReconciler struct {
	client.Client
}

// Reconcile implements reconcile.Reconciler.
func (r *MergeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var post v1.Post
	if err := r.Get(ctx, req.NamespacedName, &post); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	target := post.Status.MergedInto
	if target == "" || target == post.Name {
		return ctrl.Result{}, nil
	}

	var votes v1.VoteList
	if err := r.List(ctx, &votes, client.MatchingFields{index.Post: post.Name}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range votes.Items {
		old := &votes.Items[i]
		moved := &v1.Vote{
			ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(target, old.Spec.UserRef.Name)},
			Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: target}, UserRef: old.Spec.UserRef, Source: old.Spec.Source},
		}
		if err := r.move(ctx, moved, old); err != nil {
			return ctrl.Result{}, err
		}
	}

	var subs v1.SubscriptionList
	if err := r.List(ctx, &subs, client.MatchingFields{index.Post: post.Name}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range subs.Items {
		old := &subs.Items[i]
		moved := &v1.Subscription{
			ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(target, old.Spec.UserRef.Name)},
			Spec:       v1.SubscriptionSpec{PostRef: v1.PostReference{Name: target}, UserRef: old.Spec.UserRef, Reason: old.Spec.Reason},
		}
		if err := r.move(ctx, moved, old); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// move creates moved unless it exists, then deletes old.
func (r *MergeReconciler) move(ctx context.Context, moved, old client.Object) error {
	if err := r.Create(ctx, moved); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return client.IgnoreNotFound(r.Delete(ctx, old, client.Preconditions{UID: ptr(old.GetUID())}))
}

func ptr[T any](v T) *T { return &v }

// SetupWithManager registers the reconciler for merged posts, and for votes
// and follows that land on one.
func (r *MergeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toPost := func(name func(client.Object) string) handler.EventHandler {
		return handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name(obj)}}}
		})
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("merge").
		For(&v1.Post{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.(*v1.Post).Status.MergedInto != ""
		}))).
		Watches(&v1.Vote{}, toPost(func(o client.Object) string { return o.(*v1.Vote).Spec.PostRef.Name })).
		Watches(&v1.Subscription{}, toPost(func(o client.Object) string { return o.(*v1.Subscription).Spec.PostRef.Name })).
		Complete(r)
}
