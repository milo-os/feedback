package release

import (
	"os"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/registry/post"
	"go.miloapis.com/feedback/internal/storage/pg/pgtest"
	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var (
	ids      = identity.Identities{Sync: []string{"sync"}}
	syncCtx  = pgtest.AsPlatform("sync")
	staffCtx = pgtest.AsPlatform("staff@datum.net")
	userCtx  = pgtest.AsUser("u1")
)

type stores struct {
	posts     *post.REST
	releases  *REST
	changelog *REST
}

func setup(t *testing.T) stores {
	t.Helper()
	db := pgtest.Require(t, "posts", "releases", "changelog_entries")
	scheme := runtime.NewScheme()
	install.Install(scheme)
	p, err := post.NewREST(db, scheme, ids)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReleaseREST(db, scheme, ids)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewChangelogREST(db, scheme, ids)
	if err != nil {
		t.Fatal(err)
	}
	return stores{p, r, c}
}

// addPost creates datum-cloud/enhancements#n on a release, optionally hidden.
func (s stores) addPost(t *testing.T, n int32, release string, hidden bool) string {
	t.Helper()
	p := &v1.Post{
		ObjectMeta: metav1.ObjectMeta{Name: v1.PostName("datum-cloud", "enhancements", n)},
		Status: v1.PostStatus{Title: "p", Release: release,
			GitHub: &v1.GitHubIssue{Org: "datum-cloud", Repository: "enhancements", Number: n, State: "open"}},
	}
	out, err := s.posts.Create(syncCtx, p, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hidden {
		p = out.(*v1.Post)
		p.Spec.Hidden = true
		if _, _, err := s.posts.Update(staffCtx, p.Name, rest.DefaultUpdatedObjectInfo(p), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return p.Name
}

func TestReleasePostsDerived(t *testing.T) {
	s := setup(t)
	a := s.addPost(t, 1, "hedy-2-0", false)
	b := s.addPost(t, 2, "hedy-2-0", false)
	s.addPost(t, 3, "hedy-2-0", true) // hidden
	s.addPost(t, 4, "other", false)

	rel := &v1.Release{ObjectMeta: metav1.ObjectMeta{Name: "hedy-2-0"},
		Status: v1.ReleaseStatus{Title: "Hedy: 2.0", DueOn: "2026-10-31", Posts: []string{"stale.from.sync"}}}
	if _, err := s.releases.Create(staffCtx, rel.DeepCopy(), nil, &metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("staff create: got %v, want Forbidden", err)
	}
	out, err := s.releases.Create(syncCtx, rel, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{a, b}
	if got := out.(*v1.Release).Status.Posts; !equal(got, want) {
		t.Fatalf("create returned posts %v, want %v", got, want)
	}
	got, err := s.releases.Get(userCtx, "hedy-2-0", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p := got.(*v1.Release).Status.Posts; !equal(p, want) {
		t.Fatalf("user get posts %v, want %v", p, want)
	}
	list, err := s.releases.List(userCtx, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p := list.(*v1.ReleaseList).Items[0].Status.Posts; !equal(p, want) {
		t.Fatalf("list posts %v, want %v", p, want)
	}

	bad := &v1.Release{ObjectMeta: metav1.ObjectMeta{Name: "bad"}, Status: v1.ReleaseStatus{DueOn: "31/10/2026"}}
	if _, err := s.releases.Create(syncCtx, bad, nil, &metav1.CreateOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid release: got %v, want Invalid", err)
	}
}

func TestChangelogPostsDerived(t *testing.T) {
	s := setup(t)
	a := s.addPost(t, 913, "", false)
	s.addPost(t, 914, "", true) // hidden

	entry := &v1.ChangelogEntry{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.datum.303"},
		Status: v1.ChangelogEntryStatus{Title: "ALB wildcard hostnames", PublishedAt: metav1.Now(),
			IssueRefs: []string{"datum-cloud/enhancements#913", "datum-cloud/enhancements#914", "datum-cloud/compute#311"}}}
	out, err := s.changelog.Create(syncCtx, entry, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*v1.ChangelogEntry).Status.Posts; !equal(got, []string{a}) {
		t.Fatalf("posts = %v, want [%s]", got, a)
	}
	if refs := out.(*v1.ChangelogEntry).Status.IssueRefs; len(refs) != 3 {
		t.Fatalf("issueRefs changed: %v", refs)
	}

	entry = out.(*v1.ChangelogEntry)
	entry.Status.Title = "staff edit"
	if _, _, err := s.changelog.Update(staffCtx, entry.Name, rest.DefaultUpdatedObjectInfo(entry), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("staff update: got %v, want Forbidden", err)
	}

	bad := &v1.ChangelogEntry{ObjectMeta: metav1.ObjectMeta{Name: "bad"},
		Status: v1.ChangelogEntryStatus{Title: "x", PublishedAt: metav1.Now(), IssueRefs: []string{"#913"}}}
	if _, err := s.changelog.Create(syncCtx, bad, nil, &metav1.CreateOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("bad issue ref: got %v, want Invalid", err)
	}
}

func TestPostNameForRef(t *testing.T) {
	for ref, want := range map[string]string{
		"datum-cloud/enhancements#913": "datum-cloud.enhancements.913",
		"Milo-OS/Search#7":             "milo-os.search.7",
		"datum-cloud/datum.net#12":     "datum-cloud.datum.net.12",
		"#913":                         "",
		"datum-cloud/enhancements#0":   "",
	} {
		got, ok := PostNameForRef(ref)
		if got != want || ok != (want != "") {
			t.Errorf("%s: got %q %v, want %q", ref, got, ok, want)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
