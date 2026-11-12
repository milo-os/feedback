package submission

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/registry/securityreport"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// ToSecurityReportREST serves submissions/{name}/to-security-report: staff
// move a misfiled submission to the security track. In one transaction a
// SecurityReport owned by the submitter is created from it, its attachments
// are copied, and the submission is deleted (which deletes the originals).
type ToSecurityReportREST struct {
	store   *pg.Store
	reports *securityreport.REST
	files   *attachments.Store
	db      *pg.DB
	ids     identity.Identities
}

var _ rest.NamedCreater = &ToSecurityReportREST{}

func (r *ToSecurityReportREST) New() runtime.Object { return &v1.SubmissionToSecurityReport{} }
func (r *ToSecurityReportREST) Destroy()            {}

// Create returns the new SecurityReport.
func (r *ToSecurityReportREST) Create(ctx context.Context, name string, obj runtime.Object, createValidation rest.ValidateObjectFunc, opts *metav1.CreateOptions) (runtime.Object, error) {
	in, ok := obj.(*v1.SubmissionToSecurityReport)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("not a SubmissionToSecurityReport: %T", obj))
	}
	if kind, _ := pg.RequestContext(ctx); kind != pg.ScopePlatform || r.ids.Of(ctx) != identity.Other {
		return nil, apierrors.NewForbidden(gr, name, errors.New("submissions are moved to the security track by staff in the platform context"))
	}
	if opts != nil && len(opts.DryRun) > 0 {
		return nil, apierrors.NewBadRequest("dry run isn't supported: moving copies attachments")
	}
	if createValidation != nil {
		if err := createValidation(ctx, obj); err != nil {
			return nil, err
		}
	}
	if len(in.Affected) > 200 {
		return nil, apierrors.NewInvalid(v1.Kind("SubmissionToSecurityReport"), name, field.ErrorList{
			field.TooLong(field.NewPath("affected"), "", 200)})
	}

	var out runtime.Object
	err := r.db.InTx(ctx, func(ctx context.Context) error {
		obj, err := r.store.Get(ctx, name, &metav1.GetOptions{})
		if err != nil {
			return err
		}
		sub := obj.(*v1.Submission)
		if sub.Status.Phase != v1.SubmissionPhasePendingReview {
			return apierrors.NewConflict(gr, name, fmt.Errorf("already %s; only submissions waiting for review can move", sub.Status.Phase))
		}
		owner := sub.Annotations[pg.OwnerAnnotation]
		if owner == "" {
			return apierrors.NewInternalError(fmt.Errorf("submission %s has no owner", name))
		}

		report := &v1.SecurityReport{Spec: v1.SecurityReportSpec{
			Title:       sub.Spec.Title,
			Description: sub.Spec.Body,
			Affected:    in.Affected,
			Attachments: sub.Spec.Attachments,
			Context: v1.SecurityReportContext{
				Client: sub.Spec.Context.Client, Organization: sub.Spec.Context.Organization,
				ClientVersion: sub.Spec.Context.ClientVersion,
			},
		}}
		// The report belongs to the submitter, so they can follow it.
		created, err := r.reports.Create(securityreport.WithMovedFiles(asUser(ctx, owner)), report, nil, &metav1.CreateOptions{})
		if err != nil {
			return err
		}
		rep := created.(*v1.SecurityReport)
		if len(sub.Spec.Attachments) > 0 {
			if r.files == nil {
				return apierrors.NewServiceUnavailable("Attachments are not available, so this submission can't move yet.")
			}
			if err := r.files.CopyParent(ctx, v1.AttachmentPurposeSubmission, v1.AttachmentPurposeSecurityReport, name, rep.Name); err != nil {
				return apierrors.NewInternalError(fmt.Errorf("copy attachments: %w", err))
			}
		}
		if _, _, err := r.store.Delete(ctx, name, nil, &metav1.DeleteOptions{}); err != nil {
			return err
		}
		// Return the staff view.
		out, err = r.reports.Get(ctx, rep.Name, &metav1.GetOptions{})
		return err
	})
	return out, err
}
