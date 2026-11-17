package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/index"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// ConditionApplied is the SubmissionReview condition that says whether the
// decision has been carried out.
const ConditionApplied = "Applied"

// AttachmentsFinalizer is on a submission or report whose attachments the
// operator copied under it. Deleting the object deletes the files, then the
// finalizer.
const AttachmentsFinalizer = "feedback.miloapis.com/attachments"

// SubmissionReconciler gives every new submission its SubmissionReview, owned
// by the submission so it goes with it, and starts it in PendingReview. It
// copies attached uploads under the submission before the bucket's lifecycle
// rule deletes them, and deletes them with the submission.
type SubmissionReconciler struct {
	client.Client
	Files *attachments.Store
}

// Reconcile implements reconcile.Reconciler.
func (r *SubmissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sub v1.Submission
	if err := r.Get(ctx, req.NamespacedName, &sub); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if done, err := keepAttachments(ctx, r.Client, r.Files, attachments.Submission, &sub, sub.Spec.UserRef.Name, sub.Spec.Attachments); done || err != nil {
		return ctrl.Result{}, err
	}
	review := &v1.SubmissionReview{ObjectMeta: metav1.ObjectMeta{Name: sub.Name}}
	if err := controllerutil.SetOwnerReference(&sub, review, r.Scheme()); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, review); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	if sub.Status.Phase == "" {
		sub.Status.Phase = v1.SubmissionPhasePendingReview
		return ctrl.Result{}, r.Status().Update(ctx, &sub)
	}
	return ctrl.Result{}, nil
}

// keepAttachments copies obj's attachments under it and adds
// AttachmentsFinalizer, or, when obj is being deleted, deletes them and
// removes the finalizer. done reports that obj is being deleted, so there is
// nothing else to do.
func keepAttachments(ctx context.Context, c client.Client, files *attachments.Store, p attachments.Purpose, obj client.Object, owner string, atts []v1.AttachmentReference) (done bool, err error) {
	has := controllerutil.ContainsFinalizer(obj, AttachmentsFinalizer)
	if obj.GetDeletionTimestamp() != nil {
		if !has {
			return true, nil
		}
		if files == nil {
			return true, fmt.Errorf("can't delete the attachments of %s: no attachments bucket", obj.GetName())
		}
		if err := files.DeleteParent(ctx, p, obj.GetName()); err != nil {
			return true, err
		}
		patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
		controllerutil.RemoveFinalizer(obj, AttachmentsFinalizer)
		return true, c.Patch(ctx, obj, patch)
	}
	if has || len(atts) == 0 || files == nil {
		return false, nil
	}
	refs := make([]string, len(atts))
	for i, a := range atts {
		refs[i] = a.UploadRef
	}
	if err := files.Attach(ctx, p, owner, obj.GetName(), refs); err != nil {
		return false, fmt.Errorf("copying attachments: %w", err)
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	controllerutil.AddFinalizer(obj, AttachmentsFinalizer)
	return false, c.Patch(ctx, obj, patch)
}

// SetupWithManager registers the reconciler.
func (r *SubmissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("submission").For(&v1.Submission{}).Complete(r)
}

// ReviewReconciler carries out staff decisions on SubmissionReviews:
//
//   - Rejected: the submission becomes Rejected with the reason.
//   - Merged: the submitter follows (reason merged) and votes on the post,
//     and the submission becomes Merged into it.
//   - Approved: the submission becomes Approved. feedback-sync creates the
//     GitHub issue and annotates the post it syncs with the submission's
//     name; then the submitter follows (reason submitted) and votes, and the
//     submission links to the post.
//
// MoveToSecurity is carried out by MoveReconciler.
// status.applied is set once a decision is fully carried out.
type ReviewReconciler struct {
	client.Client
}

// Reconcile implements reconcile.Reconciler.
func (r *ReviewReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var review v1.SubmissionReview
	if err := r.Get(ctx, req.NamespacedName, &review); err != nil || review.DeletionTimestamp != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	d := review.Spec.Decision
	if d == "" || d == v1.ReviewDecisionMoveToSecurity || review.Status.Applied {
		return ctrl.Result{}, nil
	}
	var sub v1.Submission
	if err := r.Get(ctx, req.NamespacedName, &sub); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var status v1.SubmissionStatus
	pending := ""
	switch d {
	case v1.ReviewDecisionRejected:
		status = v1.SubmissionStatus{Phase: v1.SubmissionPhaseRejected, Reason: review.Spec.Reason}
	case v1.ReviewDecisionMerged:
		post := review.Spec.MergedInto
		if err := grant(ctx, r.Client, &sub, post, v1.SubscriptionReasonMerged); err != nil {
			return ctrl.Result{}, err
		}
		status = v1.SubmissionStatus{Phase: v1.SubmissionPhaseMerged, PostRef: &v1.PostReference{Name: post}}
	case v1.ReviewDecisionApproved:
		status = v1.SubmissionStatus{Phase: v1.SubmissionPhaseApproved}
		var posts v1.PostList
		if err := r.List(ctx, &posts, client.MatchingFields{index.FromSubmission: sub.Name}); err != nil {
			return ctrl.Result{}, err
		}
		if len(posts.Items) == 0 {
			pending = "Waiting for feedback-sync to create the issue and sync its post."
			break
		}
		post := posts.Items[0].Name
		if err := grant(ctx, r.Client, &sub, post, v1.SubscriptionReasonSubmitted); err != nil {
			return ctrl.Result{}, err
		}
		status.PostRef = &v1.PostReference{Name: post}
	default:
		return ctrl.Result{}, nil
	}

	if sub.Status.Phase != status.Phase || sub.Status.Reason != status.Reason || !samePost(sub.Status.PostRef, status.PostRef) {
		sub.Status = status
		if err := r.Status().Update(ctx, &sub); err != nil {
			return ctrl.Result{}, err
		}
	}
	cond := metav1.Condition{Type: ConditionApplied, Status: metav1.ConditionTrue, Reason: string(d), ObservedGeneration: review.Generation}
	if pending != "" {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "WaitingForPost", pending
	}
	review.Status.Applied = pending == ""
	meta.SetStatusCondition(&review.Status.Conditions, cond)
	return ctrl.Result{}, r.Status().Update(ctx, &review)
}

func samePost(a, b *v1.PostReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name
}

// grant has the submitter follow and vote on post. Existing ones are kept. A
// vote the admission webhook refuses (the post was hidden or merged since) is
// skipped: the decision still stands.
func grant(ctx context.Context, c client.Client, sub *v1.Submission, post string, reason v1.SubscriptionReason) error {
	user := sub.Spec.UserRef
	follow := &v1.Subscription{
		ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post, user.Name)},
		Spec:       v1.SubscriptionSpec{PostRef: v1.PostReference{Name: post}, UserRef: user, Reason: reason},
	}
	vote := &v1.Vote{
		ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post, user.Name)},
		Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: post}, UserRef: user, Source: sub.Spec.Context.Client},
	}
	for _, obj := range []client.Object{follow, vote} {
		err := c.Create(ctx, obj)
		switch {
		case err == nil || apierrors.IsAlreadyExists(err):
		case apierrors.IsForbidden(err) || apierrors.IsInvalid(err):
			log.FromContext(ctx).Info("Not giving the submitter a follow or vote", "post", post, "err", err.Error())
		default:
			return fmt.Errorf("giving the submitter a %T on %s: %w", obj, post, err)
		}
	}
	return nil
}

// SetupWithManager registers the reconciler for reviews, their submissions,
// and posts synced from an approved submission.
func (r *ReviewReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("submission-review").
		For(&v1.SubmissionReview{}).
		Watches(&v1.Post{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			if s := obj.GetAnnotations()[v1.FromSubmissionAnnotation]; s != "" {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: s}}}
			}
			return nil
		})).
		Complete(r)
}
