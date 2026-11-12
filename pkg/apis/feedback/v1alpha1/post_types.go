package v1alpha1

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PostState is the public status of a post, derived from the Enhancements
// project's Status field and the issue state.
// +enum
type PostState string

const (
	PostStateNone       PostState = "none"
	PostStateBacklog    PostState = "backlog"
	PostStatePlanning   PostState = "planning"
	PostStateOnDeck     PostState = "on-deck"
	PostStateInProgress PostState = "in-progress"
	PostStateShipped    PostState = "shipped"
	PostStateClosed     PostState = "closed"
)

// ValidPostStates lists every PostState value.
var ValidPostStates = []PostState{
	PostStateNone,
	PostStateBacklog,
	PostStatePlanning,
	PostStateOnDeck,
	PostStateInProgress,
	PostStateShipped,
	PostStateClosed,
}

// DeliveryStage is where a post is in delivery. Stages are ordered
// requested < planned < picked-up < released; closed is terminal.
// +enum
type DeliveryStage string

const (
	DeliveryStageRequested DeliveryStage = "requested"
	DeliveryStagePlanned   DeliveryStage = "planned"
	DeliveryStagePickedUp  DeliveryStage = "picked-up"
	DeliveryStageReleased  DeliveryStage = "released"
	DeliveryStageClosed    DeliveryStage = "closed"
)

// ValidDeliveryStages lists every DeliveryStage value.
var ValidDeliveryStages = []DeliveryStage{
	DeliveryStageRequested,
	DeliveryStagePlanned,
	DeliveryStagePickedUp,
	DeliveryStageReleased,
	DeliveryStageClosed,
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Post is a public roadmap item synced from a GitHub issue. Every Post is
// backed by a GitHub issue; unreviewed feedback is a Submission, never a Post.
//
// The name is "<org>.<repository>.<number>", e.g. datum-cloud.enhancements.912.
//
// Only the feedback-sync service account creates, updates and deletes posts
// and writes their status. Staff can only patch spec.hidden. Everyone else can
// read them.
type Post struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostSpec   `json:"spec,omitempty"`
	Status PostStatus `json:"status,omitempty"`
}

// PostSpec holds the fields staff control.
type PostSpec struct {
	// Hidden removes the post from every client and from search.
	// +optional
	Hidden bool `json:"hidden,omitempty"`
}

// PostStatus holds the fields synced from GitHub and derived by the service.
type PostStatus struct {
	// Title is the issue title.
	// +optional
	Title string `json:"title,omitempty"`
	// Summary is a plain-text excerpt of the body.
	// +optional
	Summary string `json:"summary,omitempty"`
	// BodyHTML is the issue body, sanitised against an allow-list.
	// +optional
	BodyHTML string `json:"bodyHTML,omitempty"`
	// State is the public status, mapped from the Enhancements project.
	// +optional
	State PostState `json:"state,omitempty"`
	// Categories are mapped from labels by the FeedbackSource.
	// +optional
	// +listType=set
	Categories []string `json:"categories,omitempty"`
	// Labels are the issue's GitHub labels.
	// +optional
	// +listType=set
	Labels []string `json:"labels,omitempty"`
	// IssueType is the GitHub issue type, e.g. Enhancement.
	// +optional
	IssueType string `json:"issueType,omitempty"`
	// GitHub identifies the backing issue.
	// +optional
	GitHub *GitHubIssue `json:"github,omitempty"`
	// Release is the name of the Release whose milestone the issue is on.
	// +optional
	Release string `json:"release,omitempty"`
	// Votes are the vote totals.
	// +optional
	Votes PostVotes `json:"votes,omitempty"`
	// MergedInto is the post this one was closed as a duplicate of. When
	// feedback-sync sets it, votes and subscriptions move to that post.
	// +optional
	MergedInto string `json:"mergedInto,omitempty"`
	// PublicVoters are up to 10 voters who opted in to showing their votes.
	// +optional
	// +listType=atomic
	PublicVoters []PublicVoter `json:"publicVoters,omitempty"`
	// Delivery tracks the post from request to release.
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`
	// LastSyncedAt is when feedback-sync last wrote this post.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// GitHubIssue identifies the issue backing a post.
type GitHubIssue struct {
	Org        string `json:"org"`
	Repository string `json:"repository"`
	Number     int32  `json:"number"`
	URL        string `json:"url"`
	// State is "open" or "closed".
	State string `json:"state"`
	// StateReason is GitHub's close reason: "completed" or "not_planned".
	// +optional
	StateReason string `json:"stateReason,omitempty"`
	// +optional
	CreatedAt metav1.Time `json:"createdAt,omitempty"`
	// +optional
	UpdatedAt metav1.Time `json:"updatedAt,omitempty"`
}

// PostVotes are vote totals for a post.
type PostVotes struct {
	// Total is Datum votes plus GitHub 👍, minus people matched as doing both.
	// +optional
	Total int32 `json:"total,omitempty"`
	// Datum is the number of Vote resources.
	// +optional
	Datum int32 `json:"datum,omitempty"`
	// GitHub is the 👍 reaction count on the issue.
	// +optional
	GitHub int32 `json:"github,omitempty"`
	// Linked is how many people both voted and reacted 👍, matched by the
	// GitHub username linked to their Datum account. They count once in
	// Total.
	// +optional
	Linked int32 `json:"linked,omitempty"`
	// TrendingScore weights votes with a 7-day half-life:
	// Σ 0.5^(age_days / 7) over Datum votes and GitHub 👍 reactions whose
	// time is known, counting linked people once.
	// +optional
	TrendingScore float64 `json:"trendingScore,omitempty"`
	// GitHubReactions are the issue's 👍 reactions, written by feedback-sync
	// when the count changes. Used to match linked voters and for the
	// trending score. GitHub reactions are public.
	// +optional
	// +listType=atomic
	GitHubReactions []GitHubReaction `json:"githubReactions,omitempty"`
}

// GitHubReaction is one 👍 on a GitHub issue.
type GitHubReaction struct {
	// Login is the reactor's GitHub username.
	Login string `json:"login"`
	// At is when they reacted.
	// +optional
	At metav1.Time `json:"at,omitempty"`
}

// PublicVoter is a voter who opted in to being shown.
type PublicVoter struct {
	DisplayName string `json:"displayName"`
	// +optional
	AvatarURL string `json:"avatarURL,omitempty"`
}

// DeliveryStatus tracks a post's delivery stage and the work behind it.
type DeliveryStatus struct {
	// Stage is the highest stage whose condition currently holds.
	// +optional
	Stage DeliveryStage `json:"stage,omitempty"`
	// HighestNotified is the highest stage announced to followers. A stage is
	// only announced when it passes this one.
	// +optional
	HighestNotified DeliveryStage `json:"highestNotified,omitempty"`
	// History records each stage change.
	// +optional
	// +listType=atomic
	History []DeliveryEvent `json:"history,omitempty"`
	// Work is the related work behind the post.
	// +optional
	Work *DeliveryWork `json:"work,omitempty"`
}

// DeliveryEvent is one stage change.
type DeliveryEvent struct {
	Stage DeliveryStage `json:"stage"`
	At    metav1.Time   `json:"at"`
	// Cause describes what moved the stage, e.g. a pull request opening.
	// +optional
	Cause string `json:"cause,omitempty"`
}

// DeliveryWork summarises sub-issues and linked pull requests. Only public
// issues and pull requests are included.
type DeliveryWork struct {
	// Total and Done count sub-issues when there are any, otherwise linked
	// pull requests.
	// +optional
	Total int32 `json:"total,omitempty"`
	// +optional
	Done int32 `json:"done,omitempty"`
	// +optional
	// +listType=atomic
	Items []WorkItem `json:"items,omitempty"`
	// PartShipped records changelog entries that shipped part of the work.
	// +optional
	// +listType=atomic
	PartShipped []PartShipped `json:"partShipped,omitempty"`
}

// WorkItem is a sub-issue or linked pull request.
type WorkItem struct {
	// Kind is "issue" or "pullRequest".
	Kind string `json:"kind"`
	// Ref is "<org>/<repo>#<number>".
	Ref   string `json:"ref"`
	Title string `json:"title,omitempty"`
	// State is "open", "closed" or "merged".
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
	// PullRequests are the pull requests that close this issue.
	// +optional
	// +listType=atomic
	PullRequests []PullRequestRef `json:"pullRequests,omitempty"`
}

// PullRequestRef is a pull request linked to a work item.
type PullRequestRef struct {
	Ref   string `json:"ref"`
	State string `json:"state"`
}

// PartShipped records a changelog entry that linked a work item before the
// post itself was released.
type PartShipped struct {
	// Changelog is the ChangelogEntry name.
	Changelog string      `json:"changelog"`
	At        metav1.Time `json:"at"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PostList is a list of Post.
type PostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Post `json:"items"`
}

// PostName is the name of the Post for a GitHub issue:
// "<org>.<repository>.<number>", lower-cased.
func PostName(org, repository string, number int32) string {
	return fmt.Sprintf("%s.%s.%d", strings.ToLower(org), strings.ToLower(repository), number)
}
