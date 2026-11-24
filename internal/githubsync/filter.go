// Package githubsync turns GitHub issues into posts: it decides which issues
// a FeedbackSource includes, maps them onto post fields, and writes the posts
// to Milo.
package githubsync

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"go.miloapis.com/feedback/internal/github"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// Match reports whether a source includes an issue, and if not, why. An issue
// is included when it is in a public repository the source selects, has any
// of the source's issue types or labels, and has none of its excluded labels.
// A source with no issue types and no labels includes nothing.
func Match(src *v1.GitHubSource, is *github.Issue) (bool, string) {
	switch {
	case !strings.EqualFold(is.Org, src.Org):
		return false, fmt.Sprintf("the issue is in %s, not %s", is.Org, src.Org)
	case is.Visibility != "public":
		return false, "the repository is not public"
	case !repositorySelected(src.Repositories, is.Repo):
		return false, "the repository is not selected"
	case anyFold(is.Labels, src.Issues.ExcludeLabels):
		return false, "the issue has an excluded label"
	case containsFold(src.Issues.IssueTypes, is.IssueType) && is.IssueType != "":
		return true, ""
	case anyFold(is.Labels, src.Issues.Labels):
		return true, ""
	default:
		return false, "the issue has none of the included issue types or labels"
	}
}

// repositorySelected applies the include and exclude globs. No include
// patterns selects every repository. Names compare case-insensitively, as on
// GitHub.
func repositorySelected(sel v1.RepositorySelector, repo string) bool {
	repo = strings.ToLower(repo)
	matches := func(patterns []string) bool {
		return slices.ContainsFunc(patterns, func(p string) bool {
			ok, err := path.Match(strings.ToLower(p), repo)
			return err == nil && ok
		})
	}
	if len(sel.Include) > 0 && !matches(sel.Include) {
		return false
	}
	return !matches(sel.Exclude)
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

// anyFold reports whether any of have is in want.
func anyFold(have, want []string) bool {
	return slices.ContainsFunc(have, func(h string) bool { return containsFold(want, h) })
}

// SearchQueries returns the issue searches that together find every issue a
// source may include: one for its issue types and one for its labels, since
// search can't OR across qualifiers. Results still go through Match, which
// applies repository globs and anything search can't express.
func SearchQueries(src *v1.GitHubSource) []string {
	base := []string{"org:" + src.Org, "is:issue", "is:public"}
	for _, r := range src.Repositories.Exclude {
		if !strings.ContainsAny(r, `*?[\`) {
			base = append(base, "-repo:"+src.Org+"/"+r)
		}
	}
	for _, l := range src.Issues.ExcludeLabels {
		if q, ok := quote(l); ok {
			base = append(base, "-label:"+q)
		}
	}
	var queries []string
	if q := qualifier("type", src.Issues.IssueTypes); q != "" {
		queries = append(queries, strings.Join(append(slices.Clone(base), q), " "))
	}
	if q := qualifier("label", src.Issues.Labels); q != "" {
		queries = append(queries, strings.Join(append(slices.Clone(base), q), " "))
	}
	return queries
}

// qualifier ORs values in one search qualifier: type:"Bug","Feature".
func qualifier(name string, values []string) string {
	var qs []string
	for _, v := range values {
		if q, ok := quote(v); ok {
			qs = append(qs, q)
		}
	}
	if len(qs) == 0 {
		return ""
	}
	return name + ":" + strings.Join(qs, ",")
}

// quote quotes a search value. Search has no escape for a double quote, so a
// value containing one can't be searched for; Match still applies it.
func quote(v string) (string, bool) {
	if v == "" || strings.Contains(v, `"`) {
		return "", false
	}
	return `"` + v + `"`, true
}
