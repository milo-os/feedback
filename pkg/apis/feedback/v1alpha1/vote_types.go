package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Vote is a user's vote on a post, in the user context. The name must equal
// spec.postRef.name, so a user votes at most once per post. Deleting the Vote
// removes the vote. Creating one also creates a Subscription to the post.
type Vote struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VoteSpec   `json:"spec"`
	Status VoteStatus `json:"status,omitempty"`
}

// VoteSpec identifies the post and where the vote came from.
type VoteSpec struct {
	PostRef PostReference `json:"postRef"`
	// Source is the client the vote was cast from.
	// +optional
	Source Client `json:"source,omitempty"`
}

// VoteStatus is written by the server.
type VoteStatus struct {
	// Organizations the voter belonged to when voting. Used for staff
	// analytics and never public.
	// +optional
	// +listType=set
	Organizations []string `json:"organizations,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VoteList is a list of Vote.
type VoteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Vote `json:"items"`
}
