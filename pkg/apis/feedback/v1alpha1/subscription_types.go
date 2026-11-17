package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SubscriptionReason is why a user follows a post.
// +kubebuilder:validation:Enum=voted;submitted;merged;manual
type SubscriptionReason string

const (
	SubscriptionReasonVoted     SubscriptionReason = "voted"
	SubscriptionReasonSubmitted SubscriptionReason = "submitted"
	SubscriptionReasonMerged    SubscriptionReason = "merged"
	SubscriptionReasonManual    SubscriptionReason = "manual"
)

// Subscription links a user to a post they follow. The operator creates one
// when a user votes or their submission is approved or merged; users create
// and delete their own in the user context. Named like a Vote.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform;User"
// +kubebuilder:metadata:annotations="iam.miloapis.com/owner-field=spec.userRef.name"
// +kubebuilder:selectablefield:JSONPath=`.spec.userRef.name`
// +kubebuilder:selectablefield:JSONPath=`.spec.postRef.name`
// +kubebuilder:printcolumn:name="Post",type=string,JSONPath=`.spec.postRef.name`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.userRef.name`,priority=1
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == self.spec.postRef.name + '.' + self.spec.userRef.name",message="metadata.name must be <spec.postRef.name>.<spec.userRef.name>"
type Subscription struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SubscriptionSpec `json:"spec"`
}

// SubscriptionSpec identifies the post and the follower.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type SubscriptionSpec struct {
	PostRef PostReference `json:"postRef"`
	// UserRef is the follower, set by the admission webhook.
	UserRef UserReference `json:"userRef"`
	// Reason is "manual" when the user follows a post themselves; the operator
	// sets the others.
	// +optional
	Reason SubscriptionReason `json:"reason,omitempty"`
}

// SubscriptionList is a list of Subscription.
// +kubebuilder:object:root=true
type SubscriptionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Subscription `json:"items"`
}
