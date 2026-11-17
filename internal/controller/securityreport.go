package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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

// MoveReconciler carries out MoveToSecurity decisions: it creates a
// SecurityReport owned by the submitter from the submission, named after it so
// a retry finds it, copies the attachments under it, then deletes the
// submission (and with it its review and files).
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

	var existing v1.SecurityReport
	err := r.Get(ctx, types.NamespacedName{Name: sub.Name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		rep := &v1.SecurityReport{
			ObjectMeta: metav1.ObjectMeta{Name: sub.Name},
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
			if r.Files == nil {
				return ctrl.Result{}, fmt.Errorf("can't move %s: it has attachments and there is no attachments bucket", sub.Name)
			}
			if err := r.Files.CopyParent(ctx, attachments.Submission, attachments.SecurityReport, sub.Name, rep.Name); err != nil {
				return ctrl.Result{}, fmt.Errorf("copying attachments: %w", err)
			}
			// Already copied, so the report reconciler only deletes them.
			controllerutil.AddFinalizer(rep, AttachmentsFinalizer)
		}
		if err := r.Create(ctx, rep); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	case err != nil:
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &sub))
}

// SetupWithManager registers the reconciler for reviews with a
// MoveToSecurity decision.
func (r *MoveReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("move-to-security").
		For(&v1.SubmissionReview{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.(*v1.SubmissionReview).Spec.Decision == v1.ReviewDecisionMoveToSecurity
		}))).
		Complete(r)
}
