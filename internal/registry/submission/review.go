package submission

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/registry/vote"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ReviewREST serves submissions/{name}/review: a staff decision.
type ReviewREST struct {
	store *pg.StatusStore
	db    *pg.DB
	ids   identity.Identities
}

var _ rest.NamedCreater = &ReviewREST{}

func (r *ReviewREST) New() runtime.Object { return &v1.SubmissionReview{} }
func (r *ReviewREST) Destroy()            {}

// Create records the decision and returns the updated Submission. Only
// staff in the platform context can decide, and only once.
func (r *ReviewREST) Create(ctx context.Context, name string, obj runtime.Object, createValidation rest.ValidateObjectFunc, opts *metav1.CreateOptions) (runtime.Object, error) {
	rv, ok := obj.(*v1.SubmissionReview)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("not a SubmissionReview: %T", obj))
	}
	if kind, _ := pg.RequestContext(ctx); kind != pg.ScopePlatform || r.ids.Of(ctx) != identity.Other {
		return nil, apierrors.NewForbidden(gr, name, errors.New("submissions are reviewed by staff in the platform context"))
	}
	if createValidation != nil {
		if err := createValidation(ctx, obj); err != nil {
			return nil, err
		}
	}
	if errs := validateReview(&rv.Spec); len(errs) > 0 {
		return nil, apierrors.NewInvalid(v1.Kind("SubmissionReview"), name, errs)
	}
	reviewer := ""
	if u, ok := request.UserFrom(ctx); ok {
		reviewer = u.GetName()
	}

	var out runtime.Object
	err := r.db.InTx(ctx, func(ctx context.Context) error {
		if rv.Spec.Decision == v1.ReviewDecisionMerged {
			if err := vote.CheckPost(ctx, r.db, v1.Kind("SubmissionReview"), name, rv.Spec.MergedInto); err != nil {
				return remapMergedInto(err, name)
			}
		}
		var err error
		out, _, err = r.store.Update(ctx, name, rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, old runtime.Object) (runtime.Object, error) {
			s := old.DeepCopyObject().(*v1.Submission)
			if s.Status.Phase != v1.SubmissionPhasePendingReview {
				return nil, apierrors.NewConflict(gr, name, fmt.Errorf("already %s", s.Status.Phase))
			}
			s.Status.Review = &v1.SubmissionReviewOutcome{
				Reviewer: reviewer, Decision: rv.Spec.Decision, ReviewedAt: metav1.Now(),
				PublicTitle: rv.Spec.PublicTitle, PublicBody: rv.Spec.PublicBody,
				MergedInto: rv.Spec.MergedInto, Reason: rv.Spec.Reason,
			}
			switch rv.Spec.Decision {
			case v1.ReviewDecisionApproved:
				s.Status.Phase = v1.SubmissionPhaseApproved
			case v1.ReviewDecisionMerged:
				s.Status.Phase = v1.SubmissionPhaseMerged
				s.Status.PostRef = &v1.PostReference{Name: rv.Spec.MergedInto}
			case v1.ReviewDecisionRejected:
				s.Status.Phase = v1.SubmissionPhaseRejected
			}
			return s, nil
		}), nil, nil, false, &metav1.UpdateOptions{DryRun: opts.DryRun})
		return err
	})
	return out, err
}

// remapMergedInto points a post check's error at spec.mergedInto.
func remapMergedInto(err error, name string) error {
	var st apierrors.APIStatus
	if errors.As(err, &st) && apierrors.IsInvalid(err) {
		msg := "must be a visible post that hasn't been merged"
		if d := st.Status().Details; d != nil && len(d.Causes) > 0 {
			msg = d.Causes[0].Message
		}
		return apierrors.NewInvalid(v1.Kind("SubmissionReview"), name, field.ErrorList{
			field.Invalid(field.NewPath("spec", "mergedInto"), "", msg)})
	}
	return err
}

func validateReview(s *v1.SubmissionReviewSpec) field.ErrorList {
	p := field.NewPath("spec")
	var errs field.ErrorList
	switch s.Decision {
	case v1.ReviewDecisionApproved:
		errs = append(errs, required(p.Child("publicTitle"), s.PublicTitle, maxTitle)...)
		errs = append(errs, required(p.Child("publicBody"), s.PublicBody, maxBody)...)
	case v1.ReviewDecisionMerged:
		if s.MergedInto == "" {
			errs = append(errs, field.Required(p.Child("mergedInto"), ""))
		}
	case v1.ReviewDecisionRejected:
		errs = append(errs, required(p.Child("reason"), s.Reason, maxReason)...)
	default:
		errs = append(errs, field.NotSupported(p.Child("decision"), s.Decision, decisions))
	}
	if s.Decision != v1.ReviewDecisionMerged && s.MergedInto != "" {
		errs = append(errs, field.Forbidden(p.Child("mergedInto"), "only for Merged"))
	}
	return errs
}

// AttachmentREST serves GET submissions/{name}/attachment?ref=: the file
// itself, to the submitter and staff.
type AttachmentREST struct {
	store *pg.Store
	files *attachments.Store
}

var _ rest.Connecter = &AttachmentREST{}

func (r *AttachmentREST) New() runtime.Object { return &v1.Submission{} }
func (r *AttachmentREST) Destroy()            {}

func (r *AttachmentREST) NewConnectOptions() (runtime.Object, bool, string) {
	return &v1.AttachmentOptions{}, false, ""
}

func (r *AttachmentREST) ConnectMethods() []string { return []string{http.MethodGet} }

// Connect checks the caller can read the submission (the store scopes it to
// the submitter in the user context) and that ref is attached to it.
func (r *AttachmentREST) Connect(ctx context.Context, name string, opts runtime.Object, _ rest.Responder) (http.Handler, error) {
	ref := opts.(*v1.AttachmentOptions).Ref
	if ref == "" {
		return nil, apierrors.NewBadRequest("set the ref query parameter to an attachment's uploadRef")
	}
	obj, err := r.store.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	sub := obj.(*v1.Submission)
	if !slices.ContainsFunc(sub.Spec.Attachments, func(a v1.AttachmentReference) bool { return a.UploadRef == ref }) {
		return nil, apierrors.NewNotFound(v1.Resource("submissions/attachment"), ref)
	}
	if r.files == nil {
		return nil, apierrors.NewServiceUnavailable("Attachments are not available.")
	}
	f, err := r.files.Get(ctx, v1.AttachmentPurposeSubmission, name, ref)
	if errors.Is(err, attachments.ErrNotFound) {
		return nil, apierrors.NewNotFound(v1.Resource("submissions/attachment"), ref)
	}
	if err != nil {
		klog.ErrorS(err, "Reading attachment failed", "submission", name, "ref", ref)
		return nil, apierrors.NewServiceUnavailable("Attachments are not available.")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveFile(w, f) }), nil
}

// serveFile sends a file as a download that browsers won't render or run.
func serveFile(w http.ResponseWriter, f *attachments.File) {
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(f.Data)))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(f.Data)
}
