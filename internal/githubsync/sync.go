package githubsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"go.miloapis.com/feedback/internal/github"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// FieldManager is the field manager feedback-sync applies a post's labels and
// GitHub-derived status with. An apply drops the fields its manager applied
// before and left out, so any other sync writer (vote counts, releases)
// needs a manager of its own, and none may share one with the operator.
const FieldManager = "feedback-sync"

// GitHub is what Syncer reads from GitHub.
type GitHub interface {
	// Issue reads one issue through the installation. It returns
	// github.ErrNotFound when the issue or its repository is gone or can't
	// be seen.
	Issue(ctx context.Context, installationID int64, owner, repo string, number int32, statusField string) (*github.Issue, error)
	// ProjectItems reads issues' project items through the app's
	// installation on org.
	ProjectItems(ctx context.Context, org string, ids []string, statusField string) (map[string][]github.ProjectItem, error)
}

// App adapts a github.App to GitHub.
type App struct{ *github.App }

// Issue implements GitHub.
func (a App) Issue(ctx context.Context, installationID int64, owner, repo string, number int32, statusField string) (*github.Issue, error) {
	return a.Client(installationID).Issue(ctx, owner, repo, number, statusField)
}

// ProjectItems implements GitHub.
func (a App) ProjectItems(ctx context.Context, org string, ids []string, statusField string) (map[string][]github.ProjectItem, error) {
	id, err := a.InstallationID(ctx, org)
	if err != nil {
		return nil, err
	}
	return a.Client(id).ProjectItems(ctx, ids, statusField)
}

// Syncer writes posts from GitHub issues.
type Syncer struct {
	// Client writes to Milo.
	Client client.Client
	// Reader reads posts, usually from a cache. Defaults to Client.
	Reader client.Reader
	GitHub GitHub
	// Now defaults to time.Now.
	Now func() time.Time
}

// Result says what syncing an issue did to its post.
type Result string

const (
	// Created means a new post was created.
	Created Result = "created"
	// Updated means the post's labels or status were applied.
	Updated Result = "updated"
	// Unchanged means the post already matched the issue.
	Unchanged Result = "unchanged"
	// Deleted means the issue no longer matches and its post was deleted.
	Deleted Result = "deleted"
	// Skipped means the issue doesn't match and has no post, or belongs to
	// another org's source.
	Skipped Result = "skipped"
	// Moved means the issue was transferred and its post marked as merged
	// into the post for its new location.
	Moved Result = "moved"
)

func (s *Syncer) reader() client.Reader {
	if s.Reader != nil {
		return s.Reader
	}
	return s.Client
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func statusField(src *v1.FeedbackSource) string {
	if p := src.Spec.GitHub.Project; p != nil {
		return p.StatusField
	}
	return ""
}

// SyncIssue re-reads one issue from GitHub and makes its post match it:
// creates the post, applies changes to it, or deletes it when the issue no
// longer matches the source (a label removed, the repository made private or
// excluded, the issue or repository deleted). Sweeps and webhook consumers
// both sync items through here.
//
// A transferred issue keeps its old post, marked as merged into the post for
// its new location, so the operator moves its votes and follows there. The
// new location is synced too when it is in the source's org; another org's
// posts are left to that org's source.
func (s *Syncer) SyncIssue(ctx context.Context, src *v1.FeedbackSource, repo string, number int32) (Result, error) {
	org := src.Spec.GitHub.Org
	name := v1.PostName(org, repo, number)
	is, err := s.GitHub.Issue(ctx, src.Spec.GitHub.InstallationID, org, repo, number, statusField(src))
	if errors.Is(err, github.ErrNotFound) {
		return s.remove(ctx, name, "the issue no longer exists or can't be seen")
	}
	if err != nil {
		return "", fmt.Errorf("reading %s/%s#%d: %w", org, repo, number, err)
	}
	if moved := v1.PostName(is.Org, is.Repo, is.Number); moved != name {
		// GitHub answers for a transferred issue from its new location.
		if strings.EqualFold(is.Org, org) {
			if _, err := s.Sync(ctx, src, []*github.Issue{is}); err != nil {
				return "", err
			}
		}
		return s.markMoved(ctx, name, moved)
	}
	results, err := s.Sync(ctx, src, []*github.Issue{is})
	if err != nil {
		return "", err
	}
	return results[0], nil
}

// Sync makes the posts for issues already read from GitHub match them, as
// SyncIssue does for one. A sweep calls it with the issues a search
// returned. The results are in the order of issues. An issue that fails to
// sync has an empty result; the rest are still synced, and the errors are
// returned joined.
func (s *Syncer) Sync(ctx context.Context, src *v1.FeedbackSource, issues []*github.Issue) ([]Result, error) {
	refused, err := s.readProjectItems(ctx, src, issues)
	if refused == nil {
		return nil, err
	}
	results := make([]Result, len(issues))
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for i, is := range issues {
		if err := ctx.Err(); err != nil {
			return results, errors.Join(append(errs, err)...)
		}
		if refused[is.ID] {
			// Without its project items, the issue's status is unknown.
			continue
		}
		res, err := s.syncOne(ctx, src, is)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		results[i] = res
	}
	return results, errors.Join(errs...)
}

// readProjectItems reads project items from the project owner's
// installation when the project belongs to another org: an installation
// only sees its own org's projects. It returns the IDs of issues whose items
// GitHub refused, which mustn't be synced as if they weren't on the project,
// or nil if it couldn't read any.
func (s *Syncer) readProjectItems(ctx context.Context, src *v1.FeedbackSource, issues []*github.Issue) (map[string]bool, error) {
	refused := map[string]bool{}
	p := src.Spec.GitHub.Project
	if p == nil || strings.EqualFold(p.Owner, src.Spec.GitHub.Org) || len(issues) == 0 {
		return refused, nil
	}
	ids := make([]string, 0, len(issues))
	for _, is := range issues {
		ids = append(ids, is.ID)
	}
	items, err := s.GitHub.ProjectItems(ctx, p.Owner, ids, p.StatusField)
	if inc, ok := errors.AsType[*github.IncompleteError](err); ok {
		for _, id := range inc.IDs {
			refused[id] = true
		}
	} else if err != nil {
		return nil, fmt.Errorf("reading the %s project: %w", p.Owner, err)
	}
	for _, is := range issues {
		if !refused[is.ID] {
			is.ProjectItems = items[is.ID]
		}
	}
	if err != nil {
		return refused, fmt.Errorf("reading the %s project: %w", p.Owner, err)
	}
	return refused, nil
}

func (s *Syncer) syncOne(ctx context.Context, src *v1.FeedbackSource, is *github.Issue) (Result, error) {
	name := v1.PostName(is.Org, is.Repo, is.Number)
	if !strings.EqualFold(is.Org, src.Spec.GitHub.Org) {
		// Another org's source owns this issue's post.
		return Skipped, nil
	}
	if ok, why := Match(&src.Spec.GitHub, is); !ok {
		return s.remove(ctx, name, why)
	}
	want := Desired(&src.Spec, is)

	var post v1.Post
	err := s.reader().Get(ctx, client.ObjectKey{Name: name}, &post)
	switch {
	case apierrors.IsNotFound(err):
		return s.create(ctx, name, want)
	case err != nil:
		return "", fmt.Errorf("reading post %s: %w", name, err)
	}
	if post.DeletionTimestamp != nil {
		return "", fmt.Errorf("post %s is being deleted; it is created again on the next sync", name)
	}

	res := Unchanged
	if !sameLabels(syncLabels(post.Labels), want.Labels) {
		if err := s.applyLabels(ctx, name, want.Labels); err != nil {
			return "", err
		}
		res = Updated
	}
	if !equality.Semantic.DeepEqual(fromPost(&post.Status), want.Status) {
		if err := s.applyStatus(ctx, name, want.Status); err != nil {
			return "", err
		}
		res = Updated
	}
	return res, nil
}

// create creates the post, then applies its labels and status. Creating it
// bare keeps every field feedback-sync sets owned by its apply, so a later
// apply that leaves a label out removes it.
func (s *Syncer) create(ctx context.Context, name string, want DesiredPost) (Result, error) {
	post := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := s.Client.Create(ctx, post, client.FieldOwner(FieldManager))
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating post %s: %w", name, err)
	}
	if err := s.applyLabels(ctx, name, want.Labels); err != nil {
		return "", err
	}
	if err := s.applyStatus(ctx, name, want.Status); err != nil {
		return "", err
	}
	logf.FromContext(ctx).Info("Created post", "post", name)
	return Created, nil
}

// markMoved marks the post of a transferred issue as merged into the post
// for its new location, and closed. It keeps the rest of what sync wrote, so
// the post still describes the issue.
func (s *Syncer) markMoved(ctx context.Context, name, target string) (Result, error) {
	var post v1.Post
	err := s.reader().Get(ctx, client.ObjectKey{Name: name}, &post)
	if apierrors.IsNotFound(err) {
		return Skipped, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading post %s: %w", name, err)
	}
	if post.DeletionTimestamp != nil {
		return Skipped, nil
	}
	labels := syncLabels(post.Labels)
	labels[v1.LabelState] = string(v1.PostStateClosed)
	st := fromPost(&post.Status)
	st.State = v1.PostStateClosed
	st.MergedInto = target

	res := Unchanged
	if !sameLabels(syncLabels(post.Labels), labels) {
		if err := s.applyLabels(ctx, name, labels); err != nil {
			return "", err
		}
		res = Moved
	}
	if !equality.Semantic.DeepEqual(fromPost(&post.Status), st) {
		if err := s.applyStatus(ctx, name, st); err != nil {
			return "", err
		}
		logf.FromContext(ctx).Info("Marked a transferred issue's post as merged", "post", name, "into", target)
		res = Moved
	}
	return res, nil
}

// remove deletes a post that no longer matches, if there is one. The
// operator keeps its votes for 30 days in case it comes back.
func (s *Syncer) remove(ctx context.Context, name, why string) (Result, error) {
	var post v1.Post
	err := s.reader().Get(ctx, client.ObjectKey{Name: name}, &post)
	if apierrors.IsNotFound(err) {
		return Skipped, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading post %s: %w", name, err)
	}
	if post.DeletionTimestamp != nil {
		return Deleted, nil
	}
	err = s.Client.Delete(ctx, &post, client.Preconditions{UID: &post.UID})
	if apierrors.IsNotFound(err) {
		return Skipped, nil
	}
	if err != nil {
		return "", fmt.Errorf("deleting post %s: %w", name, err)
	}
	logf.FromContext(ctx).Info("Deleted post", "post", name, "reason", why)
	return Deleted, nil
}

// syncLabels are the labels feedback-sync owns among a post's labels.
func syncLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if k == v1.LabelOrg || k == v1.LabelState || strings.HasPrefix(k, v1.CategoryLabelPrefix) {
			out[k] = v
		}
	}
	return out
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (s *Syncer) applyLabels(ctx context.Context, name string, labels map[string]string) error {
	l := make(map[string]any, len(labels))
	for k, v := range labels {
		l[k] = v
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1.SchemeGroupVersion.String(),
		"kind":       "Post",
		"metadata":   map[string]any{"name": name, "labels": l},
	}}
	if err := s.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying labels to post %s: %w", name, err)
	}
	return nil
}

func (s *Syncer) applyStatus(ctx context.Context, name string, st SyncedStatus) error {
	now := metav1.NewTime(s.now().UTC().Truncate(time.Second))
	st.LastSyncedAt = &now
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1.SchemeGroupVersion.String(),
		"kind":       "Post",
		"metadata":   map[string]any{"name": name},
		"status":     status,
	}}
	if err := s.Client.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying status to post %s: %w", name, err)
	}
	return nil
}

// DesiredPost is what feedback-sync writes to an issue's post.
type DesiredPost struct {
	Labels map[string]string
	Status SyncedStatus
}

// SyncedStatus is the part of a post's status that feedback-sync owns.
// The operator owns votes, delivery and conditions.
type SyncedStatus struct {
	Title      string          `json:"title,omitempty"`
	Summary    string          `json:"summary,omitempty"`
	BodyHTML   string          `json:"bodyHTML,omitempty"`
	State      v1.PostState    `json:"state,omitempty"`
	Categories []string        `json:"categories,omitempty"`
	Labels     []string        `json:"labels,omitempty"`
	IssueType  string          `json:"issueType,omitempty"`
	GitHub     *v1.GitHubIssue `json:"github,omitempty"`
	// MergedInto is set when the issue was transferred: the post for its
	// new location.
	MergedInto   string       `json:"mergedInto,omitempty"`
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// fromPost is the synced part of a post's status, without lastSyncedAt, for
// comparing with what an issue wants.
func fromPost(st *v1.PostStatus) SyncedStatus {
	return SyncedStatus{
		Title:      st.Title,
		Summary:    st.Summary,
		BodyHTML:   st.BodyHTML,
		State:      st.State,
		Categories: st.Categories,
		Labels:     st.Labels,
		IssueType:  st.IssueType,
		GitHub:     st.GitHub,
		MergedInto: st.MergedInto,
	}
}

// Desired maps an issue that matches the source to its post's labels and
// status.
func Desired(spec *v1.FeedbackSourceSpec, is *github.Issue) DesiredPost {
	state := State(spec, is)
	categories := Categories(spec.CategoryMapping, is.Labels)
	labels := slices.Clone(is.Labels)
	slices.Sort(labels)
	labels = slices.Compact(labels)
	return DesiredPost{
		Labels: PostLabels(is.Org, state, categories),
		Status: SyncedStatus{
			Title:      is.Title,
			Summary:    Summary(is.BodyText),
			BodyHTML:   SanitizeHTML(is.BodyHTML),
			State:      state,
			Categories: categories,
			Labels:     labels,
			IssueType:  is.IssueType,
			GitHub: &v1.GitHubIssue{
				Org:         is.Org,
				Repository:  is.Repo,
				Number:      is.Number,
				URL:         is.URL,
				State:       is.State,
				StateReason: is.StateReason,
				CreatedAt:   metav1.NewTime(is.CreatedAt.UTC().Truncate(time.Second)),
				UpdatedAt:   metav1.NewTime(is.UpdatedAt.UTC().Truncate(time.Second)),
			},
		},
	}
}

// IssueRef parses the repository and number of a post's issue from its
// status, or failing that from its name, "<org>.<repository>.<number>".
func IssueRef(post *v1.Post) (repo string, number int32, ok bool) {
	if gh := post.Status.GitHub; gh != nil && gh.Repository != "" && gh.Number > 0 {
		return gh.Repository, gh.Number, true
	}
	first := strings.IndexByte(post.Name, '.')
	last := strings.LastIndexByte(post.Name, '.')
	if first < 0 || last <= first+1 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(post.Name[last+1:], 10, 32)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return post.Name[first+1 : last], int32(n), true
}
