package post

import (
	"context"
	"os"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/feedback/internal/storage/pg"
	"go.miloapis.com/feedback/internal/storage/pg/pgtest"
	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var (
	syncCtx  = pgtest.AsPlatform("sync")
	ctrlCtx  = pgtest.AsPlatform("controller")
	staffCtx = pgtest.AsPlatform("staff@datum.net")
	userCtx  = pgtest.AsUser("u1")
)

func setup(t *testing.T) (*pg.DB, *REST) {
	t.Helper()
	db := pgtest.Require(t, "posts", "votes", "subscriptions", "deleted_posts", "post_embeddings")
	scheme := runtime.NewScheme()
	install.Install(scheme)
	r, err := NewREST(db, scheme, ids)
	if err != nil {
		t.Fatal(err)
	}
	return db, r
}

func newPost(number int32, state v1.PostState) *v1.Post {
	return &v1.Post{
		ObjectMeta: metav1.ObjectMeta{Name: v1.PostName("datum-cloud", "enhancements", number)},
		Status: v1.PostStatus{
			Title: "Post", BodyHTML: "<p>body</p>", State: state,
			GitHub: &v1.GitHubIssue{Org: "datum-cloud", Repository: "enhancements", Number: number, State: "open",
				URL: "https://github.com/datum-cloud/enhancements/issues/1"},
			Votes: v1.PostVotes{GitHub: 4, Datum: 50, Total: 50},
		},
	}
}

func addVote(t *testing.T, db *pg.DB, userUID, postName string) {
	t.Helper()
	_, err := db.Pool.Exec(context.Background(), `INSERT INTO votes
		(scope_type, scope_name, name, uid, resource_version, created_at, object, post_name)
		VALUES ('User', $1, $2, gen_random_uuid(), nextval('resource_version_seq'), now(), '{}', $2)`, userUID, postName)
	if err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, r *REST, ctx context.Context, name string) *v1.Post {
	t.Helper()
	obj, err := r.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return obj.(*v1.Post)
}

func update(r rest.Updater, ctx context.Context, p *v1.Post) (*v1.Post, error) {
	out, _, err := r.Update(ctx, p.Name, rest.DefaultUpdatedObjectInfo(p), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return out.(*v1.Post), nil
}

func TestOnlySyncCreatesAndDeletes(t *testing.T) {
	_, r := setup(t)
	if _, err := r.Create(staffCtx, newPost(1, ""), nil, &metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("staff create: got %v, want Forbidden", err)
	}
	if _, err := r.Create(syncCtx, newPost(1, ""), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Delete(staffCtx, newPost(1, "").Name, nil, &metav1.DeleteOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("staff delete: got %v, want Forbidden", err)
	}

	wrong := newPost(2, "")
	wrong.Name = "datum-cloud.enhancements.3"
	if _, err := r.Create(syncCtx, wrong, nil, &metav1.CreateOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("name not matching github: got %v, want Invalid", err)
	}
}

func TestVotesComputed(t *testing.T) {
	db, r := setup(t)
	name := newPost(1, "").Name
	addVote(t, db, "u1", name)
	addVote(t, db, "u2", name)

	created, err := r.Create(syncCtx, newPost(1, ""), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v := created.(*v1.Post).Status.Votes; v.Datum != 2 || v.GitHub != 4 || v.Total != 6 {
		t.Fatalf("votes on create = %+v, want datum 2 github 4 total 6", v)
	}
	var col int32
	if err := db.Pool.QueryRow(context.Background(), "SELECT votes_total FROM posts WHERE name = $1", name).Scan(&col); err != nil || col != 6 {
		t.Fatalf("votes_total column = %d, %v", col, err)
	}

	// A sync update with a new 👍 count recomputes the total.
	p := get(t, r, syncCtx, name)
	p.Status.Votes.GitHub = 10
	out, err := update(r.Status, syncCtx, p)
	if err != nil {
		t.Fatal(err)
	}
	if v := out.Status.Votes; v.Total != 12 {
		t.Fatalf("votes after sync update = %+v", v)
	}
}

func TestStaffHideAndForbiddenStatus(t *testing.T) {
	_, r := setup(t)
	created, _ := r.Create(syncCtx, newPost(1, v1.PostStateBacklog), nil, &metav1.CreateOptions{})
	name := created.(*v1.Post).Name

	p := get(t, r, staffCtx, name)
	p.Spec.Hidden = true
	p.Status.Title = "staff edit"
	out, err := update(r, staffCtx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Spec.Hidden || out.Status.Title != "Post" {
		t.Fatalf("staff main update: hidden=%v title=%q", out.Spec.Hidden, out.Status.Title)
	}

	p = get(t, r, staffCtx, name)
	p.Status.State = v1.PostStateShipped
	if _, err := update(r.Status, staffCtx, p); !apierrors.IsInvalid(err) {
		t.Fatalf("staff status update: got %v, want Invalid (forbidden field)", err)
	}
}

func TestSyncAndControllerOwnership(t *testing.T) {
	_, r := setup(t)
	created, _ := r.Create(syncCtx, newPost(1, v1.PostStateBacklog), nil, &metav1.CreateOptions{})
	name := created.(*v1.Post).Name

	// The controller sets the stage.
	p := get(t, r, ctrlCtx, name)
	p.Status.Delivery = &v1.DeliveryStatus{Stage: v1.DeliveryStagePlanned, HighestNotified: v1.DeliveryStagePlanned,
		History: []v1.DeliveryEvent{{Stage: v1.DeliveryStagePlanned, At: metav1.Now()}}}
	p.Status.Title = "controller edit"
	if _, err := update(r.Status, ctrlCtx, p); err != nil {
		t.Fatal(err)
	}

	// Sync refreshes work items and tries to reset the stage.
	p = get(t, r, syncCtx, name)
	p.Status.State = v1.PostStateInProgress
	p.Status.Delivery.Stage = v1.DeliveryStageRequested
	p.Status.Delivery.Work = &v1.DeliveryWork{Total: 1, Items: []v1.WorkItem{{Kind: "issue", Ref: "datum-cloud/compute#9", State: "open"}}}
	if _, err := update(r.Status, syncCtx, p); err != nil {
		t.Fatal(err)
	}

	got := get(t, r, staffCtx, name)
	if got.Status.Title != "Post" || got.Status.State != v1.PostStateInProgress {
		t.Errorf("sync fields: title=%q state=%s", got.Status.Title, got.Status.State)
	}
	if d := got.Status.Delivery; d.Stage != v1.DeliveryStagePlanned || len(d.History) != 1 || d.Work == nil || d.Work.Total != 1 {
		t.Errorf("delivery = %+v", d)
	}
}

func TestHiddenOutsidePlatform(t *testing.T) {
	_, r := setup(t)
	visible, _ := r.Create(syncCtx, newPost(1, v1.PostStateBacklog), nil, &metav1.CreateOptions{})
	hidden, _ := r.Create(syncCtx, newPost(2, v1.PostStateBacklog), nil, &metav1.CreateOptions{})
	h := hidden.(*v1.Post)
	h.Spec.Hidden = true
	if _, err := update(r, staffCtx, h); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(userCtx, h.Name, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("user get hidden: got %v, want NotFound", err)
	}
	list, err := r.List(userCtx, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if items := list.(*v1.PostList).Items; len(items) != 1 || items[0].Name != visible.(*v1.Post).Name {
		t.Fatalf("user list = %d items", len(items))
	}
	list, _ = r.List(staffCtx, &metainternalversion.ListOptions{})
	if n := len(list.(*v1.PostList).Items); n != 2 {
		t.Fatalf("platform list = %d items, want 2", n)
	}
	list, _ = r.List(staffCtx, &metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.hidden", "false")})
	if n := len(list.(*v1.PostList).Items); n != 1 {
		t.Fatalf("platform list spec.hidden=false = %d items, want 1", n)
	}
}

func TestFieldSelectors(t *testing.T) {
	_, r := setup(t)
	r.Create(syncCtx, newPost(1, v1.PostStateBacklog), nil, &metav1.CreateOptions{})
	r.Create(syncCtx, newPost(2, v1.PostStateInProgress), nil, &metav1.CreateOptions{})
	other := newPost(3, v1.PostStateInProgress)
	other.Name = v1.PostName("milo-os", "search", 3)
	other.Status.GitHub.Org, other.Status.GitHub.Repository = "milo-os", "search"
	if _, err := r.Create(syncCtx, other, nil, &metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for sel, want := range map[string]int{
		"status.state=in-progress":                           2,
		"status.github.org=milo-os":                          1,
		"status.state=in-progress,status.github.org=milo-os": 1,
		"status.github.repository=enhancements":              2,
	} {
		fs, _ := fields.ParseSelector(sel)
		list, err := r.List(staffCtx, &metainternalversion.ListOptions{FieldSelector: fs})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(list.(*v1.PostList).Items); n != want {
			t.Errorf("%s: %d items, want %d", sel, n, want)
		}
	}
}

func TestEmbeddingInvalidated(t *testing.T) {
	db, r := setup(t)
	created, _ := r.Create(syncCtx, newPost(1, ""), nil, &metav1.CreateOptions{})
	name := created.(*v1.Post).Name
	embed := func() {
		_, err := db.Pool.Exec(context.Background(),
			"INSERT INTO post_embeddings (post_name, model, content_hash, embedding) VALUES ($1, 'm', 'h', '[1,2,3]')", name)
		if err != nil {
			t.Fatal(err)
		}
	}
	has := func() bool {
		var n int
		_ = db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM post_embeddings WHERE post_name = $1", name).Scan(&n)
		return n == 1
	}

	embed()
	p := get(t, r, syncCtx, name)
	p.Status.State = v1.PostStateShipped
	update(r.Status, syncCtx, p)
	if !has() {
		t.Fatal("state change dropped the embedding")
	}
	p = get(t, r, syncCtx, name)
	p.Status.Title = "Renamed"
	update(r.Status, syncCtx, p)
	if has() {
		t.Fatal("title change kept a stale embedding")
	}
}

func TestVoteRetention(t *testing.T) {
	db, r := setup(t)
	ctx := context.Background()
	keep, gone := newPost(1, ""), newPost(2, "")
	for _, p := range []*v1.Post{keep, gone} {
		if _, err := r.Create(syncCtx, p.DeepCopy(), nil, &metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		addVote(t, db, "u1", p.Name)
		if _, _, err := r.Delete(syncCtx, p.Name, nil, &metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// One was deleted long ago.
	if _, err := db.Pool.Exec(ctx, "UPDATE deleted_posts SET deleted_at = now() - interval '31 days' WHERE name = $1", gone.Name); err != nil {
		t.Fatal(err)
	}

	n, err := PurgeExpiredVotes(ctx, db, 30*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purged %d votes, %v; want 1", n, err)
	}
	var votes int
	db.Pool.QueryRow(ctx, "SELECT count(*) FROM votes").Scan(&votes)
	if votes != 1 {
		t.Fatalf("%d votes left, want 1", votes)
	}

	// The kept post comes back with its vote.
	created, err := r.Create(syncCtx, keep.DeepCopy(), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v := created.(*v1.Post).Status.Votes.Datum; v != 1 {
		t.Fatalf("returned post has %d Datum votes, want 1", v)
	}
	var tracked int
	db.Pool.QueryRow(ctx, "SELECT count(*) FROM deleted_posts").Scan(&tracked)
	if tracked != 0 {
		t.Fatalf("deleted_posts still has %d rows", tracked)
	}
}
