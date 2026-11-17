package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.miloapis.com/feedback/internal/attachments"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// SecurityReportReconciler looks after each security report:
//
//   - A new report starts in Received, with a timeline entry the reporter
//     sees.
//   - It gets its SecurityReportTriage, owned by the report, for the
//     Security Team's private notes.
//   - Its attachments are copied under it and deleted with it.
//   - Once it is Fixed or Disclosed and the reporter chose public credit, it
//     has a SecurityAcknowledgement with the credited name, link, month and
//     severity; otherwise it has none.
type SecurityReportReconciler struct {
	client.Client
	Options
}

// Reconcile implements reconcile.Reconciler.
func (r *SecurityReportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rep v1.SecurityReport
	if err := r.Get(ctx, req.NamespacedName, &rep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owner := ""
	if rep.Spec.UserRef != nil {
		owner = rep.Spec.UserRef.Name
	}
	if done, err := keepAttachments(ctx, r.Client, r.Files, attachments.SecurityReport, &rep, owner, rep.Spec.Attachments); done || err != nil {
		return ctrl.Result{}, err
	}

	triage := &v1.SecurityReportTriage{ObjectMeta: metav1.ObjectMeta{Name: rep.Name}}
	if err := controllerutil.SetOwnerReference(&rep, triage, r.Scheme()); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, triage); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}

	if rep.Status.Phase == "" {
		rep.Status.Phase = v1.SecurityReportPhaseReceived
		rep.Status.Timeline = append(rep.Status.Timeline, v1.SecurityTimelineEntry{
			Phase: v1.SecurityReportPhaseReceived, At: metav1.NewTime(r.now().UTC().Truncate(time.Second)),
		})
		if err := r.Status().Update(ctx, &rep); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, r.acknowledge(ctx, &rep)
}

// acknowledge creates, updates or deletes the report's acknowledgement.
func (r *SecurityReportReconciler) acknowledge(ctx context.Context, rep *v1.SecurityReport) error {
	ack := &v1.SecurityAcknowledgement{ObjectMeta: metav1.ObjectMeta{Name: rep.Name}}
	public := rep.Spec.Credit != nil && rep.Spec.Credit.Public && rep.Spec.Credit.Name != ""
	fixedAt := fixedAt(rep)
	if !public || fixedAt == nil {
		return client.IgnoreNotFound(r.Delete(ctx, ack))
	}
	want := v1.SecurityAcknowledgementStatus{
		Name:     rep.Spec.Credit.Name,
		URL:      rep.Spec.Credit.URL,
		Month:    fixedAt.UTC().Format("2006-01"),
		Severity: rep.Status.Severity,
	}
	err := r.Get(ctx, client.ObjectKeyFromObject(ack), ack)
	switch {
	case apierrors.IsNotFound(err):
		ack.Status = want
		if err := controllerutil.SetOwnerReference(rep, ack, r.Scheme()); err != nil {
			return err
		}
		return r.Create(ctx, ack)
	case err != nil:
		return err
	case ack.Status == want:
		return nil
	}
	ack.Status = want
	return r.Update(ctx, ack)
}

// fixedAt is when the report first reached Fixed or Disclosed, or nil if it
// isn't there now.
func fixedAt(rep *v1.SecurityReport) *metav1.Time {
	switch rep.Status.Phase {
	case v1.SecurityReportPhaseFixed, v1.SecurityReportPhaseDisclosed:
	default:
		return nil
	}
	for _, e := range rep.Status.Timeline {
		if e.Phase == v1.SecurityReportPhaseFixed || e.Phase == v1.SecurityReportPhaseDisclosed {
			return &e.At
		}
	}
	return &rep.CreationTimestamp
}

// SetupWithManager registers the reconciler.
func (r *SecurityReportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("security-report").
		For(&v1.SecurityReport{}).
		Owns(&v1.SecurityAcknowledgement{}).
		Complete(r)
}

// ReasonReportNameTaken is the Applied=False reason on a MoveToSecurity
// review when a report the move didn't create already has the moved report's
// name. The submission is kept.
const ReasonReportNameTaken = "ReportNameTaken"

// MoveReconciler carries out MoveToSecurity decisions. It creates a
// SecurityReport owned by the submitter from the submission, named by
// v1.MovedReportName so a retry finds it and annotated with the submission's
// name and UID. Only once it has read back that report and confirmed it was
// made from this submission, for the same owner, does it copy the
// attachments under it and delete the submission (and with it its review and
// files). A report with that name that wasn't made from this submission is
// left alone, and so is the submission: the review gets Applied=False,
// ReportNameTaken, and the move is tried again when a report is deleted.
type MoveReconciler struct {
	client.Client
	Options
}

// Reconcile implements reconcile.Reconciler.
func (r *MoveReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var review v1.SubmissionReview
	if err := r.Get(ctx, req.NamespacedName, &review); err != nil || review.Spec.Decision != v1.ReviewDecisionMoveToSecurity {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var sub v1.Submission
	if err := r.Get(ctx, req.NamespacedName, &sub); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if sub.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	if len(sub.Spec.Attachments) > 0 {
		if r.Files == nil {
			return ctrl.Result{}, fmt.Errorf("can't move %s: it has attachments and there is no attachments bucket", sub.Name)
		}
		if !controllerutil.ContainsFinalizer(&sub, AttachmentsFinalizer) {
			// The submission reconciler hasn't copied the files under the
			// submission yet, so there is nothing to copy to the report.
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	key := types.NamespacedName{Name: v1.MovedReportName(sub.Name, sub.UID)}
	var rep v1.SecurityReport
	err := r.Get(ctx, key, &rep)
	if apierrors.IsNotFound(err) {
		rep = *movedReport(&sub, &review, key.Name)
		err = r.Create(ctx, &rep)
		if apierrors.IsAlreadyExists(err) {
			// Created since the cache was read, by an earlier attempt or by
			// someone else: read it back and check it like any other.
			rep = v1.SecurityReport{}
			err = r.Get(ctx, key, &rep)
		}
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !movedFrom(&rep, &sub) {
		log.FromContext(ctx).Info("Not moving the submission: a report it didn't create has the name", "report", rep.Name)
		return ctrl.Result{}, r.notApplied(ctx, &review, ReasonReportNameTaken, fmt.Sprintf(
			"SecurityReport %s already exists and wasn't created by moving this submission, so the submission and its files were kept. "+
				"The move is tried again once that report is deleted.", rep.Name))
	}
	if rep.DeletionTimestamp != nil {
		// The moved report is being deleted before the submission was. Wait
		// for it to go (its deletion requeues this) and create it again.
		return ctrl.Result{}, nil
	}

	if len(sub.Spec.Attachments) > 0 {
		if err := r.Files.CopyParent(ctx, attachments.Submission, attachments.SecurityReport, sub.Name, rep.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("copying attachments: %w", err)
		}
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &sub, client.Preconditions{UID: &sub.UID}))
}

// movedReport is the report sub moves to.
func movedReport(sub *v1.Submission, review *v1.SubmissionReview, name string) *v1.SecurityReport {
	rep := &v1.SecurityReport{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				v1.MovedFromSubmissionAnnotation:    sub.Name,
				v1.MovedFromSubmissionUIDAnnotation: string(sub.UID),
			},
		},
		Spec: v1.SecurityReportSpec{
			UserRef:     &v1.UserReference{Name: sub.Spec.UserRef.Name},
			Title:       sub.Spec.Title,
			Description: sub.Spec.Body,
			Affected:    review.Spec.Affected,
			Attachments: sub.Spec.Attachments,
			Context: v1.SecurityReportContext{
				Client: sub.Spec.Context.Client, Organization: sub.Spec.Context.Organization,
				ClientVersion: sub.Spec.Context.ClientVersion,
			},
		},
	}
	if len(sub.Spec.Attachments) > 0 {
		// The move copies the files, so the report reconciler only deletes
		// them.
		controllerutil.AddFinalizer(rep, AttachmentsFinalizer)
	}
	return rep
}

// movedFrom reports whether rep is the report moved from sub: it names sub
// by name and UID and has the same owner.
func movedFrom(rep *v1.SecurityReport, sub *v1.Submission) bool {
	a := rep.Annotations
	return sub.UID != "" &&
		a[v1.MovedFromSubmissionAnnotation] == sub.Name &&
		a[v1.MovedFromSubmissionUIDAnnotation] == string(sub.UID) &&
		rep.Spec.UserRef != nil && rep.Spec.UserRef.Name == sub.Spec.UserRef.Name
}

// notApplied records on the review why the move isn't done.
func (r *MoveReconciler) notApplied(ctx context.Context, review *v1.SubmissionReview, reason, msg string) error {
	if !meta.SetStatusCondition(&review.Status.Conditions, metav1.Condition{
		Type: ConditionApplied, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: review.Generation,
	}) {
		return nil
	}
	return r.Status().Update(ctx, review)
}

// pendingMoves returns every review with a MoveToSecurity decision. Their
// submissions haven't moved yet: a moved submission takes its review with
// it. A deleted report may have been in a move's way, so each is retried.
func (r *MoveReconciler) pendingMoves(ctx context.Context, _ client.Object) []reconcile.Request {
	var reviews v1.SubmissionReviewList
	if err := r.List(ctx, &reviews); err != nil {
		log.FromContext(ctx).Error(err, "Listing reviews to retry moves")
		return nil
	}
	var out []reconcile.Request
	for _, rv := range reviews.Items {
		if rv.Spec.Decision == v1.ReviewDecisionMoveToSecurity {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: rv.Name}})
		}
	}
	return out
}

// SetupWithManager registers the reconciler for reviews with a
// MoveToSecurity decision, and for deleted reports.
func (r *MoveReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("move-to-security").
		For(&v1.SubmissionReview{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.(*v1.SubmissionReview).Spec.Decision == v1.ReviewDecisionMoveToSecurity
		}))).
		Watches(&v1.SecurityReport{}, handler.EnqueueRequestsFromMapFunc(r.pendingMoves), builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return false },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			DeleteFunc:  func(event.DeleteEvent) bool { return true },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Complete(r)
}
