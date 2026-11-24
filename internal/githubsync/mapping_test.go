package githubsync

import (
	"slices"
	"testing"

	"go.miloapis.com/feedback/internal/github"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// testSpec mirrors the datum-cloud source on staging.
func testSpec() *v1.FeedbackSourceSpec {
	return &v1.FeedbackSourceSpec{
		GitHub: v1.GitHubSource{
			Org:            "datum-cloud",
			InstallationID: 185055315,
			Repositories:   v1.RepositorySelector{Include: []string{"*"}, Exclude: []string{"infra", "staff-portal"}},
			Issues: v1.IssueSelector{
				IssueTypes:    []string{"Enhancement", "Feedback", "Bug"},
				Labels:        []string{"Build", "Deliver", "Connect", "Platform Features", "Infrastructure"},
				ExcludeLabels: []string{"feedback:hide"},
			},
			Project: &v1.ProjectRef{Owner: "datum-cloud", Number: 22, StatusField: "Status", VotesField: "Votes"},
		},
		StatusMapping: map[string]v1.PostState{
			"":            v1.PostStateNone,
			"Backlog":     v1.PostStateBacklog,
			"Planning":    v1.PostStatePlanning,
			"On Deck":     v1.PostStateOnDeck,
			"In progress": v1.PostStateInProgress,
			"Done":        v1.PostStateShipped,
		},
		CategoryMapping: map[string]string{
			"Build":             "build",
			"Deliver":           "deliver",
			"Connect":           "connect",
			"Platform Features": "platform",
			"Infrastructure":    "infrastructure",
		},
	}
}

func onProject(status string) []github.ProjectItem {
	return []github.ProjectItem{{Owner: "datum-cloud", Number: 22, Status: status}}
}

// TestState covers the design's Status and Category Mapping table.
func TestState(t *testing.T) {
	tests := []struct {
		name   string
		state  string
		reason string
		items  []github.ProjectItem
		want   v1.PostState
	}{
		{name: "not on the project, open", state: "open", want: v1.PostStateNone},
		{name: "on the project without a Status, open", state: "open", items: onProject(""), want: v1.PostStateNone},
		{name: "Backlog, open", state: "open", items: onProject("Backlog"), want: v1.PostStateBacklog},
		{name: "Planning, open", state: "open", items: onProject("Planning"), want: v1.PostStatePlanning},
		{name: "On Deck, open", state: "open", items: onProject("On Deck"), want: v1.PostStateOnDeck},
		{name: "In progress, open", state: "open", items: onProject("In progress"), want: v1.PostStateInProgress},
		{name: "Status case differs from the mapping", state: "open", items: onProject("In Progress"), want: v1.PostStateInProgress},
		{name: "Done, open", state: "open", items: onProject("Done"), want: v1.PostStateShipped},
		{name: "Done, closed as completed", state: "closed", reason: "completed", items: onProject("Done"), want: v1.PostStateShipped},
		{name: "In progress, closed as completed", state: "closed", reason: "completed", items: onProject("In progress"), want: v1.PostStateShipped},
		{name: "not on the project, closed as completed", state: "closed", reason: "completed", want: v1.PostStateShipped},
		{name: "closed with no reason", state: "closed", want: v1.PostStateShipped},
		{name: "Backlog, closed as not planned", state: "closed", reason: "not_planned", items: onProject("Backlog"), want: v1.PostStateClosed},
		// The project's "item closed" workflow moves it to Done.
		{name: "Done, closed as not planned", state: "closed", reason: "not_planned", items: onProject("Done"), want: v1.PostStateClosed},
		{name: "closed as a duplicate", state: "closed", reason: "duplicate", want: v1.PostStateClosed},
		{name: "reopened", state: "open", reason: "reopened", items: onProject("Planning"), want: v1.PostStatePlanning},
		{name: "Status with no mapping entry", state: "open", items: onProject("Triage"), want: v1.PostStateNone},
		{name: "on another project only", state: "open", items: []github.ProjectItem{{Owner: "datum-cloud", Number: 60, Status: "In progress"}}, want: v1.PostStateNone},
		{name: "on another org's project with the same number", state: "open", items: []github.ProjectItem{{Owner: "milo-os", Number: 22, Status: "Done"}}, want: v1.PostStateNone},
		{name: "on two projects", state: "open", items: []github.ProjectItem{{Owner: "datum-cloud", Number: 60, Status: "Done"}, {Owner: "Datum-Cloud", Number: 22, Status: "On Deck"}}, want: v1.PostStateOnDeck},
	}
	spec := testSpec()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := &github.Issue{State: tt.state, StateReason: tt.reason, ProjectItems: tt.items}
			if got := State(spec, is); got != tt.want {
				t.Errorf("State() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStateMappingFallbacks(t *testing.T) {
	open := &github.Issue{State: "open", ProjectItems: onProject("Triage")}

	spec := testSpec()
	spec.StatusMapping[""] = v1.PostStateBacklog
	if got := State(spec, open); got != v1.PostStateBacklog {
		t.Errorf("unmapped Status: got %q, want the \"\" entry, backlog", got)
	}

	spec.StatusMapping = nil
	if got := State(spec, open); got != v1.PostStateNone {
		t.Errorf("no mapping: got %q, want none", got)
	}

	spec = testSpec()
	spec.GitHub.Project = nil
	if got := State(spec, &github.Issue{State: "open", ProjectItems: onProject("Done")}); got != v1.PostStateNone {
		t.Errorf("source without a project: got %q, want none", got)
	}
}

func TestCategories(t *testing.T) {
	m := testSpec().CategoryMapping
	m["AX/DX/UX"] = "experience"
	m["Bad"] = "Not A Label!"
	tests := []struct {
		labels []string
		want   []string
	}{
		{nil, nil},
		{[]string{"bug", "help wanted"}, nil},
		{[]string{"Deliver", "Build"}, []string{"build", "deliver"}},
		{[]string{"platform features"}, []string{"platform"}},
		{[]string{"AX/DX/UX", "Connect", "connect"}, []string{"connect", "experience"}},
		{[]string{"Bad", "Infrastructure"}, []string{"infrastructure"}},
	}
	for _, tt := range tests {
		if got := Categories(m, tt.labels); !slices.Equal(got, tt.want) {
			t.Errorf("Categories(%q) = %q, want %q", tt.labels, got, tt.want)
		}
	}
}

func TestPostLabels(t *testing.T) {
	got := PostLabels("Datum-Cloud", v1.PostStateInProgress, []string{"build", "connect"})
	want := map[string]string{
		v1.LabelOrg:                        "datum-cloud",
		v1.LabelState:                      "in-progress",
		v1.CategoryLabelPrefix + "build":   "",
		v1.CategoryLabelPrefix + "connect": "",
	}
	if len(got) != len(want) {
		t.Fatalf("PostLabels() = %v, want %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("label %s = %q (present %v), want %q", k, g, ok, v)
		}
	}
}
