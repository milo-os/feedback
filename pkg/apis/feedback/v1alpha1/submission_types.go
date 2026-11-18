package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SubmissionKind is what the submitter says the feedback is.
// +kubebuilder:validation:Enum=idea;bug;other
type SubmissionKind string

const (
	SubmissionKindIdea  SubmissionKind = "idea"
	SubmissionKindBug   SubmissionKind = "bug"
	SubmissionKindOther SubmissionKind = "other"
)

// SubmissionPhase is where a submission is in review.
// +kubebuilder:validation:Enum=PendingReview;Approved;Merged;Rejected
type SubmissionPhase string

const (
	SubmissionPhasePendingReview SubmissionPhase = "PendingReview"
	SubmissionPhaseApproved      SubmissionPhase = "Approved"
	SubmissionPhaseMerged        SubmissionPhase = "Merged"
	SubmissionPhaseRejected      SubmissionPhase = "Rejected"
)

// ReviewDecision is a staff decision on a submission.
// +kubebuilder:validation:Enum=Approved;Merged;Rejected;MoveToSecurity
type ReviewDecision string

const (
	// ReviewDecisionApproved creates a public issue from the submission.
	ReviewDecisionApproved ReviewDecision = "Approved"
	// ReviewDecisionMerged adds the submitter to an existing post.
	ReviewDecisionMerged ReviewDecision = "Merged"
	// ReviewDecisionRejected closes the submission with a reason.
	ReviewDecisionRejected ReviewDecision = "Rejected"
	// ReviewDecisionMoveToSecurity moves a misfiled submission to the
	// security track: the operator creates a SecurityReport owned by the
	// submitter, copies the attachments, and deletes the submission.
	ReviewDecisionMoveToSecurity ReviewDecision = "MoveToSecurity"
)

// Submission is private feedback waiting for a staff decision. Submitters
// create and read their own in the user context; staff list them in the
// platform context and decide on its SubmissionReview. It holds only what the
// submitter may see. Nothing in a submission reaches GitHub unless staff
// approve it, and then only the staff-edited public title and body.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform;User"
// +kubebuilder:metadata:annotations="iam.miloapis.com/owner-field=spec.userRef.name"
// +kubebuilder:selectablefield:JSONPath=`.spec.userRef.name`
// +kubebuilder:selectablefield:JSONPath=`.spec.kind`
// +kubebuilder:selectablefield:JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.userRef.name`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Submission struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SubmissionSpec   `json:"spec"`
	Status SubmissionStatus `json:"status,omitempty"`
}

// SubmissionSpec is what the user submitted.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="submissions can't be changed after they are sent"
type SubmissionSpec struct {
	// UserRef is the submitter, set by the admission webhook.
	UserRef UserReference  `json:"userRef"`
	Kind    SubmissionKind `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	Title string `json:"title"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=20000
	Body string `json:"body"`
	// Attachments reference files uploaded to feedback-uploads.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=5
	Attachments []AttachmentReference `json:"attachments,omitempty"`
	// Context is filled by the client and never sent to GitHub.
	// +optional
	Context SubmissionContext `json:"context,omitempty"`
}

// SubmissionContext is private context about where the feedback came from.
type SubmissionContext struct {
	// +optional
	Client Client `json:"client,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	PageURL string `json:"pageURL,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Organization string `json:"organization,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Project string `json:"project,omitempty"`
	// RequestIDs are recent API request IDs, for debugging.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MaxLength=128
	RequestIDs []string `json:"requestIDs,omitempty"`
	// ClientVersion is e.g. cloud-portal@2026.10.1.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	ClientVersion string `json:"clientVersion,omitempty"`
}

// SubmissionStatus is written by the operator from the SubmissionReview, and
// is all the submitter learns about the review.
type SubmissionStatus struct {
	// +optional
	Phase SubmissionPhase `json:"phase,omitempty"`
	// Reason is shown to the submitter when the submission is Rejected.
	// +optional
	Reason string `json:"reason,omitempty"`
	// PostRef is the post the submission became or was merged into.
	// +optional
	PostRef *PostReference `json:"postRef,omitempty"`
}

// SubmissionList is a list of Submission.
// +kubebuilder:object:root=true
type SubmissionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Submission `json:"items"`
}

// SubmissionReview is the staff side of a Submission, with the same name. The
// operator creates it when the submission arrives and writes triage to its
// status. Staff decide by setting spec.decision, which can't change once set;
// the operator then applies the decision. Submitters can't read it.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
// +kubebuilder:selectablefield:JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="Reviewer",type=string,JSONPath=`.spec.reviewer`
// +kubebuilder:printcolumn:name="Applied",type=boolean,JSONPath=`.status.applied`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SubmissionReview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SubmissionReviewSpec   `json:"spec,omitempty"`
	Status SubmissionReviewStatus `json:"status,omitempty"`
}

// SubmissionReviewSpec is the staff decision. Empty until staff decide.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.decision) || self == oldSelf",message="a decision can't change once it is set"
// +kubebuilder:validation:XValidation:rule="!has(self.decision) || self.decision != 'Approved' || (has(self.publicTitle) && has(self.publicBody))",message="Approved needs publicTitle and publicBody"
// +kubebuilder:validation:XValidation:rule="!has(self.decision) || self.decision != 'Merged' || has(self.mergedInto)",message="Merged needs mergedInto"
// +kubebuilder:validation:XValidation:rule="!has(self.decision) || self.decision != 'Rejected' || has(self.reason)",message="Rejected needs a reason for the submitter"
type SubmissionReviewSpec struct {
	// +optional
	Decision ReviewDecision `json:"decision,omitempty"`
	// PublicTitle and PublicBody are what goes to GitHub on approval. Clients
	// default them to the triage draft.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	PublicTitle string `json:"publicTitle,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=20000
	PublicBody string `json:"publicBody,omitempty"`
	// MergedInto is the post name when the decision is Merged.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	MergedInto string `json:"mergedInto,omitempty"`
	// Reason is shown to the submitter when the decision is Rejected.
	// +optional
	// +kubebuilder:validation:MaxLength=2000
	Reason string `json:"reason,omitempty"`
	// Affected sets spec.affected on the SecurityReport for MoveToSecurity.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	Affected string `json:"affected,omitempty"`
	// Reviewer is the staff member who set the decision. The admission
	// webhook sets it from the request; it can't be set by clients.
	// +optional
	Reviewer string `json:"reviewer,omitempty"`
	// ReviewedAt is when the decision was set, by the admission webhook.
	// +optional
	ReviewedAt *metav1.Time `json:"reviewedAt,omitempty"`
}

// SubmissionReviewStatus is written by the operator.
type SubmissionReviewStatus struct {
	// Applied is set once the operator has carried out the decision.
	// +optional
	Applied bool `json:"applied,omitempty"`
	// Triage is advisory.
	// +optional
	Triage *SubmissionTriage `json:"triage,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SubmissionTriage is the operator's advisory triage.
type SubmissionTriage struct {
	// +optional
	// +listType=set
	SuggestedCategories []string `json:"suggestedCategories,omitempty"`
	// +optional
	SuggestedKind SubmissionKind `json:"suggestedKind,omitempty"`
	// SimilarPosts come from a keyword search on the submission.
	// +optional
	// +listType=atomic
	SimilarPosts []SimilarPost `json:"similarPosts,omitempty"`
	// DraftIssue is a public title and body with private details stripped.
	// +optional
	DraftIssue *DraftIssue `json:"draftIssue,omitempty"`
	// FraudScore is from the fraud service, 0 to 1, as a decimal string.
	// +optional
	FraudScore string `json:"fraudScore,omitempty"`
	// LikelySecurityIssue flags submissions that should be moved to the
	// security track.
	// +optional
	LikelySecurityIssue bool `json:"likelySecurityIssue,omitempty"`
}

// SimilarPost is a post that may cover the same feedback.
type SimilarPost struct {
	Name string `json:"name"`
}

// DraftIssue is a proposed public issue.
type DraftIssue struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// SubmissionReviewList is a list of SubmissionReview.
// +kubebuilder:object:root=true
type SubmissionReviewList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SubmissionReview `json:"items"`
}
