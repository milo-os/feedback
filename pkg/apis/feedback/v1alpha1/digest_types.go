package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DigestEvent is something a follower or submitter is told about.
// +kubebuilder:validation:Enum=picked-up;part-shipped;released;closed;approved;merged;rejected
type DigestEvent string

const (
	DigestEventPickedUp    DigestEvent = "picked-up"
	DigestEventPartShipped DigestEvent = "part-shipped"
	DigestEventReleased    DigestEvent = "released"
	DigestEventClosed      DigestEvent = "closed"
	DigestEventApproved    DigestEvent = "approved"
	DigestEventMerged      DigestEvent = "merged"
	DigestEventRejected    DigestEvent = "rejected"
)

// FeedbackDigest is one user's queue of events for their next daily digest,
// named after the user. Only the operator writes it; staff can read it. The
// operator clears it once the digest Email is created.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
// +kubebuilder:printcolumn:name="Items",type=integer,JSONPath=`.spec.itemCount`
// +kubebuilder:printcolumn:name="Last sent",type=date,JSONPath=`.status.lastSentAt`
type FeedbackDigest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FeedbackDigestSpec   `json:"spec"`
	Status FeedbackDigestStatus `json:"status,omitempty"`
}

// FeedbackDigestSpec is the pending queue.
type FeedbackDigestSpec struct {
	UserRef UserReference `json:"userRef"`
	// Items holds at most one line per post or submission; a later event
	// replaces an earlier one.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=500
	Items []DigestItem `json:"items,omitempty"`
	// ItemCount is len(items), for the printer column.
	// +optional
	ItemCount int32 `json:"itemCount,omitempty"`
}

// DigestItem is one line of a digest.
type DigestItem struct {
	Event DigestEvent `json:"event"`
	// Post is the post the event is about.
	// +optional
	Post string `json:"post,omitempty"`
	// Submission is the submission the event is about, for review outcomes.
	// +optional
	Submission string `json:"submission,omitempty"`
	// Changelog is the ChangelogEntry for part-shipped and released.
	// +optional
	Changelog string `json:"changelog,omitempty"`
	At        metav1.Time `json:"at"`
}

// FeedbackDigestStatus records sends.
type FeedbackDigestStatus struct {
	// +optional
	LastSentAt *metav1.Time `json:"lastSentAt,omitempty"`
	// LastEmail is the name of the last Email created.
	// +optional
	LastEmail string `json:"lastEmail,omitempty"`
}

// FeedbackDigestList is a list of FeedbackDigest.
// +kubebuilder:object:root=true
type FeedbackDigestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FeedbackDigest `json:"items"`
}
