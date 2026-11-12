// Package vote implements storage for Vote and Subscription. Both are owned
// by a user: created, read and deleted in the user context, and listed
// across every user by staff in the platform context.
//
// Creating a Vote also creates a Subscription to the post, and recomputes
// the post's vote totals and trending score, in the same transaction.
package vote

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/milo"
	"go.miloapis.com/feedback/internal/registry/post"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// lookupTimeout bounds the Milo lookups made while voting.
const lookupTimeout = 3 * time.Second

// Votable are the clients a vote can come from.
var Votable = []v1.Client{v1.ClientWebsite, v1.ClientPortal, v1.ClientDatumctl, v1.ClientPatch}

// REST is Vote storage.
type REST struct {
	*pg.Store
}

type voteStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
	users milo.Users
}

func (voteStrategy) NamespaceScoped() bool { return false }

// PrepareForCreate drops client-set status and records the voter's
// organizations and linked GitHub username. Lookups are best effort.
func (s voteStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	v := obj.(*v1.Vote)
	v.Status = v1.VoteStatus{}
	_, user := pg.RequestContext(ctx)
	if s.users == nil || user == "" {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	if orgs, err := s.users.Organizations(lctx, user); err != nil {
		klog.ErrorS(err, "Looking up voter's organizations failed; recording the vote without them")
	} else {
		v.Status.Organizations = orgs
	}
	if login, err := s.users.GitHubLogin(lctx, user); err != nil {
		klog.ErrorS(err, "Looking up voter's GitHub username failed; the vote may be counted twice")
	} else {
		v.Status.GitHubLogin = login
	}
}

// PrepareForUpdate keeps status; only metadata can change.
func (voteStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	obj.(*v1.Vote).Status = old.(*v1.Vote).Status
}

func (voteStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	v := obj.(*v1.Vote)
	errs := validateRef(v.Name, v.Spec.PostRef)
	if s := v.Spec.Source; s != "" && !slices.Contains(Votable, s) {
		errs = append(errs, field.NotSupported(field.NewPath("spec", "source"), s, Votable))
	}
	return errs
}

func (s voteStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	errs := s.Validate(ctx, obj)
	if !equality.Semantic.DeepEqual(obj.(*v1.Vote).Spec, old.(*v1.Vote).Spec) {
		errs = append(errs, field.Forbidden(field.NewPath("spec"), "is immutable; delete the vote and vote again"))
	}
	return errs
}

func (voteStrategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (voteStrategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
func (voteStrategy) AllowCreateOnUpdate(context.Context) bool      { return false }
func (voteStrategy) AllowUnconditionalUpdate(context.Context) bool { return true }
func (voteStrategy) Canonicalize(runtime.Object)                   {}

// validateRef checks that an object named after a post refers to it.
func validateRef(name string, ref v1.PostReference) field.ErrorList {
	p := field.NewPath("spec", "postRef", "name")
	switch {
	case ref.Name == "":
		return field.ErrorList{field.Required(p, "")}
	case ref.Name != name:
		return field.ErrorList{field.Invalid(p, ref.Name, "must equal metadata.name")}
	}
	return nil
}

// NewREST returns Vote storage. subs is used to follow the post on vote.
func NewREST(db *pg.DB, typer runtime.ObjectTyper, users milo.Users, subs *SubscriptionREST) (*REST, error) {
	s := voteStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator, users: users}
	gr := v1.Resource("votes")
	store, err := pg.NewStore(db, pg.Kind{
		Table:                     "votes",
		NewFunc:                   func() runtime.Object { return &v1.Vote{} },
		NewListFunc:               func() runtime.Object { return &v1.VoteList{} },
		DefaultQualifiedResource:  gr,
		SingularQualifiedResource: v1.Resource("vote"),
		CreateStrategy:            s, UpdateStrategy: s, DeleteStrategy: s,
		Scope: pg.UserOwned,
		Columns: func(obj runtime.Object) []pg.Column {
			return []pg.Column{{Name: "post_name", Value: obj.(*v1.Vote).Spec.PostRef.Name}}
		},
		FieldColumns: map[string]string{"spec.postRef.name": "post_name"},
		BeforeWrite: func(ctx context.Context, op pg.Op, _, obj runtime.Object) error {
			if op != pg.OpCreate {
				return userOnly(ctx)
			}
			v := obj.(*v1.Vote)
			if err := requireVerifiedEmail(ctx, v.Name); err != nil {
				return err
			}
			return CheckPost(ctx, db, v1.Kind("Vote"), v.Name, v.Spec.PostRef.Name)
		},
		AfterWrite: func(ctx context.Context, op pg.Op, old, obj runtime.Object) error {
			var postName string
			switch op {
			case pg.OpCreate:
				postName = obj.(*v1.Vote).Spec.PostRef.Name
				if err := subs.Follow(ctx, postName, v1.SubscriptionReasonVoted); err != nil {
					return err
				}
			case pg.OpDelete:
				postName = old.(*v1.Vote).Spec.PostRef.Name
			default:
				return nil
			}
			if _, err := post.Retally(ctx, db, postName, time.Now()); err != nil {
				return apierrors.NewInternalError(fmt.Errorf("recount votes on %s: %w", postName, err))
			}
			return nil
		},
		TableConvertor: voteTable{},
	})
	if err != nil {
		return nil, err
	}
	return &REST{Store: store}, nil
}

// userOnly keeps changes to votes and subscriptions with their owner: staff
// can list them in the platform context but not change or delete them.
func userOnly(ctx context.Context) error {
	if kind, _ := pg.RequestContext(ctx); kind != pg.ScopeUser {
		return apierrors.NewForbidden(v1.Resource("votes"), "", errors.New("votes and subscriptions are changed only by their owner"))
	}
	return nil
}

// requireVerifiedEmail rejects people whose email isn't verified.
func requireVerifiedEmail(ctx context.Context, name string) error {
	if identity.EmailUnverified(ctx) {
		return apierrors.NewForbidden(v1.Resource("votes"), name, errors.New("verify your email address to vote"))
	}
	return nil
}

// CheckPost checks the post exists, is visible and hasn't been merged.
func CheckPost(ctx context.Context, db *pg.DB, kind schema.GroupKind, name, postName string) error {
	var hidden bool
	var mergedInto string
	err := db.Q(ctx).QueryRow(ctx, `SELECT hidden, coalesce(object->'status'->>'mergedInto', '')
		FROM posts WHERE scope_type = $1 AND name = $2`, pg.ScopePlatform, postName).Scan(&hidden, &mergedInto)
	p := field.NewPath("spec", "postRef", "name")
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && hidden):
		return apierrors.NewInvalid(kind, name, field.ErrorList{field.NotFound(p, postName)})
	case err != nil:
		return apierrors.NewInternalError(err)
	case mergedInto != "":
		return apierrors.NewInvalid(kind, name, field.ErrorList{
			field.Invalid(p, postName, "this post was merged into "+mergedInto+"; use that one")})
	}
	return nil
}

type voteTable struct{}

func (voteTable) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name"},
		{Name: "Owner", Type: "string", Priority: 1},
		{Name: "Source", Type: "string"},
		{Name: "Organizations", Type: "string", Priority: 1},
		{Name: "Age", Type: "date"},
	}}
	add := func(v *v1.Vote) {
		t.Rows = append(t.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Object: v},
			Cells: []any{v.Name, v.Annotations[pg.OwnerAnnotation], string(v.Spec.Source),
				fmt.Sprint(v.Status.Organizations), age(v.CreationTimestamp)},
		})
	}
	switch o := obj.(type) {
	case *v1.Vote:
		add(o)
	case *v1.VoteList:
		t.ResourceVersion, t.Continue = o.ResourceVersion, o.Continue
		for i := range o.Items {
			add(&o.Items[i])
		}
	}
	return t, nil
}

func age(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(t.Time))
}

var _ rest.TableConvertor = voteTable{}
