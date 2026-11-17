package controller

import (
	"math"
	"strings"
	"time"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// halfLife is the trending score's half-life.
const halfLife = 7 * 24 * time.Hour

// voter is one Datum vote, as far as the tally cares.
type voter struct {
	at    time.Time
	login string // linked GitHub username
}

// weight is a vote's contribution to the trending score.
func weight(at, now time.Time) float64 {
	age := max(now.Sub(at), 0)
	return math.Pow(0.5, float64(age)/float64(halfLife))
}

// tally returns the operator's vote fields for a post from its Datum voters
// and the GitHub fields feedback-sync wrote (github, githubReactions):
//
//	total    = datum + github - linked
//	trending = Σ weight(vote) + Σ weight(👍 from someone who didn't also vote)
//
// Linked people are matched by GitHub username, case-insensitively.
// Reactions without a time count towards the total but not trending. The
// score is stored multiplied by 1000.
func tally(sync v1.PostVotes, voters []voter, now time.Time) v1.PostVotes {
	logins := map[string]bool{}
	trending := 0.0
	for _, vt := range voters {
		if vt.login != "" {
			logins[strings.ToLower(vt.login)] = true
		}
		trending += weight(vt.at, now)
	}
	seen := map[string]bool{}
	var linked int32
	for _, r := range sync.GitHubReactions {
		l := strings.ToLower(r.Login)
		if seen[l] {
			continue
		}
		seen[l] = true
		if logins[l] {
			linked++
			continue
		}
		if !r.At.IsZero() {
			trending += weight(r.At.Time, now)
		}
	}
	// The reactions list can lag the count; never subtract more than the
	// count says.
	linked = min(linked, sync.GitHub)
	datum := int32(len(voters))
	return v1.PostVotes{
		Datum:         datum,
		Linked:        linked,
		Total:         datum + sync.GitHub - linked,
		TrendingScore: int64(math.Round(trending * 1000)),
	}
}
