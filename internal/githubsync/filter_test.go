package githubsync

import (
	"os"
	"path"
	"regexp"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"go.miloapis.com/feedback/internal/github"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func matching() *github.Issue {
	return &github.Issue{
		Org: "datum-cloud", Repo: "enhancements", Number: 912, Visibility: "public",
		State: "open", IssueType: "Enhancement", Labels: []string{"Build"},
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name   string
		change func(*github.Issue)
		source func(*v1.GitHubSource)
		want   bool
	}{
		{name: "issue type and label", want: true},
		{name: "issue type only", change: func(i *github.Issue) { i.Labels = nil }, want: true},
		{name: "label only", change: func(i *github.Issue) { i.IssueType = "" }, want: true},
		{name: "issue type in another case", change: func(i *github.Issue) { i.IssueType, i.Labels = "bug", nil }, want: true},
		{name: "label in another case", change: func(i *github.Issue) { i.IssueType, i.Labels = "Task", []string{"platform features"} }, want: true},
		{name: "neither issue type nor label", change: func(i *github.Issue) { i.IssueType, i.Labels = "Task", []string{"help wanted"} }},
		{name: "no issue type, no labels", change: func(i *github.Issue) { i.IssueType, i.Labels = "", nil }},
		{name: "excluded label wins over a match", change: func(i *github.Issue) { i.Labels = append(i.Labels, "feedback:hide") }},
		{name: "excluded label in another case", change: func(i *github.Issue) { i.Labels = append(i.Labels, "Feedback:Hide") }},
		{name: "private repository", change: func(i *github.Issue) { i.Visibility = "private" }},
		{name: "internal repository", change: func(i *github.Issue) { i.Visibility = "internal" }},
		{name: "unknown visibility", change: func(i *github.Issue) { i.Visibility = "" }},
		{name: "excluded repository", change: func(i *github.Issue) { i.Repo = "infra" }},
		{name: "excluded repository in another case", change: func(i *github.Issue) { i.Repo = "Staff-Portal" }},
		{name: "another org", change: func(i *github.Issue) { i.Org = "milo-os" }},
		{name: "org in another case", change: func(i *github.Issue) { i.Org = "Datum-Cloud" }, want: true},
		{
			name:   "include glob",
			source: func(s *v1.GitHubSource) { s.Repositories.Include = []string{"enh*"} },
			want:   true,
		},
		{
			name:   "not in the include globs",
			source: func(s *v1.GitHubSource) { s.Repositories.Include = []string{"compute", "net-*"} },
		},
		{
			name:   "exclude glob",
			source: func(s *v1.GitHubSource) { s.Repositories.Exclude = []string{"*ments"} },
		},
		{
			name:   "a malformed exclude pattern excludes everything",
			source: func(s *v1.GitHubSource) { s.Repositories.Exclude = []string{"infra-[a-z"} },
		},
		{
			name:   "a malformed include pattern selects nothing",
			source: func(s *v1.GitHubSource) { s.Repositories.Include = []string{"[enhancements"} },
		},
		{
			name:   "no include patterns selects every repository",
			source: func(s *v1.GitHubSource) { s.Repositories.Include = nil },
			want:   true,
		},
		{
			name:   "a source with no issue types or labels includes nothing",
			source: func(s *v1.GitHubSource) { s.Issues.IssueTypes, s.Issues.Labels = nil, nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := testSpec().GitHub
			if tt.source != nil {
				tt.source(&src)
			}
			is := matching()
			if tt.change != nil {
				tt.change(is)
			}
			got, reason := Match(&src, is)
			if got != tt.want {
				t.Errorf("Match() = %v (%s), want %v", got, reason, tt.want)
			}
			if !got && reason == "" {
				t.Error("Match() gave no reason for leaving the issue out")
			}
		})
	}
}

func TestSearchQueries(t *testing.T) {
	src := testSpec().GitHub
	src.Repositories.Exclude = append(src.Repositories.Exclude, "tmp-*")
	got := SearchQueries(&src)
	base := `org:datum-cloud is:issue is:public -repo:datum-cloud/infra -repo:datum-cloud/staff-portal -label:"feedback:hide"`
	want := []string{
		base + ` type:"Enhancement","Feedback","Bug"`,
		base + ` label:"Build","Deliver","Connect","Platform Features","Infrastructure"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("SearchQueries() =\n%q\nwant\n%q", got, want)
	}

	src.Issues.IssueTypes = nil
	src.Issues.Labels = []string{`say "hi"`, "Build"}
	if got := SearchQueries(&src); len(got) != 1 || !slices.Contains(got, base+` label:"Build"`) {
		t.Errorf("labels only, one unsearchable: got %q", got)
	}

	src.Issues.Labels = nil
	if got := SearchQueries(&src); len(got) != 0 {
		t.Errorf("no issue types or labels: got %q, want none", got)
	}
}

// TestRepositoryPatternsAreWellFormed checks that every glob the CRD accepts
// is one path.Match can use, over every short string of glob characters.
func TestRepositoryPatternsAreWellFormed(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/feedback.miloapis.com_feedbacksources.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	repos := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["github"].Properties["repositories"]
	for _, field := range []string{"include", "exclude"} {
		pattern := repos.Properties[field].Items.Schema.Pattern
		if pattern == "" {
			t.Fatalf("repositories.%s has no pattern", field)
		}
		re := regexp.MustCompile(pattern)
		for _, bad := range []string{"infra-[a-z", "[]", "[-a]", "[a-]", "a]", `a\b`, "[^a]"} {
			if re.MatchString(bad) {
				t.Errorf("repositories.%s accepts %q", field, bad)
			}
		}
		const alphabet = "az-[]*?."
		var walk func(prefix string)
		walk = func(prefix string) {
			if prefix != "" && re.MatchString(prefix) {
				if _, err := path.Match(prefix, "x"); err != nil {
					t.Errorf("repositories.%s accepts %q, which path.Match can't use: %v", field, prefix, err)
				}
			}
			if len(prefix) == 6 {
				return
			}
			for _, c := range alphabet {
				walk(prefix + string(c))
			}
		}
		walk("")
	}
}
