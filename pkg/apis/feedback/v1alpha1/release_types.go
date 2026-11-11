package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Release maps to a milestone in the FeedbackSource's milestones repository.
// Only feedback-sync writes releases; everyone can read them.
type Release struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status ReleaseStatus `json:"status,omitempty"`
}

// ReleaseStatus is synced from the milestone.
type ReleaseStatus struct {
	// Title is the milestone title, e.g. "Hedy: 2.0".
	Title string `json:"title"`
	// +optional
	Codename string `json:"codename,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
	// DueOn is the milestone due date, as YYYY-MM-DD.
	// +optional
	DueOn string `json:"dueOn,omitempty"`
	// Shipped is set when the milestone is closed.
	// +optional
	Shipped bool `json:"shipped,omitempty"`
	// +optional
	DescriptionHTML string `json:"descriptionHTML,omitempty"`
	// CoverImage is a key into the website's static assets.
	// +optional
	CoverImage string `json:"coverImage,omitempty"`
	// Posts are the names of posts on the milestone.
	// +optional
	// +listType=set
	Posts []string `json:"posts,omitempty"`
	// +optional
	GitHub *GitHubLink `json:"github,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ReleaseList is a list of Release.
type ReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Release `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ChangelogEntry maps to a discussion in the FeedbackSource's changelog
// category. Only feedback-sync writes entries; everyone can read them.
type ChangelogEntry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status ChangelogEntryStatus `json:"status,omitempty"`
}

// ChangelogEntryStatus is synced from the discussion.
type ChangelogEntryStatus struct {
	Title       string      `json:"title"`
	PublishedAt metav1.Time `json:"publishedAt"`
	// Categories are mapped from discussion labels.
	// +optional
	// +listType=set
	Categories []string `json:"categories,omitempty"`
	// BodyHTML is the discussion body, sanitised like post bodies.
	// +optional
	BodyHTML string `json:"bodyHTML,omitempty"`
	// Posts are the names of posts referenced in the body.
	// +optional
	// +listType=set
	Posts []string `json:"posts,omitempty"`
	// IssueRefs are every issue referenced in the body, as
	// "<org>/<repo>#<number>". A reference to a post's work item rather than
	// the post marks that post part-shipped.
	// +optional
	// +listType=set
	IssueRefs []string `json:"issueRefs,omitempty"`
	// +optional
	Reactions ChangelogReactions `json:"reactions,omitempty"`
	// +optional
	GitHub *GitHubLink `json:"github,omitempty"`
}

// ChangelogReactions are reaction counts on the discussion.
type ChangelogReactions struct {
	// +optional
	ThumbsUp int32 `json:"thumbsUp,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ChangelogEntryList is a list of ChangelogEntry.
type ChangelogEntryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ChangelogEntry `json:"items"`
}
