package submission

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/registry/attachmentupload"
	"go.miloapis.com/feedback/internal/registry/securityreport"
	"go.miloapis.com/feedback/internal/registry/vote"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// Options configures Submission storage.
type Options struct {
	Identities identity.Identities
	// Files stores attachments. Nil disables them.
	Files *attachments.Store
	// Votes and Subscriptions give submitters a vote and a subscription when
	// their submission is approved or merged.
	Votes         *vote.REST
	Subscriptions *vote.SubscriptionREST
	// Reports receives submissions moved to the security track.
	Reports *securityreport.REST
	// Limit submissions per user to Limit per Window. Zero disables it.
	Limit  int
	Window time.Duration
}

// REST is Submission storage and its subresources.
type REST struct {
	*pg.Store
	Status     *pg.StatusStore
	Review     *ReviewREST
	ToSecurity *ToSecurityReportREST
	Attachment *attachmentupload.DownloadREST
}

var gr = v1.Resource("submissions")

// NewREST returns Submission storage.
func NewREST(db *pg.DB, typer runtime.ObjectTyper, o Options) (*REST, error) {
	s := strategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator, ids: o.Identities}
	h := hooks{db: db, o: o}
	store, err := pg.NewStore(db, pg.Kind{
		Table:                     "submissions",
		NewFunc:                   func() runtime.Object { return &v1.Submission{} },
		NewListFunc:               func() runtime.Object { return &v1.SubmissionList{} },
		DefaultQualifiedResource:  gr,
		SingularQualifiedResource: v1.Resource("submission"),
		CreateStrategy:            s, UpdateStrategy: s, DeleteStrategy: s,
		Scope: pg.UserOwned,
		Columns: func(obj runtime.Object) []pg.Column {
			sub := obj.(*v1.Submission)
			return []pg.Column{{Name: "kind", Value: string(sub.Spec.Kind)}, {Name: "phase", Value: string(sub.Status.Phase)}}
		},
		FieldColumns: map[string]string{"spec.kind": "kind", "status.phase": "phase"},
		BeforeWrite:  h.beforeWrite,
		AfterWrite:   h.afterWrite,
		Decorate: func(ctx context.Context, objs []runtime.Object) error {
			if kind, _ := pg.RequestContext(ctx); kind != pg.ScopePlatform {
				for _, obj := range objs {
					redact(obj.(*v1.Submission))
				}
			}
			return nil
		},
		TableConvertor: table{},
	})
	if err != nil {
		return nil, err
	}
	return &REST{
		Store:      store,
		Status:     pg.NewStatusStore(store, statusStrategy{s}),
		Review:     &ReviewREST{store: pg.NewStatusStore(store, reviewStrategy{s}), db: db, ids: o.Identities},
		ToSecurity: &ToSecurityReportREST{store: store, reports: o.Reports, files: o.Files, db: db, ids: o.Identities},
		Attachment: &attachmentupload.DownloadREST{
			Get: func(ctx context.Context, name string) ([]v1.AttachmentReference, error) {
				obj, err := store.Get(ctx, name, &metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return obj.(*v1.Submission).Spec.Attachments, nil
			},
			NewFunc:  func() runtime.Object { return &v1.Submission{} },
			Files:    o.Files,
			Purpose:  v1.AttachmentPurposeSubmission,
			Resource: v1.Resource("submissions/attachment"),
		},
	}, nil
}

type hooks struct {
	db *pg.DB
	o  Options
}

func (h hooks) beforeWrite(ctx context.Context, op pg.Op, _, obj runtime.Object) error {
	if op != pg.OpCreate {
		// Status and review writes come from the platform context too; users
		// can't change or withdraw a submission.
		if kind, _ := pg.RequestContext(ctx); kind != pg.ScopePlatform {
			return apierrors.NewForbidden(gr, "", errors.New("submissions can't be changed after they are sent"))
		}
		return nil
	}
	sub := obj.(*v1.Submission)
	if identity.EmailUnverified(ctx) {
		return apierrors.NewForbidden(gr, "", errors.New("verify your email address to send feedback"))
	}
	_, owner := pg.RequestContext(ctx)
	if err := h.rateLimit(ctx, owner); err != nil {
		return err
	}
	if len(sub.Spec.Attachments) == 0 {
		return nil
	}
	ap := field.NewPath("spec", "attachments")
	if h.o.Files == nil {
		return apierrors.NewInvalid(v1.Kind("Submission"), sub.Name, field.ErrorList{
			field.Forbidden(ap, "attachments are not available")})
	}
	refs := make([]string, len(sub.Spec.Attachments))
	for i, a := range sub.Spec.Attachments {
		refs[i] = a.UploadRef
	}
	attached, err := h.o.Files.Attach(ctx, v1.AttachmentPurposeSubmission, owner, sub.Name, refs)
	if errors.Is(err, attachments.ErrNotFound) {
		return apierrors.NewInvalid(v1.Kind("Submission"), sub.Name, field.ErrorList{
			field.Invalid(ap, refs, "every uploadRef must be one of your Submission uploads from the last day")})
	}
	if err != nil {
		klog.ErrorS(err, "Attaching files failed")
		return apierrors.NewServiceUnavailable("Attachments are not available; try again or send without them.")
	}
	// Names and types come from the upload, not the client.
	sub.Spec.Attachments = attached
	return nil
}

// rateLimit allows Limit submissions per Window per user, counted in
// Postgres so every replica agrees.
func (h hooks) rateLimit(ctx context.Context, owner string) error {
	if h.o.Limit <= 0 {
		return nil
	}
	var oldest *time.Time
	err := h.db.Q(ctx).QueryRow(ctx, `SELECT created_at FROM submissions
		WHERE scope_type = $1 AND scope_name = $2 AND created_at > now() - make_interval(secs => $3)
		ORDER BY created_at DESC OFFSET $4 LIMIT 1`,
		pg.ScopeUser, owner, h.o.Window.Seconds(), h.o.Limit-1).Scan(&oldest)
	if err != nil && !isNoRows(err) {
		return apierrors.NewInternalError(err)
	}
	if oldest == nil {
		return nil
	}
	wait := time.Until(oldest.Add(h.o.Window))
	return apierrors.NewTooManyRequests(
		fmt.Sprintf("You can send %d pieces of feedback every %s; try again later.", h.o.Limit, duration.HumanDuration(h.o.Window)),
		max(1, int(math.Ceil(wait.Seconds()))))
}

func (h hooks) afterWrite(ctx context.Context, op pg.Op, old, obj runtime.Object) error {
	switch op {
	case pg.OpUpdate:
		o, n := old.(*v1.Submission), obj.(*v1.Submission)
		owner := o.Annotations[pg.OwnerAnnotation]
		switch {
		case n.Status.Phase == v1.SubmissionPhaseMerged && o.Status.Phase != v1.SubmissionPhaseMerged:
			return h.grant(ctx, owner, n, n.Status.Review.MergedInto, v1.SubscriptionReasonMerged)
		case n.Status.Phase == v1.SubmissionPhaseApproved && o.Status.PostRef == nil && n.Status.PostRef != nil:
			// The post for an approved submission exists once sync links it.
			return h.grant(ctx, owner, n, n.Status.PostRef.Name, v1.SubscriptionReasonSubmitted)
		}
	case pg.OpDelete:
		if f := h.o.Files; f != nil && len(old.(*v1.Submission).Spec.Attachments) > 0 {
			if err := f.DeleteParent(ctx, v1.AttachmentPurposeSubmission, old.(*v1.Submission).Name); err != nil {
				klog.ErrorS(err, "Deleting a deleted submission's attachments failed", "submission", old.(*v1.Submission).Name)
			}
		}
	}
	return nil
}

// grant gives the submitter a subscription and a vote on post, in this
// transaction. A vote that already exists is left alone; a post that can't
// be voted on (hidden, merged) is skipped.
func (h hooks) grant(ctx context.Context, owner string, sub *v1.Submission, post string, reason v1.SubscriptionReason) error {
	if owner == "" || h.o.Votes == nil {
		return nil
	}
	as := asUser(ctx, owner)
	if err := h.o.Subscriptions.Follow(as, post, reason); err != nil && !apierrors.IsInvalid(err) {
		return err
	}
	v := &v1.Vote{
		ObjectMeta: metav1.ObjectMeta{Name: post},
		Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: post}, Source: sub.Spec.Context.Client},
	}
	_, err := h.o.Votes.Create(as, v, nil, &metav1.CreateOptions{})
	switch {
	case err == nil, apierrors.IsAlreadyExists(err):
		return nil
	case apierrors.IsInvalid(err):
		klog.InfoS("Not giving the submitter a vote", "submission", sub.Name, "post", post, "err", err)
		return nil
	}
	return err
}

// asUser is ctx acting for a user in the user context, keeping ctx's
// transaction. The apiserver itself is the actor.
func asUser(ctx context.Context, owner string) context.Context {
	return request.WithUser(ctx, &user.DefaultInfo{
		Name: "system:feedback-apiserver",
		Extra: map[string][]string{
			pg.ParentKindExtraKey: {pg.ScopeUser},
			pg.ParentNameExtraKey: {owner},
		},
	})
}

type table struct{}

func (table) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name"},
		{Name: "Owner", Type: "string", Priority: 1},
		{Name: "Kind", Type: "string"},
		{Name: "Phase", Type: "string"},
		{Name: "Post", Type: "string", Priority: 1},
		{Name: "Title", Type: "string"},
		{Name: "Age", Type: "date"},
	}}
	add := func(s *v1.Submission) {
		post := ""
		if s.Status.PostRef != nil {
			post = s.Status.PostRef.Name
		}
		age := "<unknown>"
		if !s.CreationTimestamp.IsZero() {
			age = duration.HumanDuration(time.Since(s.CreationTimestamp.Time))
		}
		t.Rows = append(t.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Object: s},
			Cells: []any{s.Name, s.Annotations[pg.OwnerAnnotation], string(s.Spec.Kind), string(s.Status.Phase),
				post, s.Spec.Title, age},
		})
	}
	switch o := obj.(type) {
	case *v1.Submission:
		add(o)
	case *v1.SubmissionList:
		t.ResourceVersion, t.Continue = o.ResourceVersion, o.Continue
		for i := range o.Items {
			add(&o.Items[i])
		}
	}
	return t, nil
}
