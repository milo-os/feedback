package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Vote is a user's vote on a post. Users create and delete their own in the
// user context; staff list every vote in the platform context.
//
// Every user's votes share one collection, so the name is
// "<post>.<user>" (see OwnedName), which allows one vote per user per post.
// The admission webhook sets spec.userRef from the request. The operator
// follows the post for the voter and recomputes the post's totals.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform;User"
// +kubebuilder:metadata:annotations="iam.miloapis.com/owner-field=spec.userRef.name"
// +kubebuilder:selectablefield:JSONPath=`.spec.userRef.name`
// +kubebuilder:selectablefield:JSONPath=`.spec.postRef.name`
// +kubebuilder:printcolumn:name="Post",type=string,JSONPath=`.spec.postRef.name`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.userRef.name`,priority=1
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == self.spec.postRef.name + '.' + self.spec.userRef.name",message="metadata.name must be <spec.postRef.name>.<spec.userRef.name>"
type Vote struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VoteSpec   `json:"spec"`
	Status VoteStatus `json:"status,omitempty"`
}

// VoteSpec identifies the post and where the vote came from.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; delete the vote and vote again"
type VoteSpec struct {
	PostRef PostReference `json:"postRef"`
	// UserRef is the voter, set by the admission webhook.
	UserRef UserReference `json:"userRef"`
	// Source is the client the vote was cast from.
	// +optional
	Source Client `json:"source,omitempty"`
}

// VoteStatus is written by the operator.
type VoteStatus struct {
	// Organizations the voter belonged to when voting. Used for staff
	// analytics and never public.
	// +optional
	// +listType=set
	Organizations []string `json:"organizations,omitempty"`
	// GitHubLogin is the GitHub username linked to the voter's account when
	// they voted, so a 👍 from the same person isn't counted twice. Never
	// public.
	// +optional
	GitHubLogin string `json:"githubLogin,omitempty"`
	// Subscribed is set once the operator has followed the post for the
	// voter, so a follow the voter later deletes isn't recreated.
	// +optional
	Subscribed bool `json:"subscribed,omitempty"`
}

// VoteList is a list of Vote.
// +kubebuilder:object:root=true
type VoteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Vote `json:"items"`
}
