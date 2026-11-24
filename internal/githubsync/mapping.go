package githubsync

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"go.miloapis.com/feedback/internal/github"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// State maps an issue to a post status.
//
// A closed issue is shipped when closed as completed and closed otherwise
// (not planned or a duplicate), whatever its project Status: a project's
// "item closed" workflow moves every closed item to Done, so Done can't tell
// the two apart. An open issue takes its project Status through
// statusMapping, where the key "" stands for an issue with no Status or not
// on the project. A Status with no entry falls back to the "" entry, and
// then to none.
func State(spec *v1.FeedbackSourceSpec, is *github.Issue) v1.PostState {
	if is.State == "closed" {
		switch is.StateReason {
		case "not_planned", "duplicate":
			return v1.PostStateClosed
		default:
			return v1.PostStateShipped
		}
	}
	status := ProjectStatus(spec.GitHub.Project, is.ProjectItems)
	if s, ok := lookupStatus(spec.StatusMapping, status); ok {
		return s
	}
	if s, ok := lookupStatus(spec.StatusMapping, ""); ok && status != "" {
		return s
	}
	return v1.PostStateNone
}

// lookupStatus finds key in the mapping, exactly or else ignoring case.
func lookupStatus(m map[string]v1.PostState, key string) (v1.PostState, bool) {
	if s, ok := m[key]; ok {
		return s, true
	}
	for k, s := range m {
		if strings.EqualFold(k, key) {
			return s, true
		}
	}
	return "", false
}

// ProjectStatus is the issue's Status on the source's project, or "" when it
// has none or isn't on the project.
func ProjectStatus(p *v1.ProjectRef, items []github.ProjectItem) string {
	if p == nil {
		return ""
	}
	for _, it := range items {
		if strings.EqualFold(it.Owner, p.Owner) && it.Number == p.Number {
			return it.Status
		}
	}
	return ""
}

// Categories maps labels to categories through categoryMapping, matching
// label names ignoring case as GitHub does. The result is sorted and has no
// duplicates. Categories that can't be a label name are left out, since each
// becomes a category.feedback.miloapis.com/<category> label.
func Categories(mapping map[string]string, labels []string) []string {
	var out []string
	for _, l := range labels {
		for k, c := range mapping {
			if strings.EqualFold(k, l) && ValidCategory(c) && !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	slices.Sort(out)
	return out
}

// ValidCategory reports whether c can be used in a category label key.
func ValidCategory(c string) bool {
	return c != "" && len(validation.IsQualifiedName(v1.CategoryLabelPrefix+c)) == 0
}

// PostLabels are the labels feedback-sync sets on a post, so clients can
// filter posts by org, status and category.
func PostLabels(org string, state v1.PostState, categories []string) map[string]string {
	l := map[string]string{
		v1.LabelOrg:   strings.ToLower(org),
		v1.LabelState: string(state),
	}
	for _, c := range categories {
		l[v1.CategoryLabelPrefix+c] = ""
	}
	return l
}
