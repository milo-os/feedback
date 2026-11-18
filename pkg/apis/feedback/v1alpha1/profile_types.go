package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FeedbackProfile holds a user's feedback settings. Users create and update
// their own in the user context; it is named after the user.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform;User"
// +kubebuilder:metadata:annotations="iam.miloapis.com/owner-field=spec.userRef.name"
// +kubebuilder:selectablefield:JSONPath=`.spec.userRef.name`
// +kubebuilder:printcolumn:name="Public votes",type=boolean,JSONPath=`.spec.showPublicVotes`
// +kubebuilder:printcolumn:name="Emails",type=boolean,JSONPath=`.spec.digestEmails`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == self.spec.userRef.name",message="metadata.name must be the user's name"
type FeedbackProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec FeedbackProfileSpec `json:"spec"`
}

// FeedbackProfileSpec is the user's settings.
type FeedbackProfileSpec struct {
	// UserRef is the user, set by the admission webhook.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="userRef is immutable"
	UserRef UserReference `json:"userRef"`
	// ShowPublicVotes shows the user's avatar and name on posts they voted
	// on, including past votes.
	// +optional
	ShowPublicVotes bool `json:"showPublicVotes,omitempty"`
	// DigestEmails turns the daily digest on or off. Unset means on.
	// +optional
	DigestEmails *bool `json:"digestEmails,omitempty"`
}

// FeedbackProfileList is a list of FeedbackProfile.
// +kubebuilder:object:root=true
type FeedbackProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FeedbackProfile `json:"items"`
}
