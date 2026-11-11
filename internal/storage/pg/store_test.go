package pg_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage/names"

	"go.miloapis.com/feedback/internal/postgres"
	"go.miloapis.com/feedback/internal/storage/pg"
	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

var db *pg.DB

// TestMain starts one pgvector Postgres for the package and applies the real
// migrations. Without Docker the tests skip.
func TestMain(m *testing.M) {
	os.Exit(func() int {
		ctx := context.Background()
		ctr, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg17",
			tcpostgres.WithDatabase("feedback"),
			tcpostgres.WithUsername("feedback"),
			tcpostgres.WithPassword("feedback"),
			testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping Postgres tests: %v\n", err)
			return m.Run() // tests call requireDB and skip
		}
		defer func() { _ = testcontainers.TerminateContainer(ctr) }()

		dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cfg := postgres.Config{DSN: dsn}
		if err := postgres.Migrate(ctx, cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		pool, err := postgres.NewPool(ctx, cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer pool.Close()
		db = &pg.DB{Pool: pool}
		return m.Run()
	}())
}

func requireDB(t *testing.T) {
	t.Helper()
	if db == nil {
		t.Skip("Postgres not available (needs Docker)")
	}
	// Each test starts from empty tables.
	_, err := db.Pool.Exec(context.Background(), "TRUNCATE posts, votes, subscriptions")
	if err != nil {
		t.Fatal(err)
	}
}

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	install.Install(s)
	return s
}()

// strategy is a minimal create/update/delete strategy for tests.
type strategy struct {
	runtime.ObjectTyper
	names.NameGenerator
	unconditional bool
}

func (strategy) NamespaceScoped() bool                                            { return false }
func (strategy) PrepareForCreate(context.Context, runtime.Object)                 {}
func (strategy) PrepareForUpdate(context.Context, runtime.Object, runtime.Object) {}
func (strategy) Validate(context.Context, runtime.Object) field.ErrorList         { return nil }
func (strategy) ValidateUpdate(context.Context, runtime.Object, runtime.Object) field.ErrorList {
	return nil
}
func (strategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (strategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
func (strategy) Canonicalize(runtime.Object)                     {}
func (strategy) AllowCreateOnUpdate(context.Context) bool        { return false }
func (s strategy) AllowUnconditionalUpdate(context.Context) bool { return s.unconditional }

// postStatusStrategy only lets status change.
type postStatusStrategy struct{ strategy }

func (postStatusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	obj.(*v1.Post).Spec = old.(*v1.Post).Spec
}

func newStrategy(unconditional bool) strategy {
	return strategy{ObjectTyper: scheme, NameGenerator: names.SimpleNameGenerator, unconditional: unconditional}
}

func postStore(t *testing.T) *pg.Store {
	t.Helper()
	st := newStrategy(true)
	s, err := pg.NewStore(db, pg.Kind{
		Table:                     "posts",
		NewFunc:                   func() runtime.Object { return &v1.Post{} },
		NewListFunc:               func() runtime.Object { return &v1.PostList{} },
		DefaultQualifiedResource:  v1.Resource("posts"),
		SingularQualifiedResource: v1.Resource("post"),
		CreateStrategy:            st, UpdateStrategy: st, DeleteStrategy: st,
		Scope: pg.PlatformScoped,
		Columns: func(obj runtime.Object) []pg.Column {
			p := obj.(*v1.Post)
			return []pg.Column{
				{Name: "hidden", Value: p.Spec.Hidden},
				{Name: "state", Value: string(p.Status.State)},
			}
		},
		FieldColumns: map[string]string{"spec.hidden": "hidden", "status.state": "state"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func voteStore(t *testing.T, unconditional bool) *pg.Store {
	t.Helper()
	st := newStrategy(unconditional)
	s, err := pg.NewStore(db, pg.Kind{
		Table:                     "votes",
		NewFunc:                   func() runtime.Object { return &v1.Vote{} },
		NewListFunc:               func() runtime.Object { return &v1.VoteList{} },
		DefaultQualifiedResource:  v1.Resource("votes"),
		SingularQualifiedResource: v1.Resource("vote"),
		CreateStrategy:            st, UpdateStrategy: st, DeleteStrategy: st,
		Scope: pg.UserOwned,
		Columns: func(obj runtime.Object) []pg.Column {
			return []pg.Column{{Name: "post_name", Value: obj.(*v1.Vote).Spec.PostRef.Name}}
		},
		FieldColumns: map[string]string{"spec.postRef.name": "post_name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func subscriptionStore(t *testing.T) *pg.Store {
	t.Helper()
	st := newStrategy(true)
	s, err := pg.NewStore(db, pg.Kind{
		Table:                     "subscriptions",
		NewFunc:                   func() runtime.Object { return &v1.Subscription{} },
		NewListFunc:               func() runtime.Object { return &v1.SubscriptionList{} },
		DefaultQualifiedResource:  v1.Resource("subscriptions"),
		SingularQualifiedResource: v1.Resource("subscription"),
		CreateStrategy:            st, UpdateStrategy: st, DeleteStrategy: st,
		Scope: pg.UserOwned,
		Columns: func(obj runtime.Object) []pg.Column {
			sub := obj.(*v1.Subscription)
			return []pg.Column{{Name: "post_name", Value: sub.Spec.PostRef.Name}, {Name: "reason", Value: string(sub.Spec.Reason)}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// requestContext is what the apiserver passes a cluster-scoped request.
func requestContext() context.Context {
	return request.WithNamespace(context.Background(), "")
}

func asUser(uid string) context.Context {
	return request.WithUser(requestContext(), &user.DefaultInfo{
		Name: uid + "@example.com", UID: uid,
		Extra: map[string][]string{pg.ParentKindExtraKey: {"User"}, pg.ParentNameExtraKey: {uid}},
	})
}

func asPlatform() context.Context {
	return request.WithUser(requestContext(), &user.DefaultInfo{Name: "staff@datum.net", UID: "staff"})
}

func vote(post string) *v1.Vote {
	return &v1.Vote{ObjectMeta: metav1.ObjectMeta{Name: post}, Spec: v1.VoteSpec{PostRef: v1.PostReference{Name: post}}}
}

func post(name string, state v1.PostState, lbls map[string]string) *v1.Post {
	return &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls}, Status: v1.PostStatus{Title: name, State: state}}
}

func mustCreate(t *testing.T, ctx context.Context, s *pg.Store, obj runtime.Object) runtime.Object {
	t.Helper()
	out, err := s.Create(ctx, obj, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return out
}

func names_(t *testing.T, list runtime.Object) []string {
	t.Helper()
	var out []string
	switch l := list.(type) {
	case *v1.PostList:
		for _, i := range l.Items {
			out = append(out, i.Name)
		}
	case *v1.VoteList:
		for _, i := range l.Items {
			out = append(out, i.Name)
		}
	default:
		t.Fatalf("unexpected list %T", list)
	}
	return out
}

func equal(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func TestCreateGetDelete(t *testing.T) {
	requireDB(t)
	s, ctx := postStore(t), asPlatform()

	created := mustCreate(t, ctx, s, post("datum-cloud.enhancements.1", v1.PostStateBacklog, nil)).(*v1.Post)
	if created.ResourceVersion == "" || created.UID == "" || created.CreationTimestamp.IsZero() {
		t.Fatalf("system fields not set: %+v", created.ObjectMeta)
	}

	got, err := s.Get(ctx, created.Name, &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if g := got.(*v1.Post); g.ResourceVersion != created.ResourceVersion || g.Status.Title != created.Status.Title {
		t.Fatalf("get = %+v, want %+v", g, created)
	}

	if _, err := s.Create(ctx, post(created.Name, "", nil), nil, &metav1.CreateOptions{}); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("duplicate create: got %v, want AlreadyExists", err)
	}

	wrongUID := types.UID("nope")
	if _, _, err := s.Delete(ctx, created.Name, nil, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &wrongUID}}); !apierrors.IsConflict(err) {
		t.Fatalf("delete with wrong UID: got %v, want Conflict", err)
	}
	if _, _, err := s.Delete(ctx, created.Name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, created.Name, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("get after delete: got %v, want NotFound", err)
	}
}

func TestGenerateName(t *testing.T) {
	requireDB(t)
	s, ctx := postStore(t), asPlatform()
	out := mustCreate(t, ctx, s, &v1.Post{ObjectMeta: metav1.ObjectMeta{GenerateName: "gen-"}}).(*v1.Post)
	if len(out.Name) <= len("gen-") {
		t.Fatalf("name not generated: %q", out.Name)
	}
}

func TestUpdate(t *testing.T) {
	requireDB(t)
	s, ctx := postStore(t), asPlatform()
	p := mustCreate(t, ctx, s, post("p", v1.PostStateBacklog, nil)).(*v1.Post)

	// A no-op update keeps the resourceVersion.
	same, _, err := s.Update(ctx, "p", rest.DefaultUpdatedObjectInfo(p.DeepCopy()), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if same.(*v1.Post).ResourceVersion != p.ResourceVersion {
		t.Fatalf("no-op update bumped resourceVersion %s -> %s", p.ResourceVersion, same.(*v1.Post).ResourceVersion)
	}

	changed := p.DeepCopy()
	changed.Status.State = v1.PostStateInProgress
	out, created, err := s.Update(ctx, "p", rest.DefaultUpdatedObjectInfo(changed), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil || created {
		t.Fatalf("update: created=%v err=%v", created, err)
	}
	if out.(*v1.Post).ResourceVersion == p.ResourceVersion {
		t.Fatal("update did not bump resourceVersion")
	}

	// The extra column follows the object.
	var state string
	if err := db.Pool.QueryRow(ctx, "SELECT state FROM posts WHERE name = 'p'").Scan(&state); err != nil || state != "in-progress" {
		t.Fatalf("state column = %q, %v", state, err)
	}

	// Writing with the old resourceVersion conflicts.
	stale := p.DeepCopy()
	stale.Status.State = v1.PostStateShipped
	if _, _, err := s.Update(ctx, "p", rest.DefaultUpdatedObjectInfo(stale), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("stale update: got %v, want Conflict", err)
	}

	// Without a resourceVersion it applies, because the strategy allows
	// unconditional updates.
	stale.ResourceVersion = ""
	if _, _, err := s.Update(ctx, "p", rest.DefaultUpdatedObjectInfo(stale), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("unconditional update: %v", err)
	}

	if _, _, err := s.Update(ctx, "missing", rest.DefaultUpdatedObjectInfo(post("missing", "", nil)), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("update missing: got %v, want NotFound", err)
	}
	out, created, err = s.Update(ctx, "upserted", rest.DefaultUpdatedObjectInfo(post("upserted", "", nil)), nil, nil, true, &metav1.UpdateOptions{})
	if err != nil || !created || out.(*v1.Post).UID == "" {
		t.Fatalf("forced create on update: created=%v err=%v", created, err)
	}
}

func TestUnconditionalUpdateRejected(t *testing.T) {
	requireDB(t)
	s, ctx := voteStore(t, false), asUser("u1")
	v := mustCreate(t, ctx, s, vote("p")).(*v1.Vote)
	v.ResourceVersion = ""
	v.Spec.Source = v1.ClientPortal
	if _, _, err := s.Update(ctx, "p", rest.DefaultUpdatedObjectInfo(v), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsBadRequest(err) {
		t.Fatalf("got %v, want BadRequest", err)
	}
}

func TestStatusStore(t *testing.T) {
	requireDB(t)
	s, ctx := postStore(t), asPlatform()
	status := pg.NewStatusStore(s, postStatusStrategy{newStrategy(true)})
	p := mustCreate(t, ctx, s, post("p", v1.PostStateBacklog, nil)).(*v1.Post)

	edit := p.DeepCopy()
	edit.Spec.Hidden = true
	edit.Status.State = v1.PostStateShipped
	out, _, err := status.Update(ctx, "p", rest.DefaultUpdatedObjectInfo(edit), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*v1.Post); got.Spec.Hidden || got.Status.State != v1.PostStateShipped {
		t.Fatalf("status update: hidden=%v state=%s", got.Spec.Hidden, got.Status.State)
	}
}

func TestListSelectors(t *testing.T) {
	requireDB(t)
	s, ctx := postStore(t), asPlatform()
	mustCreate(t, ctx, s, post("a", v1.PostStateBacklog, map[string]string{"team": "edge", "tier": "1"}))
	mustCreate(t, ctx, s, post("b", v1.PostStateInProgress, map[string]string{"team": "core", "tier": "2"}))
	mustCreate(t, ctx, s, post("c", v1.PostStateInProgress, map[string]string{"team": "edge"}))
	mustCreate(t, ctx, s, post("d", v1.PostStateShipped, nil))

	cases := []struct {
		label, field string
		want         []string
	}{
		{want: []string{"a", "b", "c", "d"}},
		{label: "team=edge", want: []string{"a", "c"}},
		{label: "team in (core,edge),tier", want: []string{"a", "b"}},
		{label: "team!=edge", want: []string{"b", "d"}},
		{label: "team notin (edge)", want: []string{"b", "d"}},
		{label: "!tier", want: []string{"c", "d"}},
		{label: "tier>1", want: []string{"b"}}, // applied in Go
		{field: "status.state=in-progress", want: []string{"b", "c"}},
		{field: "status.state!=in-progress,metadata.name!=d", want: []string{"a"}},
		{label: "team=edge", field: "status.state=in-progress", want: []string{"c"}},
		{field: "spec.hidden=false", want: []string{"a", "b", "c", "d"}},
	}
	for _, tc := range cases {
		t.Run(tc.label+"|"+tc.field, func(t *testing.T) {
			opts := &metainternalversion.ListOptions{}
			if tc.label != "" {
				sel, err := labels.Parse(tc.label)
				if err != nil {
					t.Fatal(err)
				}
				opts.LabelSelector = sel
			}
			if tc.field != "" {
				sel, err := fields.ParseSelector(tc.field)
				if err != nil {
					t.Fatal(err)
				}
				opts.FieldSelector = sel
			}
			list, err := s.List(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := names_(t, list); !equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	_, err := s.List(ctx, &metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("status.title", "x")})
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("unsupported field: got %v, want BadRequest", err)
	}
}

func TestListPaging(t *testing.T) {
	requireDB(t)
	s, ctx := postStore(t), asPlatform()
	for _, n := range []string{"e", "a", "d", "b", "c"} {
		mustCreate(t, ctx, s, post(n, "", nil))
	}
	var all []string
	cont := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging did not end")
		}
		list, err := s.List(ctx, &metainternalversion.ListOptions{Limit: 2, Continue: cont})
		if err != nil {
			t.Fatal(err)
		}
		l := list.(*v1.PostList)
		if l.ResourceVersion == "" {
			t.Fatal("list has no resourceVersion")
		}
		all = append(all, names_(t, l)...)
		if cont = l.Continue; cont == "" {
			break
		}
	}
	if !equal(all, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("pages = %v", all)
	}
	if _, err := s.List(ctx, &metainternalversion.ListOptions{Continue: "!!"}); !apierrors.IsBadRequest(err) {
		t.Fatalf("bad continue: got %v, want BadRequest", err)
	}
}

func TestUserScopes(t *testing.T) {
	requireDB(t)
	s := voteStore(t, true)
	alice, bob := asUser("alice-uid"), asUser("bob-uid")

	mustCreate(t, alice, s, vote("p1"))
	mustCreate(t, alice, s, vote("p2"))
	mustCreate(t, bob, s, vote("p1")) // same name, different owner

	for ctx, want := range map[context.Context][]string{alice: {"p1", "p2"}, bob: {"p1"}} {
		list, err := s.List(ctx, &metainternalversion.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := names_(t, list); !equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if _, err := s.Get(bob, "p2", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("bob reading alice's vote: got %v, want NotFound", err)
	}
	if _, _, err := s.Delete(bob, "p2", nil, &metav1.DeleteOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("bob deleting alice's vote: got %v, want NotFound", err)
	}

	// Staff in the platform context see every user's votes.
	staff := asPlatform()
	list, err := s.List(staff, &metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.postRef.name", "p1")})
	if err != nil {
		t.Fatal(err)
	}
	if got := names_(t, list); !equal(got, []string{"p1", "p1"}) {
		t.Fatalf("platform list = %v", got)
	}
	if _, err := s.Get(staff, "p2", &metav1.GetOptions{}); err != nil {
		t.Fatalf("platform get of a unique name: %v", err)
	}
	if _, err := s.Get(staff, "p1", &metav1.GetOptions{}); !apierrors.IsBadRequest(err) {
		t.Fatalf("platform get of an ambiguous name: got %v, want BadRequest", err)
	}
	if _, err := s.Create(staff, vote("p3"), nil, &metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("platform create: got %v, want Forbidden", err)
	}

	org := request.WithUser(requestContext(), &user.DefaultInfo{Name: "x", Extra: map[string][]string{
		pg.ParentKindExtraKey: {"Organization"}, pg.ParentNameExtraKey: {"acme"}}})
	if _, err := s.List(org, &metainternalversion.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("organization context: got %v, want Forbidden", err)
	}
}

func TestTransactions(t *testing.T) {
	requireDB(t)
	votes, subs := voteStore(t, true), subscriptionStore(t)
	ctx := asUser("u1")

	// A vote and its subscription commit together...
	err := db.InTx(ctx, func(ctx context.Context) error {
		mustCreate(t, ctx, votes, vote("p1"))
		_, err := subs.Create(ctx, &v1.Subscription{ObjectMeta: metav1.ObjectMeta{Name: "p1"},
			Spec: v1.SubscriptionSpec{PostRef: v1.PostReference{Name: "p1"}, Reason: v1.SubscriptionReasonVoted}}, nil, &metav1.CreateOptions{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// ...and roll back together.
	boom := errors.New("boom")
	err = db.InTx(ctx, func(ctx context.Context) error {
		mustCreate(t, ctx, votes, vote("p2"))
		mustCreate(t, ctx, subs, &v1.Subscription{ObjectMeta: metav1.ObjectMeta{Name: "p2"},
			Spec: v1.SubscriptionSpec{PostRef: v1.PostReference{Name: "p2"}}})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	for _, s := range []*pg.Store{votes, subs} {
		if _, err := s.Get(ctx, "p1", &metav1.GetOptions{}); err != nil {
			t.Fatalf("committed row missing: %v", err)
		}
		if _, err := s.Get(ctx, "p2", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("rolled-back row present: %v", err)
		}
	}

	// A failing nested write rolls back only its savepoint when the caller
	// handles the error.
	err = db.InTx(ctx, func(ctx context.Context) error {
		mustCreate(t, ctx, votes, vote("p3"))
		if _, err := votes.Create(ctx, vote("p3"), nil, &metav1.CreateOptions{}); !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("want AlreadyExists, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := votes.Get(ctx, "p3", &metav1.GetOptions{}); err != nil {
		t.Fatalf("outer write lost after nested failure: %v", err)
	}
}

func TestDryRun(t *testing.T) {
	requireDB(t)
	s, ctx := postStore(t), asPlatform()
	dry := []string{metav1.DryRunAll}

	out, err := s.Create(ctx, post("dry", "", nil), nil, &metav1.CreateOptions{DryRun: dry})
	if err != nil || out.(*v1.Post).UID == "" {
		t.Fatalf("dry-run create: %v", err)
	}
	if _, err := s.Get(ctx, "dry", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("dry-run create persisted: %v", err)
	}

	p := mustCreate(t, ctx, s, post("real", v1.PostStateBacklog, nil)).(*v1.Post)
	p.Status.State = v1.PostStateShipped
	if _, _, err := s.Update(ctx, "real", rest.DefaultUpdatedObjectInfo(p), nil, nil, false, &metav1.UpdateOptions{DryRun: dry}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Delete(ctx, "real", nil, &metav1.DeleteOptions{DryRun: dry}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "real", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("dry-run delete removed the object: %v", err)
	}
	if got.(*v1.Post).Status.State != v1.PostStateBacklog {
		t.Fatal("dry-run update persisted")
	}
}
