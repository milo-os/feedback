package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestTally(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	week := metav1.NewTime(now.Add(-halfLife))
	voters := []voter{{at: now, login: "ada"}, {at: now.Add(-2 * halfLife)}, {at: now.Add(time.Hour)}}

	got := tally(v1.PostVotes{GitHub: 2, GitHubReactions: []v1.GitHubReaction{
		{Login: "ADA", At: week}, {Login: "ada", At: week}, // same person twice
		{Login: "grace", At: week},
		{Login: "linus"}, // no time
	}}, voters, now)
	// The list is longer than the count (lagging sync): linked can't exceed it.
	if got.Datum != 3 || got.Linked != 1 || got.Total != 4 {
		t.Errorf("counts: %+v", got)
	}
	// 1 (now) + 0.25 (two weeks) + 1 (future clamps to now) + 0.5 (grace).
	if got.TrendingScore != 2750 {
		t.Errorf("trending %v, want 2750", got.TrendingScore)
	}

	got = tally(v1.PostVotes{GitHub: 0, GitHubReactions: []v1.GitHubReaction{{Login: "ada", At: week}}}, voters[:1], now)
	if got.Linked != 0 || got.Total != 1 {
		t.Errorf("linked capped by count: %+v", got)
	}
}
