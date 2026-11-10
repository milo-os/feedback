package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PostStatus is the public status of a post, derived from the Enhancements
// project's Status field and the issue state.
type PostStatus string

const (
	PostStatusNone       PostStatus = "none"
	PostStatusBacklog    PostStatus = "backlog"
	PostStatusPlanning   PostStatus = "planning"
	PostStatusOnDeck     PostStatus = "on-deck"
	PostStatusInProgress PostStatus = "in-progress"
	PostStatusShipped    PostStatus = "shipped"
	PostStatusClosed     PostStatus = "closed"
)

// ValidPostStatuses lists every PostStatus value.
var ValidPostStatuses = []PostStatus{
	PostStatusNone,
	PostStatusBacklog,
	PostStatusPlanning,
	PostStatusOnDeck,
	PostStatusInProgress,
	PostStatusShipped,
	PostStatusClosed,
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FeedbackSource configures what the feedback service syncs from one GitHub
// organization and how GitHub fields map onto posts. There is one per org.
//
// FeedbackSource is a Platform resource stored in etcd.
type FeedbackSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FeedbackSourceSpec   `json:"spec,omitempty"`
	Status FeedbackSourceStatus `json:"status,omitempty"`
}

// FeedbackSourceSpec defines what to sync and how to map it.
type FeedbackSourceSpec struct {
	// GitHub configures the GitHub organization this source syncs.
	GitHub GitHubSource `json:"github"`

	// StatusMapping maps Enhancements project Status option names to post
	// statuses. The empty key matches items with no Status set.
	// +optional
	StatusMapping map[string]PostStatus `json:"statusMapping,omitempty"`

	// CategoryMapping maps GitHub label names to post categories.
	// +optional
	CategoryMapping map[string]string `json:"categoryMapping,omitempty"`
}

// GitHubSource identifies a GitHub organization and the content to sync from it.
type GitHubSource struct {
	// Org is the GitHub organization login.
	Org string `json:"org"`

	// InstallationID is the GitHub App installation ID for Org.
	InstallationID int64 `json:"installationID"`

	// Repositories selects which public repositories are synced. Private
	// repositories are always skipped.
	// +optional
	Repositories RepositorySelector `json:"repositories,omitempty"`

	// Issues selects which issues become posts. An issue is included when it
	// matches any issue type or label, and is not excluded.
	// +optional
	Issues IssueSelector `json:"issues,omitempty"`

	// Project is the GitHub project that provides status and receives vote
	// totals.
	// +optional
	Project *ProjectRef `json:"project,omitempty"`

	// Milestones configures the repository whose milestones become releases.
	// +optional
	Milestones *MilestoneSource `json:"milestones,omitempty"`

	// Changelog configures the discussion category that becomes changelog
	// entries.
	// +optional
	Changelog *ChangelogSource `json:"changelog,omitempty"`
}

// RepositorySelector includes and excludes repositories by name or glob.
type RepositorySelector struct {
	// +optional
	// +listType=set
	Include []string `json:"include,omitempty"`
	// +optional
	// +listType=set
	Exclude []string `json:"exclude,omitempty"`
}

// IssueSelector selects issues by type and label.
type IssueSelector struct {
	// +optional
	// +listType=set
	IssueTypes []string `json:"issueTypes,omitempty"`
	// +optional
	// +listType=set
	Labels []string `json:"labels,omitempty"`
	// +optional
	// +listType=set
	ExcludeLabels []string `json:"excludeLabels,omitempty"`
}

// ProjectRef identifies a GitHub ProjectV2 and the fields the service uses.
type ProjectRef struct {
	// Owner is the org that owns the project.
	Owner string `json:"owner"`
	// Number is the project number.
	Number int32 `json:"number"`
	// StatusField is the single-select field that holds item status.
	// +optional
	StatusField string `json:"statusField,omitempty"`
	// VotesField is the number field the service writes vote totals to.
	// +optional
	VotesField string `json:"votesField,omitempty"`
}

// MilestoneSource is the repository whose milestones become releases.
type MilestoneSource struct {
	Repository string `json:"repository"`
}

// ChangelogSource is the discussion category that becomes changelog entries.
type ChangelogSource struct {
	Repository         string `json:"repository"`
	DiscussionCategory string `json:"discussionCategory"`
}

// FeedbackSourceStatus reports sync health for the source.
type FeedbackSourceStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastWebhookAt is when the last webhook for this source was processed.
	// +optional
	LastWebhookAt *metav1.Time `json:"lastWebhookAt,omitempty"`

	// LastReconcile describes the last completed reconcile.
	// +optional
	LastReconcile *ReconcileStatus `json:"lastReconcile,omitempty"`

	// RateLimit is the GitHub rate limit for this installation.
	// +optional
	RateLimit *RateLimitStatus `json:"rateLimit,omitempty"`
}

// ReconcileStatus describes a completed reconcile.
type ReconcileStatus struct {
	CompletedAt metav1.Time `json:"completedAt"`
	// +optional
	PostsSynced int32 `json:"postsSynced,omitempty"`
}

// RateLimitStatus is a snapshot of the GitHub rate limit.
type RateLimitStatus struct {
	Remaining int32       `json:"remaining"`
	ResetAt   metav1.Time `json:"resetAt"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FeedbackSourceList is a list of FeedbackSource.
type FeedbackSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FeedbackSource `json:"items"`
}
