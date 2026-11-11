package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SubscriptionReason is why a user follows a post.
// +enum
type SubscriptionReason string

const (
	SubscriptionReasonVoted     SubscriptionReason = "voted"
	SubscriptionReasonSubmitted SubscriptionReason = "submitted"
	SubscriptionReasonMerged    SubscriptionReason = "merged"
	SubscriptionReasonManual    SubscriptionReason = "manual"
)

// +genclient
// +genclient:nonNamespaced
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Subscription links a user to a post they follow, in the user context. It is
// created when the user votes or their submission is approved or merged, and
// the user can delete it. The name must equal spec.postRef.name.
type Subscription struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SubscriptionSpec `json:"spec"`
}

// SubscriptionSpec identifies the post.
type SubscriptionSpec struct {
	PostRef PostReference `json:"postRef"`
	// +optional
	Reason SubscriptionReason `json:"reason,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SubscriptionList is a list of Subscription.
type SubscriptionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Subscription `json:"items"`
}
