package post

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/auditx"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// REST is the storage for posts and posts/status.
type REST struct {
	*pg.Store
	Status *pg.StatusStore
}

// Option configures post storage.
type Option func(*options)

type options struct {
	authz authorizer.Authorizer
}

// WithHiddenAuthorizer decides who sees hidden posts: callers allowed get on
// the posts/hidden virtual subresource (staff, feedback-sync,
// feedback-controller). Without it, every platform-context caller sees them,
// which is only right for tests and local development.
func WithHiddenAuthorizer(a authorizer.Authorizer) Option {
	return func(o *options) { o.authz = a }
}

// NewREST returns Postgres-backed storage for Post.
func NewREST(db *pg.DB, typer runtime.ObjectTyper, ids identity.Identities, opts ...Option) (*REST, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	s := newStrategy(typer, ids)
	h := hooks{db: db, ids: ids}
	store, err := pg.NewStore(db, pg.Kind{
		Table:                     "posts",
		NewFunc:                   func() runtime.Object { return &v1.Post{} },
		NewListFunc:               func() runtime.Object { return &v1.PostList{} },
		DefaultQualifiedResource:  v1.Resource("posts"),
		SingularQualifiedResource: v1.Resource("post"),
		CreateStrategy:            s,
		UpdateStrategy:            s,
		DeleteStrategy:            s,
		Scope:                     pg.PlatformScoped,
		Columns:                   columns,
		FieldColumns: map[string]string{
			"spec.hidden":              "hidden",
			"status.state":             "state",
			"status.github.org":        "github_org",
			"status.github.repository": "github_repository",
			"status.release":           "release",
		},
		TableConvertor: tableConvertor{},
		ReadFilter:     o.readFilter,
		BeforeWrite:    h.beforeWrite,
		AfterWrite:     h.afterWrite,
	})
	if err != nil {
		return nil, err
	}
	return &REST{Store: store, Status: pg.NewStatusStore(store, statusStrategy{s})}, nil
}

// readFilter hides hidden posts from callers who may not see them. Being in
// the platform context isn't enough: the website's service account reads
// posts there too.
func (o options) readFilter(ctx context.Context) string {
	if o.authz == nil {
		if kind, _ := pg.RequestContext(ctx); kind == pg.ScopePlatform {
			return ""
		}
		return "NOT hidden"
	}
	if CanSeeHidden(ctx, o.authz) {
		return ""
	}
	return "NOT hidden"
}

// CanSeeHidden reports whether the caller may get posts/hidden.
func CanSeeHidden(ctx context.Context, a authorizer.Authorizer) bool {
	u, ok := request.UserFrom(ctx)
	if !ok {
		return false
	}
	d, _, err := a.Authorize(ctx, authorizer.AttributesRecord{
		User: u, Verb: "get", APIGroup: v1.GroupName, APIVersion: v1.SchemeGroupVersion.Version,
		Resource: "posts", Subresource: "hidden", ResourceRequest: true,
	})
	if err != nil {
		klog.V(2).InfoS("Checking posts/hidden failed; hiding hidden posts", "user", u.GetName(), "err", err)
	}
	return d == authorizer.DecisionAllow
}

func columns(obj runtime.Object) []pg.Column {
	p := obj.(*v1.Post)
	var (
		org, repo string
		number    int32
		createdAt *time.Time
		stage     string
		changedAt *time.Time
	)
	if gh := p.Status.GitHub; gh != nil {
		org, repo, number = gh.Org, gh.Repository, gh.Number
		if !gh.CreatedAt.IsZero() {
			t := gh.CreatedAt.Time
			createdAt = &t
		}
	}
	if d := p.Status.Delivery; d != nil {
		stage = string(d.Stage)
		if n := len(d.History); n > 0 {
			t := d.History[n-1].At.Time
			changedAt = &t
		}
	}
	return []pg.Column{
		{Name: "hidden", Value: p.Spec.Hidden},
		{Name: "state", Value: string(p.Status.State)},
		{Name: "github_org", Value: org},
		{Name: "github_repository", Value: repo},
		{Name: "github_number", Value: number},
		{Name: "github_created_at", Value: createdAt},
		{Name: "release", Value: p.Status.Release},
		{Name: "votes_total", Value: p.Status.Votes.Total},
		{Name: "trending_score", Value: p.Status.Votes.TrendingScore},
		{Name: "delivery_stage", Value: stage},
		{Name: "delivery_changed_at", Value: changedAt},
	}
}

type hooks struct {
	db  *pg.DB
	ids identity.Identities
}

func (h hooks) beforeWrite(ctx context.Context, op pg.Op, old, obj runtime.Object) error {
	if op != pg.OpUpdate && h.ids.Of(ctx) != identity.Sync {
		name := ""
		if obj != nil {
			name = obj.(*v1.Post).Name
		} else {
			name = old.(*v1.Post).Name
		}
		return apierrors.NewForbidden(v1.Resource("posts"), name,
			fmt.Errorf("posts are created and deleted only by feedback-sync"))
	}
	if obj == nil {
		return nil
	}
	p := obj.(*v1.Post)
	q := h.db.Q(ctx)
	now := time.Now()
	// feedback-sync sets mergedInto when the issue is closed as a duplicate.
	// Votes and subscriptions follow, in this transaction.
	if to := p.Status.MergedInto; to != "" && (old == nil || old.(*v1.Post).Status.MergedInto != to) {
		if err := moveVotes(ctx, q, p.Name, to); err != nil {
			return apierrors.NewInternalError(fmt.Errorf("move votes to %s: %w", to, err))
		}
		auditx.Annotate(ctx, auditx.VotesMovedTo, to)
		if _, err := Retally(ctx, h.db, to, now); err != nil {
			return apierrors.NewInternalError(fmt.Errorf("retally %s: %w", to, err))
		}
	}
	if err := Tally(ctx, q, p, now); err != nil {
		return apierrors.NewInternalError(err)
	}
	return nil
}

func (h hooks) afterWrite(ctx context.Context, op pg.Op, old, obj runtime.Object) error {
	q := h.db.Q(ctx)
	var err error
	switch op {
	case pg.OpCreate:
		// A post that comes back keeps the votes it had.
		_, err = q.Exec(ctx, "DELETE FROM deleted_posts WHERE name = $1", obj.(*v1.Post).Name)
	case pg.OpDelete:
		name := old.(*v1.Post).Name
		if _, err = q.Exec(ctx, `INSERT INTO deleted_posts (name, deleted_at) VALUES ($1, now())
			ON CONFLICT (name) DO UPDATE SET deleted_at = excluded.deleted_at`, name); err == nil {
			_, err = q.Exec(ctx, "DELETE FROM post_embeddings WHERE post_name = $1", name)
		}
	case pg.OpUpdate:
		// The embedder re-embeds posts that have no embedding.
		o, n := old.(*v1.Post), obj.(*v1.Post)
		if o.Status.Title != n.Status.Title || o.Status.BodyHTML != n.Status.BodyHTML {
			_, err = q.Exec(ctx, "DELETE FROM post_embeddings WHERE post_name = $1", n.Name)
		}
	}
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("post %s side effects: %w", op, err))
	}
	return nil
}

// PurgeExpiredVotes deletes votes and subscriptions for posts deleted more
// than retention ago, and forgets those posts. It returns the number of
// votes removed.
func PurgeExpiredVotes(ctx context.Context, db *pg.DB, retention time.Duration) (int64, error) {
	var votes int64
	err := db.InTx(ctx, func(ctx context.Context) error {
		return db.Q(ctx).QueryRow(ctx, `
			WITH expired AS (
				DELETE FROM deleted_posts WHERE deleted_at < now() - make_interval(secs => $1) RETURNING name
			), v AS (
				DELETE FROM votes WHERE post_name IN (SELECT name FROM expired) RETURNING 1
			), s AS (
				DELETE FROM subscriptions WHERE post_name IN (SELECT name FROM expired) RETURNING 1
			)
			-- Data-modifying CTEs always run in full, so s runs without being read.
			SELECT count(*) FROM v`, retention.Seconds()).Scan(&votes)
	})
	return votes, err
}

var errNotPost = errors.New("not a Post")

type tableConvertor struct{}

var _ rest.TableConvertor = tableConvertor{}

func (tableConvertor) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name"},
		{Name: "State", Type: "string"},
		{Name: "Votes", Type: "integer"},
		{Name: "Stage", Type: "string"},
		{Name: "Hidden", Type: "boolean"},
		{Name: "Title", Type: "string"},
		{Name: "Age", Type: "string"},
	}}
	add := func(p *v1.Post) {
		stage := ""
		if p.Status.Delivery != nil {
			stage = string(p.Status.Delivery.Stage)
		}
		age := "<unknown>"
		if !p.CreationTimestamp.IsZero() {
			age = duration.HumanDuration(time.Since(p.CreationTimestamp.Time))
		}
		t.Rows = append(t.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Object: p},
			Cells:  []any{p.Name, string(p.Status.State), p.Status.Votes.Total, stage, p.Spec.Hidden, p.Status.Title, age},
		})
	}
	switch o := obj.(type) {
	case *v1.Post:
		add(o)
	case *v1.PostList:
		for i := range o.Items {
			add(&o.Items[i])
		}
	default:
		return nil, errNotPost
	}
	return t, nil
}
