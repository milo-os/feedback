package securityreport

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/registry/attachmentupload"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// Options configures SecurityReport storage.
type Options struct {
	Identities identity.Identities
	// Files stores attachments. Nil disables them.
	Files *attachments.Store
	// Limit reports per reporter to Limit per Window. Zero disables it.
	Limit  int
	Window time.Duration
}

// REST is SecurityReport storage and its subresources.
type REST struct {
	*pg.Store
	Status     *pg.StatusStore
	Attachment *attachmentupload.DownloadREST
}

var gr = v1.Resource("securityreports")

type movedKey struct{}

// WithMovedFiles marks ctx as creating a report whose attachments the caller
// copies itself (to-security-report), so create doesn't look for uploads.
func WithMovedFiles(ctx context.Context) context.Context {
	return context.WithValue(ctx, movedKey{}, true)
}

// createScope puts a reporter's report in their user scope, and a report the
// Security Team logs for an external reporter in the platform scope.
func createScope(ctx context.Context) (pg.Scope, error) {
	switch kind, name := pg.RequestContext(ctx); kind {
	case pg.ScopeUser:
		return pg.Scope{Type: pg.ScopeUser, Name: name}, nil
	case pg.ScopePlatform:
		return pg.Scope{Type: pg.ScopePlatform}, nil
	default:
		return pg.Scope{}, fmt.Errorf("report from the user context, not %s", kind)
	}
}

// NewREST returns SecurityReport storage.
func NewREST(db *pg.DB, typer runtime.ObjectTyper, o Options) (*REST, error) {
	s := strategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator, ids: o.Identities, now: time.Now}
	h := hooks{db: db, o: o}
	store, err := pg.NewStore(db, pg.Kind{
		Table:                     "security_reports",
		NewFunc:                   func() runtime.Object { return &v1.SecurityReport{} },
		NewListFunc:               func() runtime.Object { return &v1.SecurityReportList{} },
		DefaultQualifiedResource:  gr,
		SingularQualifiedResource: v1.Resource("securityreport"),
		CreateStrategy:            s, UpdateStrategy: s, DeleteStrategy: s,
		Scope:       pg.UserOwned,
		CreateScope: createScope,
		Columns: func(obj runtime.Object) []pg.Column {
			r := obj.(*v1.SecurityReport)
			return []pg.Column{{Name: "phase", Value: string(r.Status.Phase)}, {Name: "severity", Value: string(r.Status.Severity)}}
		},
		FieldColumns: map[string]string{"status.phase": "phase", "status.severity": "severity"},
		BeforeWrite:  h.beforeWrite,
		AfterWrite:   h.afterWrite,
		Decorate: func(ctx context.Context, objs []runtime.Object) error {
			if kind, _ := pg.RequestContext(ctx); kind != pg.ScopePlatform {
				for _, obj := range objs {
					redact(obj.(*v1.SecurityReport))
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
		Store:  store,
		Status: pg.NewStatusStore(store, statusStrategy{s}),
		Attachment: &attachmentupload.DownloadREST{
			Get: func(ctx context.Context, name string) ([]v1.AttachmentReference, error) {
				obj, err := store.Get(ctx, name, &metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return obj.(*v1.SecurityReport).Spec.Attachments, nil
			},
			NewFunc:  func() runtime.Object { return &v1.SecurityReport{} },
			Files:    o.Files,
			Purpose:  v1.AttachmentPurposeSecurityReport,
			Resource: v1.Resource("securityreports/attachment"),
		},
	}, nil
}

type hooks struct {
	db *pg.DB
	o  Options
}

func (h hooks) beforeWrite(ctx context.Context, op pg.Op, _, obj runtime.Object) error {
	kind, owner := pg.RequestContext(ctx)
	if op != pg.OpCreate {
		if kind != pg.ScopePlatform {
			return apierrors.NewForbidden(gr, "", errors.New("security reports can't be changed after they are sent"))
		}
		return nil
	}
	r := obj.(*v1.SecurityReport)
	sp := field.NewPath("spec")
	if moved, _ := ctx.Value(movedKey{}).(bool); moved {
		return nil
	}
	if kind == pg.ScopePlatform {
		// The Security Team logging a report from email or GitHub.
		if h.o.Identities.Of(ctx) != identity.Other {
			return apierrors.NewForbidden(gr, "", errors.New("security reports are logged by the Security Team"))
		}
		var errs field.ErrorList
		if r.Spec.Reporter == nil {
			errs = append(errs, field.Required(sp.Child("reporter"), "reports logged in the platform context are for an external reporter"))
		}
		if len(r.Spec.Attachments) > 0 {
			errs = append(errs, field.Forbidden(sp.Child("attachments"), "attach files to logged reports in the tracking issue"))
		}
		if len(errs) > 0 {
			return apierrors.NewInvalid(v1.Kind("SecurityReport"), r.Name, errs)
		}
		return nil
	}

	if r.Spec.Reporter != nil {
		return apierrors.NewInvalid(v1.Kind("SecurityReport"), r.Name, field.ErrorList{
			field.Forbidden(sp.Child("reporter"), "you are the reporter; this is for reports the Security Team logs")})
	}
	if identity.EmailUnverified(ctx) {
		return apierrors.NewForbidden(gr, "", errors.New("verify your email address to report a vulnerability"))
	}
	if err := h.rateLimit(ctx, owner); err != nil {
		return err
	}
	if len(r.Spec.Attachments) == 0 {
		return nil
	}
	ap := sp.Child("attachments")
	if h.o.Files == nil {
		return apierrors.NewInvalid(v1.Kind("SecurityReport"), r.Name, field.ErrorList{field.Forbidden(ap, "attachments are not available")})
	}
	refs := make([]string, len(r.Spec.Attachments))
	for i, a := range r.Spec.Attachments {
		refs[i] = a.UploadRef
	}
	attached, err := h.o.Files.Attach(ctx, v1.AttachmentPurposeSecurityReport, owner, r.Name, refs)
	if errors.Is(err, attachments.ErrNotFound) {
		return apierrors.NewInvalid(v1.Kind("SecurityReport"), r.Name, field.ErrorList{
			field.Invalid(ap, refs, "every uploadRef must be one of your SecurityReport uploads from the last day")})
	}
	if err != nil {
		klog.ErrorS(err, "Attaching files to a security report failed")
		return apierrors.NewServiceUnavailable("Attachments are not available; try again or send without them.")
	}
	r.Spec.Attachments = attached
	return nil
}

// rateLimit allows Limit reports per Window per reporter, counted in
// Postgres so every replica agrees.
func (h hooks) rateLimit(ctx context.Context, owner string) error {
	if h.o.Limit <= 0 {
		return nil
	}
	var oldest time.Time
	err := h.db.Q(ctx).QueryRow(ctx, `SELECT created_at FROM security_reports
		WHERE scope_type = $1 AND scope_name = $2 AND created_at > now() - make_interval(secs => $3)
		ORDER BY created_at DESC OFFSET $4 LIMIT 1`,
		pg.ScopeUser, owner, h.o.Window.Seconds(), h.o.Limit-1).Scan(&oldest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return apierrors.NewInternalError(err)
	}
	return apierrors.NewTooManyRequests(
		fmt.Sprintf("You can send %d reports every %s; try again later, or email security@datum.net.", h.o.Limit, duration.HumanDuration(h.o.Window)),
		max(1, int(math.Ceil(time.Until(oldest.Add(h.o.Window)).Seconds()))))
}

func (h hooks) afterWrite(ctx context.Context, op pg.Op, old, _ runtime.Object) error {
	if op == pg.OpDelete && h.o.Files != nil {
		r := old.(*v1.SecurityReport)
		if len(r.Spec.Attachments) > 0 {
			if err := h.o.Files.DeleteParent(ctx, v1.AttachmentPurposeSecurityReport, r.Name); err != nil {
				klog.ErrorS(err, "Deleting a deleted report's attachments failed", "report", r.Name)
			}
		}
	}
	return nil
}

type table struct{}

func (table) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name"},
		{Name: "Owner", Type: "string", Priority: 1},
		{Name: "Phase", Type: "string"},
		{Name: "Severity", Type: "string"},
		{Name: "Title", Type: "string"},
		{Name: "Age", Type: "date"},
	}}
	add := func(r *v1.SecurityReport) {
		age := "<unknown>"
		if !r.CreationTimestamp.IsZero() {
			age = duration.HumanDuration(time.Since(r.CreationTimestamp.Time))
		}
		owner := r.Annotations[pg.OwnerAnnotation]
		if owner == "" && r.Spec.Reporter != nil {
			owner = "external"
		}
		t.Rows = append(t.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Object: r},
			Cells:  []any{r.Name, owner, string(r.Status.Phase), string(r.Status.Severity), r.Spec.Title, age},
		})
	}
	switch o := obj.(type) {
	case *v1.SecurityReport:
		add(o)
	case *v1.SecurityReportList:
		t.ResourceVersion, t.Continue = o.ResourceVersion, o.Continue
		for i := range o.Items {
			add(&o.Items[i])
		}
	}
	return t, nil
}
