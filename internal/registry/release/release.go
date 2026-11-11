// Package release implements storage for Release and ChangelogEntry. Both
// are synced from GitHub: only feedback-sync writes them and everyone can
// read them. Their status.posts lists are derived from the posts table on
// every read, so they always match the posts that exist and are visible.
package release

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage/names"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// REST is the storage for one of the kinds and its status subresource.
type REST struct {
	*pg.Store
	Status *pg.StatusStore
}

// strategy is shared by both kinds: they have only status, so an update
// through either endpoint may change it.
type strategy struct {
	runtime.ObjectTyper
	names.NameGenerator
	validate func(runtime.Object) field.ErrorList
}

func (strategy) NamespaceScoped() bool                                            { return false }
func (strategy) PrepareForCreate(context.Context, runtime.Object)                 {}
func (strategy) PrepareForUpdate(context.Context, runtime.Object, runtime.Object) {}
func (s strategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return s.validate(obj)
}
func (s strategy) ValidateUpdate(_ context.Context, obj, _ runtime.Object) field.ErrorList {
	return s.validate(obj)
}
func (strategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (strategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
func (strategy) AllowCreateOnUpdate(context.Context) bool      { return false }
func (strategy) AllowUnconditionalUpdate(context.Context) bool { return true }
func (strategy) Canonicalize(runtime.Object)                   {}

// syncOnly rejects writes from anyone but feedback-sync.
func syncOnly(ids identity.Identities, gr schema.GroupResource) func(context.Context, pg.Op, runtime.Object, runtime.Object) error {
	return func(ctx context.Context, op pg.Op, _, _ runtime.Object) error {
		if ids.Of(ctx) != identity.Sync {
			return apierrors.NewForbidden(gr, "", fmt.Errorf("%s is written only by feedback-sync", gr.Resource))
		}
		return nil
	}
}

// NewReleaseREST returns storage for Release.
func NewReleaseREST(db *pg.DB, typer runtime.ObjectTyper, ids identity.Identities) (*REST, error) {
	s := strategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator, validate: validateRelease}
	gr := v1.Resource("releases")
	guard := syncOnly(ids, gr)
	store, err := pg.NewStore(db, pg.Kind{
		Table:                     "releases",
		NewFunc:                   func() runtime.Object { return &v1.Release{} },
		NewListFunc:               func() runtime.Object { return &v1.ReleaseList{} },
		DefaultQualifiedResource:  gr,
		SingularQualifiedResource: v1.Resource("release"),
		CreateStrategy:            s, UpdateStrategy: s, DeleteStrategy: s,
		Scope: pg.PlatformScoped,
		Columns: func(obj runtime.Object) []pg.Column {
			return []pg.Column{{Name: "shipped", Value: obj.(*v1.Release).Status.Shipped}}
		},
		FieldColumns: map[string]string{"status.shipped": "shipped"},
		BeforeWrite: func(ctx context.Context, op pg.Op, old, obj runtime.Object) error {
			if err := guard(ctx, op, old, obj); err != nil {
				return err
			}
			if r, ok := obj.(*v1.Release); ok {
				r.Status.Posts = nil // derived on read
			}
			return nil
		},
		Decorate:       func(ctx context.Context, objs []runtime.Object) error { return decorateReleases(ctx, db, objs) },
		TableConvertor: releaseTable{},
	})
	if err != nil {
		return nil, err
	}
	return &REST{Store: store, Status: pg.NewStatusStore(store, s)}, nil
}

var dueOnRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func validateRelease(obj runtime.Object) field.ErrorList {
	r := obj.(*v1.Release)
	var errs field.ErrorList
	st := field.NewPath("status")
	if r.Status.Title == "" {
		errs = append(errs, field.Required(st.Child("title"), ""))
	}
	if d := r.Status.DueOn; d != "" {
		if _, err := time.Parse(time.DateOnly, d); err != nil || !dueOnRE.MatchString(d) {
			errs = append(errs, field.Invalid(st.Child("dueOn"), d, "must be YYYY-MM-DD"))
		}
	}
	return errs
}

// decorateReleases fills status.posts with the visible posts on each
// release's milestone.
func decorateReleases(ctx context.Context, db *pg.DB, objs []runtime.Object) error {
	byName := map[string]*v1.Release{}
	var keys []string
	for _, o := range objs {
		r := o.(*v1.Release)
		r.Status.Posts = nil
		byName[r.Name] = r
		keys = append(keys, r.Name)
	}
	rows, err := db.Q(ctx).Query(ctx,
		"SELECT release, name FROM posts WHERE release = ANY($1) AND NOT hidden ORDER BY name", keys)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("release posts: %w", err))
	}
	defer rows.Close()
	for rows.Next() {
		var rel, name string
		if err := rows.Scan(&rel, &name); err != nil {
			return err
		}
		byName[rel].Status.Posts = append(byName[rel].Status.Posts, name)
	}
	return rows.Err()
}

// NewChangelogREST returns storage for ChangelogEntry.
func NewChangelogREST(db *pg.DB, typer runtime.ObjectTyper, ids identity.Identities) (*REST, error) {
	s := strategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator, validate: validateChangelog}
	gr := v1.Resource("changelogentries")
	guard := syncOnly(ids, gr)
	store, err := pg.NewStore(db, pg.Kind{
		Table:                     "changelog_entries",
		NewFunc:                   func() runtime.Object { return &v1.ChangelogEntry{} },
		NewListFunc:               func() runtime.Object { return &v1.ChangelogEntryList{} },
		DefaultQualifiedResource:  gr,
		SingularQualifiedResource: v1.Resource("changelogentry"),
		CreateStrategy:            s, UpdateStrategy: s, DeleteStrategy: s,
		Scope: pg.PlatformScoped,
		Columns: func(obj runtime.Object) []pg.Column {
			var published *time.Time
			if t := obj.(*v1.ChangelogEntry).Status.PublishedAt; !t.IsZero() {
				published = &t.Time
			}
			return []pg.Column{{Name: "published_at", Value: published}}
		},
		BeforeWrite: func(ctx context.Context, op pg.Op, old, obj runtime.Object) error {
			if err := guard(ctx, op, old, obj); err != nil {
				return err
			}
			if c, ok := obj.(*v1.ChangelogEntry); ok {
				c.Status.Posts = nil // derived on read
			}
			return nil
		},
		Decorate:       func(ctx context.Context, objs []runtime.Object) error { return decorateChangelog(ctx, db, objs) },
		TableConvertor: changelogTable{},
	})
	if err != nil {
		return nil, err
	}
	return &REST{Store: store, Status: pg.NewStatusStore(store, s)}, nil
}

// issueRefRE matches "<org>/<repo>#<number>".
var issueRefRE = regexp.MustCompile(`^([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)#([1-9][0-9]*)$`)

// PostNameForRef returns the post name for an issue reference, or false if
// it isn't one.
func PostNameForRef(ref string) (string, bool) {
	m := issueRefRE.FindStringSubmatch(ref)
	if m == nil {
		return "", false
	}
	n, err := strconv.ParseInt(m[3], 10, 32)
	if err != nil {
		return "", false
	}
	return v1.PostName(m[1], m[2], int32(n)), true
}

func validateChangelog(obj runtime.Object) field.ErrorList {
	c := obj.(*v1.ChangelogEntry)
	var errs field.ErrorList
	st := field.NewPath("status")
	if c.Status.Title == "" {
		errs = append(errs, field.Required(st.Child("title"), ""))
	}
	if c.Status.PublishedAt.IsZero() {
		errs = append(errs, field.Required(st.Child("publishedAt"), ""))
	}
	for i, ref := range c.Status.IssueRefs {
		if _, ok := PostNameForRef(ref); !ok {
			errs = append(errs, field.Invalid(st.Child("issueRefs").Index(i), ref, "must be <org>/<repo>#<number>"))
		}
	}
	return errs
}

// decorateChangelog fills status.posts with the visible posts the entry's
// issueRefs point at. References to other issues (work items) are left in
// issueRefs only.
func decorateChangelog(ctx context.Context, db *pg.DB, objs []runtime.Object) error {
	wanted := map[string][]*v1.ChangelogEntry{}
	var keys []string
	for _, o := range objs {
		c := o.(*v1.ChangelogEntry)
		c.Status.Posts = nil
		for _, ref := range c.Status.IssueRefs {
			if name, ok := PostNameForRef(ref); ok {
				if _, seen := wanted[name]; !seen {
					keys = append(keys, name)
				}
				wanted[name] = append(wanted[name], c)
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err := db.Q(ctx).Query(ctx, "SELECT name FROM posts WHERE name = ANY($1) AND NOT hidden", keys)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("changelog posts: %w", err))
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		for _, c := range wanted[name] {
			c.Status.Posts = append(c.Status.Posts, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range objs {
		slices.Sort(o.(*v1.ChangelogEntry).Status.Posts)
	}
	return nil
}

type releaseTable struct{}

var _ rest.TableConvertor = releaseTable{}

func (releaseTable) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name"},
		{Name: "Title", Type: "string"},
		{Name: "Due", Type: "string"},
		{Name: "Shipped", Type: "boolean"},
		{Name: "Posts", Type: "integer"},
	}}
	add := func(r *v1.Release) {
		t.Rows = append(t.Rows, metav1.TableRow{Object: runtime.RawExtension{Object: r},
			Cells: []any{r.Name, r.Status.Title, r.Status.DueOn, r.Status.Shipped, len(r.Status.Posts)}})
	}
	switch o := obj.(type) {
	case *v1.Release:
		add(o)
	case *v1.ReleaseList:
		for i := range o.Items {
			add(&o.Items[i])
		}
	}
	return t, nil
}

type changelogTable struct{}

var _ rest.TableConvertor = changelogTable{}

func (changelogTable) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name"},
		{Name: "Title", Type: "string"},
		{Name: "Published", Type: "string"},
		{Name: "Posts", Type: "integer"},
	}}
	add := func(c *v1.ChangelogEntry) {
		t.Rows = append(t.Rows, metav1.TableRow{Object: runtime.RawExtension{Object: c},
			Cells: []any{c.Name, c.Status.Title, c.Status.PublishedAt.Format(time.DateOnly), len(c.Status.Posts)}})
	}
	switch o := obj.(type) {
	case *v1.ChangelogEntry:
		add(o)
	case *v1.ChangelogEntryList:
		for i := range o.Items {
			add(&o.Items[i])
		}
	}
	return t, nil
}
