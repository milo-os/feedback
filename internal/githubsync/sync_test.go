package githubsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"go.miloapis.com/feedback/internal/github"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

var (
	envOnce sync.Once
	envCfg  *envtest.Environment
	envErr  error
	scheme  = runtime.NewScheme()
)

func init() {
	if err := v1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if envCfg != nil {
		_ = envCfg.Stop()
	}
	os.Exit(code)
}

// milo returns a client for an envtest API server with the CRDs, shared by
// the tests in this package. Each test uses its own post names.
func milo(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run with task test")
	}
	envOnce.Do(func() {
		envCfg = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
		}
		_, envErr = envCfg.Start()
	})
	if envErr != nil {
		t.Fatalf("start API server: %v", envErr)
	}
	c, err := client.New(envCfg.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeGitHub serves issues from a map keyed "org/repo#number". A missing key
// is not found.
type fakeGitHub struct {
	mu       sync.Mutex
	issues   map[string]*github.Issue
	projects map[string][]github.ProjectItem // by issue ID, for ProjectItems
	reads    []string                        // orgs whose installation ProjectItems used
	refused  map[string]bool                 // issue IDs whose project items GitHub refuses
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{issues: map[string]*github.Issue{}, projects: map[string][]github.ProjectItem{}}
}

func key(org, repo string, n int32) string {
	return strings.ToLower(fmt.Sprintf("%s/%s#%d", org, repo, n))
}

func (f *fakeGitHub) put(is *github.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *is
	f.issues[key(is.Org, is.Repo, is.Number)] = &cp
}

func (f *fakeGitHub) Issue(_ context.Context, installationID int64, owner, repo string, number int32, _ string) (*github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if installationID == 0 {
		return nil, fmt.Errorf("no installation")
	}
	is, ok := f.issues[key(owner, repo, number)]
	if !ok {
		return nil, github.ErrNotFound
	}
	cp := *is
	cp.Labels = slices.Clone(is.Labels)
	return &cp, nil
}

func (f *fakeGitHub) ProjectItems(_ context.Context, org string, ids []string, _ string) (map[string][]github.ProjectItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, org)
	out := map[string][]github.ProjectItem{}
	var inc github.IncompleteError
	for _, id := range ids {
		if f.refused[id] {
			inc.IDs = append(inc.IDs, id)
			inc.Missing++
		} else if items, ok := f.projects[id]; ok {
			out[id] = items
		}
	}
	if inc.Missing > 0 {
		return out, &inc
	}
	return out, nil
}

func testSource() *v1.FeedbackSource {
	return &v1.FeedbackSource{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud"}, Spec: *testSpec()}
}

// testIssue is a matching issue in a repository unique to the test, so
// tests sharing the API server don't share posts.
func testIssue(t *testing.T, n int32) *github.Issue {
	repo := strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	created := time.Date(2026, 9, 30, 14, 12, 0, 0, time.UTC)
	return &github.Issue{
		ID: fmt.Sprintf("I_%s_%d", repo, n), Org: "datum-cloud", Repo: repo, Number: n,
		URL:   fmt.Sprintf("https://github.com/datum-cloud/%s/issues/%d", repo, n),
		Title: "Bare Metal Infrastructure service",
		BodyHTML: `<h3>High-Level Summary</h3><p onclick="x()">This <a href="https://example.com">enhancement</a></p>` +
			`<script>alert(1)</script>`,
		BodyText:   "High-Level Summary\nThis enhancement",
		State:      "open",
		IssueType:  "Enhancement",
		Labels:     []string{"Deliver", "Build"},
		Visibility: "public",
		CreatedAt:  created,
		UpdatedAt:  created.Add(time.Hour),
		ProjectItems: []github.ProjectItem{
			{Owner: "datum-cloud", Number: 22, Status: "In progress"},
		},
	}
}

func newSyncer(c client.Client, gh GitHub) *Syncer {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	return &Syncer{Client: c, GitHub: gh, Now: func() time.Time { return now }}
}

func getPost(t *testing.T, c client.Client, name string) *v1.Post {
	t.Helper()
	var p v1.Post
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &p); err != nil {
		t.Fatalf("get post %s: %v", name, err)
	}
	return &p
}

func wantGone(t *testing.T, c client.Client, name string) {
	t.Helper()
	var p v1.Post
	err := c.Get(context.Background(), types.NamespacedName{Name: name}, &p)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("post %s: got %v, want it deleted", name, err)
	}
}

func syncIssue(t *testing.T, s *Syncer, src *v1.FeedbackSource, is *github.Issue, want Result) {
	t.Helper()
	got, err := s.SyncIssue(context.Background(), src, is.Repo, is.Number)
	if err != nil {
		t.Fatalf("SyncIssue(%s#%d): %v", is.Repo, is.Number, err)
	}
	if got != want {
		t.Fatalf("SyncIssue(%s#%d) = %s, want %s", is.Repo, is.Number, got, want)
	}
}

func TestSyncCreatesThenSkipsNoOps(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	s := newSyncer(c, gh)
	src := testSource()
	is := testIssue(t, 912)
	gh.put(is)
	name := v1.PostName(is.Org, is.Repo, is.Number)

	syncIssue(t, s, src, is, Created)
	p := getPost(t, c, name)
	wantLabels := map[string]string{
		v1.LabelOrg:                        "datum-cloud",
		v1.LabelState:                      "in-progress",
		v1.CategoryLabelPrefix + "build":   "",
		v1.CategoryLabelPrefix + "deliver": "",
	}
	if !sameLabels(p.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", p.Labels, wantLabels)
	}
	st := p.Status
	if st.Title != is.Title || st.State != v1.PostStateInProgress || st.IssueType != "Enhancement" ||
		!slices.Equal(st.Categories, []string{"build", "deliver"}) || !slices.Equal(st.Labels, []string{"Build", "Deliver"}) {
		t.Errorf("status = %+v", st)
	}
	if strings.Contains(st.BodyHTML, "script") || strings.Contains(st.BodyHTML, "onclick") || !strings.Contains(st.BodyHTML, "<h3>High-Level Summary</h3>") {
		t.Errorf("bodyHTML not sanitised: %q", st.BodyHTML)
	}
	if st.Summary != "High-Level Summary This enhancement" {
		t.Errorf("summary = %q", st.Summary)
	}
	if gh := st.GitHub; gh == nil || gh.Org != "datum-cloud" || gh.Repository != is.Repo || gh.Number != 912 ||
		gh.URL != is.URL || gh.State != "open" || !gh.CreatedAt.Equal(&metav1.Time{Time: is.CreatedAt}) {
		t.Errorf("status.github = %+v", st.GitHub)
	}
	if st.LastSyncedAt == nil {
		t.Error("lastSyncedAt not set")
	}
	for _, m := range p.ManagedFields {
		if m.Manager != FieldManager {
			t.Errorf("field manager %q wrote the post; only %q should", m.Manager, FieldManager)
		}
	}

	rv := p.ResourceVersion
	syncIssue(t, s, src, is, Unchanged)
	if got := getPost(t, c, name).ResourceVersion; got != rv {
		t.Errorf("a no-op sync wrote the post: resourceVersion %s -> %s", rv, got)
	}
}

func TestSyncUpdates(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	s := newSyncer(c, gh)
	src := testSource()
	is := testIssue(t, 1)
	gh.put(is)
	name := v1.PostName(is.Org, is.Repo, is.Number)
	syncIssue(t, s, src, is, Created)

	// The operator writes votes and a stage label; staff hide the post.
	ctx := context.Background()
	votes := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1.SchemeGroupVersion.String(), "kind": "Post",
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"votes": map[string]any{"total": int64(9), "datum": int64(6)}},
	}}
	if err := c.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(votes), client.FieldOwner("feedback-operator"), client.ForceOwnership); err != nil {
		t.Fatal(err)
	}
	stage := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1.SchemeGroupVersion.String(), "kind": "Post",
		"metadata": map[string]any{"name": name, "labels": map[string]any{v1.LabelStage: "picked-up"}},
	}}
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(stage), client.FieldOwner("feedback-operator"), client.ForceOwnership); err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(ctx, &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: name}}, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"hidden":true}}`)), client.FieldOwner("staff")); err != nil {
		t.Fatal(err)
	}

	// The issue moves to Done, loses Deliver and gains a label, its title
	// changes, and its type is removed.
	is.ProjectItems[0].Status = "Done"
	is.Labels = []string{"Build", "help wanted"}
	is.Title = "Bare metal"
	is.IssueType = ""
	gh.put(is)
	syncIssue(t, s, src, is, Updated)

	p := getPost(t, c, name)
	wantLabels := map[string]string{
		v1.LabelOrg:                      "datum-cloud",
		v1.LabelState:                    "shipped",
		v1.CategoryLabelPrefix + "build": "",
		v1.LabelStage:                    "picked-up",
	}
	if !sameLabels(p.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", p.Labels, wantLabels)
	}
	st := p.Status
	if st.State != v1.PostStateShipped || st.Title != "Bare metal" || st.IssueType != "" ||
		!slices.Equal(st.Categories, []string{"build"}) || !slices.Equal(st.Labels, []string{"Build", "help wanted"}) {
		t.Errorf("status = %+v", st)
	}
	if st.Votes.Total != 9 || st.Votes.Datum != 6 {
		t.Errorf("the operator's votes were lost: %+v", st.Votes)
	}
	if !p.Spec.Hidden {
		t.Error("staff's spec.hidden was lost")
	}

	// The last category goes: its label and status.categories go with it.
	is.Labels = nil
	is.IssueType = "Bug"
	gh.put(is)
	syncIssue(t, s, src, is, Updated)
	p = getPost(t, c, name)
	if _, ok := p.Labels[v1.CategoryLabelPrefix+"build"]; ok || len(p.Status.Categories) != 0 || len(p.Status.Labels) != 0 {
		t.Errorf("categories not removed: labels %v, status %+v", p.Labels, p.Status)
	}
	if p.Status.IssueType != "Bug" || p.Status.Votes.Total != 9 || p.Labels[v1.LabelStage] != "picked-up" {
		t.Errorf("after removing categories: %+v, labels %v", p.Status, p.Labels)
	}
}

// TestSyncStopsMatching covers each way an issue stops being a post.
func TestSyncStopsMatching(t *testing.T) {
	tests := []struct {
		name   string
		change func(gh *fakeGitHub, is *github.Issue, src *v1.FeedbackSource)
	}{
		{"included label removed", func(gh *fakeGitHub, is *github.Issue, _ *v1.FeedbackSource) {
			is.IssueType, is.Labels = "", []string{"help wanted"}
			gh.put(is)
		}},
		{"issue type changed", func(gh *fakeGitHub, is *github.Issue, _ *v1.FeedbackSource) {
			is.IssueType, is.Labels = "Task", nil
			gh.put(is)
		}},
		{"excluded label added", func(gh *fakeGitHub, is *github.Issue, _ *v1.FeedbackSource) {
			is.Labels = append(is.Labels, "feedback:hide")
			gh.put(is)
		}},
		{"repository made private", func(gh *fakeGitHub, is *github.Issue, _ *v1.FeedbackSource) {
			is.Visibility = "private"
			gh.put(is)
		}},
		{"repository excluded", func(_ *fakeGitHub, is *github.Issue, src *v1.FeedbackSource) {
			src.Spec.GitHub.Repositories.Exclude = append(src.Spec.GitHub.Repositories.Exclude, is.Repo)
		}},
		{"issue deleted", func(gh *fakeGitHub, is *github.Issue, _ *v1.FeedbackSource) {
			gh.mu.Lock()
			delete(gh.issues, key(is.Org, is.Repo, is.Number))
			gh.mu.Unlock()
		}},
		{"repository deleted or hidden from the app", func(gh *fakeGitHub, is *github.Issue, _ *v1.FeedbackSource) {
			gh.mu.Lock()
			gh.issues = map[string]*github.Issue{}
			gh.mu.Unlock()
		}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := milo(t)
			gh := newFakeGitHub()
			s := newSyncer(c, gh)
			src := testSource()
			is := testIssue(t, int32(i+1))
			gh.put(is)
			name := v1.PostName(is.Org, is.Repo, is.Number)
			syncIssue(t, s, src, is, Created)

			tt.change(gh, is, src)
			syncIssue(t, s, src, is, Deleted)
			wantGone(t, c, name)
			syncIssue(t, s, src, is, Skipped)
		})
	}
}

func TestSyncTransferredIssue(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	s := newSyncer(c, gh)
	src := testSource()
	is := testIssue(t, 5)
	gh.put(is)
	old := v1.PostName(is.Org, is.Repo, is.Number)
	syncIssue(t, s, src, is, Created)

	// GitHub answers for the old location from the new one.
	moved := *is
	moved.Repo, moved.Number = is.Repo+"-new", 77
	gh.mu.Lock()
	gh.issues[key(is.Org, is.Repo, is.Number)] = &moved
	gh.mu.Unlock()
	syncIssue(t, s, src, is, Moved)

	target := v1.PostName(moved.Org, moved.Repo, moved.Number)
	if p := getPost(t, c, target); p.Status.GitHub.Number != 77 {
		t.Errorf("new post = %+v", p.Status.GitHub)
	}
	// The old post stays, merged into the new one, so the operator moves its
	// votes and follows there.
	p := getPost(t, c, old)
	if p.Status.MergedInto != target || p.Status.State != v1.PostStateClosed || p.Labels[v1.LabelState] != "closed" {
		t.Errorf("old post: mergedInto %q, state %q, labels %v; want merged into %s and closed", p.Status.MergedInto, p.Status.State, p.Labels, target)
	}
	if p.Status.Title != is.Title || p.Status.GitHub.Number != 5 {
		t.Errorf("old post lost what it said about the issue: %+v", p.Status)
	}
	rv := p.ResourceVersion
	syncIssue(t, s, src, is, Unchanged)
	if got := getPost(t, c, old).ResourceVersion; got != rv {
		t.Error("syncing a transferred issue again rewrote its old post")
	}

	// Transferred back: the post is the issue's again, no longer merged.
	gh.mu.Lock()
	gh.issues[key(is.Org, is.Repo, is.Number)] = is
	gh.mu.Unlock()
	syncIssue(t, s, src, is, Updated)
	if p := getPost(t, c, old); p.Status.MergedInto != "" || p.Status.State != v1.PostStateInProgress {
		t.Errorf("after moving back: mergedInto %q, state %q", p.Status.MergedInto, p.Status.State)
	}
}

// TestSyncTransferredToAnotherOrg: a source never touches another org's
// post, even when an issue moves there.
func TestSyncTransferredToAnotherOrg(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	s := newSyncer(c, gh)
	miloOS := testSource()
	miloOS.Spec.GitHub.Org = "milo-os"
	is := testIssue(t, 5)
	is.Org = "milo-os"
	gh.put(is)
	syncIssue(t, s, miloOS, is, Created)

	// The issue moves to datum-cloud, whose source has synced it already.
	moved := *is
	moved.Org, moved.Number = "datum-cloud", 7
	gh.put(&moved)
	syncIssue(t, s, testSource(), &moved, Created)
	target := v1.PostName(moved.Org, moved.Repo, moved.Number)
	rv := getPost(t, c, target).ResourceVersion

	gh.mu.Lock()
	gh.issues[key(is.Org, is.Repo, is.Number)] = &moved
	gh.mu.Unlock()
	syncIssue(t, s, miloOS, is, Moved)
	if got := getPost(t, c, target).ResourceVersion; got != rv {
		t.Error("the milo-os source wrote datum-cloud's post")
	}
	if p := getPost(t, c, v1.PostName(is.Org, is.Repo, is.Number)); p.Status.MergedInto != target {
		t.Errorf("old post mergedInto = %q, want %s", p.Status.MergedInto, target)
	}

	// Sync never writes a post for an issue in another org.
	res, err := s.Sync(context.Background(), miloOS, []*github.Issue{&moved})
	if err != nil || res[0] != Skipped {
		t.Errorf("Sync(another org's issue) = %v, %v; want skipped", res, err)
	}
}

func TestSyncNeverCreatesANonMatchingPost(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	s := newSyncer(c, gh)
	is := testIssue(t, 3)
	is.Visibility = "private"
	gh.put(is)
	syncIssue(t, s, testSource(), is, Skipped)
	wantGone(t, c, v1.PostName(is.Org, is.Repo, is.Number))
}

// TestSyncProjectInAnotherOrg reads the status of a milo-os issue from the
// datum-cloud project through the datum-cloud installation.
func TestSyncProjectInAnotherOrg(t *testing.T) {
	c := milo(t)
	gh := newFakeGitHub()
	s := newSyncer(c, gh)
	src := testSource()
	src.Spec.GitHub.Org = "milo-os"
	src.Spec.GitHub.Issues = v1.IssueSelector{IssueTypes: []string{"Feature"}, ExcludeLabels: []string{"feedback:hide"}}
	is := testIssue(t, 8)
	is.Org, is.IssueType, is.ProjectItems = "milo-os", "Feature", nil
	gh.put(is)
	gh.projects[is.ID] = []github.ProjectItem{{Owner: "datum-cloud", Number: 22, Status: "On Deck"}}

	syncIssue(t, s, src, is, Created)
	p := getPost(t, c, v1.PostName("milo-os", is.Repo, is.Number))
	if p.Status.State != v1.PostStateOnDeck || p.Labels[v1.LabelOrg] != "milo-os" {
		t.Errorf("state = %s, labels %v; want on-deck from the datum-cloud project", p.Status.State, p.Labels)
	}
	if !slices.Equal(gh.reads, []string{"datum-cloud"}) {
		t.Errorf("ProjectItems read through %v, want the datum-cloud installation", gh.reads)
	}

	// An issue whose project items GitHub refuses isn't synced as if it
	// weren't on the project; the others still are.
	refused := testIssue(t, 9)
	refused.Org, refused.IssueType, refused.ProjectItems = "milo-os", "Feature", nil
	gh.refused = map[string]bool{refused.ID: true}
	other := *is
	other.Title = "Renamed"
	results, err := s.Sync(context.Background(), src, []*github.Issue{refused, &other})
	if _, ok := errors.AsType[*github.IncompleteError](err); !ok || !slices.Equal(results, []Result{"", Updated}) {
		t.Errorf("Sync() = %v, %v; want the refused issue to fail and the other updated", results, err)
	}
	var none v1.Post
	if err := c.Get(context.Background(), client.ObjectKey{Name: v1.PostName("milo-os", refused.Repo, refused.Number)}, &none); !apierrors.IsNotFound(err) {
		t.Errorf("post for the refused issue: %v, want not found", err)
	}
}

func TestIssueRef(t *testing.T) {
	tests := []struct {
		post   v1.Post
		repo   string
		number int32
		ok     bool
	}{
		{v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.912"}}, "enhancements", 912, true},
		{v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.docs.example.com.4"}}, "docs.example.com", 4, true},
		{v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Status: v1.PostStatus{GitHub: &v1.GitHubIssue{Repository: "Infra", Number: 3}}}, "Infra", 3, true},
		{v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements"}}, "", 0, false},
		{v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.x"}}, "", 0, false},
		{v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud..9"}}, "", 0, false},
	}
	for _, tt := range tests {
		repo, n, ok := IssueRef(&tt.post)
		if repo != tt.repo || n != tt.number || ok != tt.ok {
			t.Errorf("IssueRef(%s) = %q, %d, %v; want %q, %d, %v", tt.post.Name, repo, n, ok, tt.repo, tt.number, tt.ok)
		}
	}
}
