package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
// +kubebuilder:validation:XValidation:rule="self.metadata.name == self.spec.github.org.lowerAscii()",message="metadata.name must be spec.github.org in lower case, so there is one source per org"
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.github.org`
// +kubebuilder:printcolumn:name="Last reconcile",type=date,JSONPath=`.status.lastReconcile.completedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FeedbackSource configures what the feedback service syncs from one GitHub
// organization and how GitHub fields map onto posts. There is one per org,
// named after it: a sweep treats every post of the org as its own.
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
	StatusMapping map[string]PostState `json:"statusMapping,omitempty"`

	// CategoryMapping maps GitHub label names to post categories.
	// +optional
	CategoryMapping map[string]string `json:"categoryMapping,omitempty"`
}

// GitHubSource identifies a GitHub organization and the content to sync from it.
// +kubebuilder:validation:XValidation:rule="has(self.issues) && ((has(self.issues.issueTypes) && size(self.issues.issueTypes) > 0) || (has(self.issues.labels) && size(self.issues.labels) > 0))",message="issues needs at least one issue type or label; a source that includes nothing would delete every post of the org"
type GitHubSource struct {
	// Org is the GitHub organization login.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`
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

// RepositorySelector includes and excludes repositories by name or glob
// (path.Match syntax), ignoring case.
type RepositorySelector struct {
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^([A-Za-z0-9._*?-]|\[([A-Za-z0-9._](-[A-Za-z0-9._])?)+\])+$`
	Include []string `json:"include,omitempty"`
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^([A-Za-z0-9._*?-]|\[([A-Za-z0-9._](-[A-Za-z0-9._])?)+\])+$`
	Exclude []string `json:"exclude,omitempty"`
}

// IssueSelector selects issues by type and label, ignoring case. Values
// can't contain a double quote, which GitHub search can't escape.
type IssueSelector struct {
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[^"]+$`
	IssueTypes []string `json:"issueTypes,omitempty"`
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[^"]+$`
	Labels []string `json:"labels,omitempty"`
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[^"]+$`
	ExcludeLabels []string `json:"excludeLabels,omitempty"`
}

// ProjectRef identifies a GitHub ProjectV2 and the fields the service uses.
type ProjectRef struct {
	// Owner is the org that owns the project.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`
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

// +kubebuilder:object:root=true

// FeedbackSourceList is a list of FeedbackSource.
type FeedbackSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FeedbackSource `json:"items"`
}

// FeedbackSource conditions and their reasons, written by feedback-sync.
const (
	// FeedbackSourceConditionReady is True when the last sweep synced every
	// matching issue.
	FeedbackSourceConditionReady = "Ready"

	// FeedbackSourceReasonSynced: the last sweep finished without errors.
	FeedbackSourceReasonSynced = "Synced"
	// FeedbackSourceReasonSyncFailed: the last sweep couldn't read GitHub or
	// write some posts; the message says what failed.
	FeedbackSourceReasonSyncFailed = "SyncFailed"
	// FeedbackSourceReasonRateLimited: the sweep is waiting for the
	// installation's GitHub rate limit to reset.
	FeedbackSourceReasonRateLimited = "RateLimited"
)
