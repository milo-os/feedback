package controller

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
	// maxMergeChain is how many merges the operator follows from a merged
	// post looking for the post its votes go to. Real chains are one or two
	// long; this only stops a runaway walk.
	maxMergeChain = 10
	// mergedIntoIndex indexes posts by status.mergedInto.
	mergedIntoIndex = "status.mergedInto"
	// MergeFieldManager applies the VotesMoved condition on posts. It is not
	// FieldManager, which applies status.votes: an apply drops the fields its
	// manager applied before and left out, so each needs its own.
	MergeFieldManager = "feedback-operator-merge"
)

// MergeReconciler moves votes and follows off a post that feedback-sync
// marked as merged into another (status.mergedInto).
//
// It follows status.mergedInto from post to post to the first one that isn't
// merged, and moves them there: each voter gets a vote on that post unless
// they already have one, then loses the old one; the same for follows. It
// never moves them onto a post the vote webhook would refuse them on.
//
// When the chain doesn't end at a post people can vote on (it reaches a post
// that doesn't exist or is hidden, loops, or is longer than maxMergeChain),
// nothing is moved or deleted: the votes and follows stay on the merged post,
// still counted there, and the post's VotesMoved condition is False with the
// reason. The reconcile succeeds rather than retrying; a change to any post in
// the chain (created, deleted, hidden or shown, merged or unmerged) queues
// every post merged into it, directly or through others, so they move as soon
// as the chain leads somewhere they can go.
type MergeReconciler struct {
	client.Client
}

// Reconcile implements reconcile.Reconciler.
func (r *MergeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var post v1.Post
	if err := r.Get(ctx, req.NamespacedName, &post); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if post.Status.MergedInto == "" {
		// No longer merged; drop the condition if it has one.
		return ctrl.Result{}, r.setCondition(ctx, &post, nil)
	}

	target, blocked, err := r.resolve(ctx, &post)
	if err != nil {
		return ctrl.Result{}, err
	}
	if blocked != nil {
		log.FromContext(ctx).Info("Leaving votes and follows on a merged post", "reason", blocked.Reason, "message", blocked.Message)
		return ctrl.Result{}, r.setCondition(ctx, &post, blocked)
	}

	var votes v1.VoteList
	if err := r.List(ctx, &votes, client.MatchingFields{postIndex: post.Name}); err != nil {
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
	if err := r.List(ctx, &subs, client.MatchingFields{postIndex: post.Name}); err != nil {
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
	return ctrl.Result{}, r.setCondition(ctx, &post, &metav1.Condition{
		Type:    v1.PostConditionVotesMoved,
		Status:  metav1.ConditionTrue,
		Reason:  v1.PostReasonMoved,
		Message: fmt.Sprintf("Votes and follows were moved to post %q.", target),
	})
}

// resolve follows status.mergedInto from post to the first post that isn't
// merged and returns its name. When there is no such post people can vote
// on, it returns the False VotesMoved condition saying why instead.
//
// It reads posts from the cache the vote webhook also reads, so it agrees
// with the webhook about which posts take votes. If a post changes between
// the two reads, the create is refused, the reconcile fails and is retried,
// and the change itself queues the post again.
func (r *MergeReconciler) resolve(ctx context.Context, post *v1.Post) (string, *metav1.Condition, error) {
	blocked := func(reason, format string, args ...any) (string, *metav1.Condition, error) {
		return "", &metav1.Condition{
			Type:    v1.PostConditionVotesMoved,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: fmt.Sprintf(format, args...) + " Votes and follows stay on this post until then.",
		}, nil
	}
	seen := map[string]bool{post.Name: true}
	name := post.Status.MergedInto
	for range maxMergeChain {
		if seen[name] {
			return blocked(v1.PostReasonMergeCycle, "The posts this one is merged into loop back to post %q.", name)
		}
		seen[name] = true
		var next v1.Post
		err := r.Get(ctx, types.NamespacedName{Name: name}, &next)
		switch {
		case apierrors.IsNotFound(err) || (err == nil && next.Spec.Hidden):
			// Posts are public; like the webhook, don't say which.
			log.FromContext(ctx).V(1).Info("Merge target unavailable", "target", name, "hidden", err == nil)
			return blocked(v1.PostReasonTargetUnavailable, "Post %q, which this one is merged into, isn't available.", name)
		case err != nil:
			return "", nil, err
		case next.Status.MergedInto == "":
			return name, nil, nil
		}
		name = next.Status.MergedInto
	}
	return blocked(v1.PostReasonMergeChainTooLong, "The posts this one is merged into are merged on more than %d times.", maxMergeChain)
}

// move creates moved unless it exists, then deletes old.
func (r *MergeReconciler) move(ctx context.Context, moved, old client.Object) error {
	if err := r.Create(ctx, moved); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return client.IgnoreNotFound(r.Delete(ctx, old, client.Preconditions{UID: ptr(old.GetUID())}))
}

// setCondition applies want as the post's VotesMoved condition, or removes
// the condition when want is nil. It writes nothing when the post already
// has it, so repeat reconciles of a blocked post don't touch it.
func (r *MergeReconciler) setCondition(ctx context.Context, post *v1.Post, want *metav1.Condition) error {
	cur := meta.FindStatusCondition(post.Status.Conditions, v1.PostConditionVotesMoved)
	status := map[string]any{}
	if want == nil {
		if cur == nil {
			return nil
		}
		// Applying no conditions releases ours, which removes it.
	} else {
		want.ObservedGeneration = post.Generation
		if cur != nil && cur.Status == want.Status && cur.Reason == want.Reason &&
			cur.Message == want.Message && cur.ObservedGeneration == want.ObservedGeneration {
			return nil
		}
		conds := slices.Clone(post.Status.Conditions)
		meta.SetStatusCondition(&conds, *want) // keeps lastTransitionTime unless status changes
		c, err := runtime.DefaultUnstructuredConverter.ToUnstructured(meta.FindStatusCondition(conds, v1.PostConditionVotesMoved))
		if err != nil {
			return err
		}
		status["conditions"] = []any{c}
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1.SchemeGroupVersion.String(),
		"kind":       "Post",
		"metadata":   map[string]any{"name": post.Name},
		"status":     status,
	}}
	return r.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(MergeFieldManager), client.ForceOwnership)
}

func ptr[T any](v T) *T { return &v }

// SetupWithManager registers the reconciler for merged posts, for votes and
// follows that land on one, and for changes to posts merged posts lead to.
func (r *MergeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toPost := func(name func(client.Object) string) handler.EventHandler {
		return handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name(obj)}}}
		})
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("merge").
		For(&v1.Post{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			p := obj.(*v1.Post)
			return p.Status.MergedInto != "" || meta.FindStatusCondition(p.Status.Conditions, v1.PostConditionVotesMoved) != nil
		}))).
		Watches(&v1.Vote{}, toPost(func(o client.Object) string { return o.(*v1.Vote).Spec.PostRef.Name })).
		Watches(&v1.Subscription{}, toPost(func(o client.Object) string { return o.(*v1.Subscription).Spec.PostRef.Name })).
		// Whether a post takes votes depends on it existing, not being
		// hidden and not being merged; only those changes affect a chain.
		Watches(&v1.Post{}, handler.EnqueueRequestsFromMapFunc(r.mergedIntoPost), builder.WithPredicates(predicate.Funcs{
			CreateFunc: func(event.CreateEvent) bool { return true },
			DeleteFunc: func(event.DeleteEvent) bool { return true },
			UpdateFunc: func(e event.UpdateEvent) bool {
				o, n := e.ObjectOld.(*v1.Post), e.ObjectNew.(*v1.Post)
				return o.Spec.Hidden != n.Spec.Hidden || o.Status.MergedInto != n.Status.MergedInto
			},
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Complete(r)
}

// mergedIntoPost returns the posts merged into post, directly or through
// other posts, up to maxMergeChain merges away.
func (r *MergeReconciler) mergedIntoPost(ctx context.Context, post client.Object) []reconcile.Request {
	var reqs []reconcile.Request
	seen := map[string]bool{post.GetName(): true}
	next := []string{post.GetName()}
	for range maxMergeChain {
		var found []string
		for _, name := range next {
			var posts v1.PostList
			if err := r.List(ctx, &posts, client.MatchingFields{mergedIntoIndex: name}); err != nil {
				log.FromContext(ctx).Error(err, "Listing posts merged into a post failed", "post", name)
				return reqs
			}
			for _, p := range posts.Items {
				if !seen[p.Name] {
					seen[p.Name] = true
					found = append(found, p.Name)
					reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
				}
			}
		}
		if len(found) == 0 {
			break
		}
		next = found
	}
	return reqs
}
