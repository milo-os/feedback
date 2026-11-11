package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SubmissionKind is what the submitter says the feedback is.
// +enum
type SubmissionKind string

const (
	SubmissionKindIdea  SubmissionKind = "idea"
	SubmissionKindBug   SubmissionKind = "bug"
	SubmissionKindOther SubmissionKind = "other"
)

// SubmissionPhase is where a submission is in review.
// +enum
type SubmissionPhase string

const (
	SubmissionPhasePendingReview SubmissionPhase = "PendingReview"
	SubmissionPhaseApproved      SubmissionPhase = "Approved"
	SubmissionPhaseMerged        SubmissionPhase = "Merged"
	SubmissionPhaseRejected      SubmissionPhase = "Rejected"
)

// ReviewDecision is a staff decision on a submission.
// +enum
type ReviewDecision string

const (
	// ReviewDecisionApproved creates a public issue from the submission.
	ReviewDecisionApproved ReviewDecision = "Approved"
	// ReviewDecisionMerged adds the submitter to an existing post.
	ReviewDecisionMerged ReviewDecision = "Merged"
	// ReviewDecisionRejected closes the submission with a reason.
	ReviewDecisionRejected ReviewDecision = "Rejected"
)

// +genclient
// +genclient:nonNamespaced
// +genclient:method=Review,verb=create,subresource=review,input=SubmissionReview,result=Submission
// +genclient:method=ToSecurityReport,verb=create,subresource=to-security-report,input=SubmissionToSecurityReport,result=SecurityReport
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Submission is private feedback waiting for a staff decision. Submitters
// create and read their own in the user context; staff list and review them
// in the platform context. Nothing in a submission reaches GitHub unless staff
// approve it, and then only the staff-edited public title and body.
type Submission struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SubmissionSpec   `json:"spec"`
	Status SubmissionStatus `json:"status,omitempty"`
}

// SubmissionSpec is what the user submitted.
type SubmissionSpec struct {
	Kind  SubmissionKind `json:"kind"`
	Title string         `json:"title"`
	Body  string         `json:"body"`
	// Attachments reference files uploaded with AttachmentUpload.
	// +optional
	// +listType=atomic
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
	PageURL string `json:"pageURL,omitempty"`
	// +optional
	Organization string `json:"organization,omitempty"`
	// +optional
	Project string `json:"project,omitempty"`
	// RequestIDs are recent API request IDs, for debugging.
	// +optional
	// +listType=atomic
	RequestIDs []string `json:"requestIDs,omitempty"`
	// ClientVersion is e.g. cloud-portal@2026.10.1.
	// +optional
	ClientVersion string `json:"clientVersion,omitempty"`
}

// SubmissionStatus is written by the server, feedback-controller,
// feedback-sync and the review subresource. Submitters see phase, postRef and
// review.reason only.
type SubmissionStatus struct {
	// +optional
	Phase SubmissionPhase `json:"phase,omitempty"`
	// Triage is advisory, written by feedback-controller. Staff only.
	// +optional
	Triage *SubmissionTriage `json:"triage,omitempty"`
	// Review is the staff decision, written through the review subresource.
	// +optional
	Review *SubmissionReviewOutcome `json:"review,omitempty"`
	// PostRef is the post the submission became or was merged into. Set by
	// feedback-sync once the post syncs.
	// +optional
	PostRef *PostReference `json:"postRef,omitempty"`
}

// SubmissionTriage is the controller's advisory triage.
type SubmissionTriage struct {
	// +optional
	// +listType=set
	SuggestedCategories []string `json:"suggestedCategories,omitempty"`
	// +optional
	SuggestedKind SubmissionKind `json:"suggestedKind,omitempty"`
	// +optional
	// +listType=atomic
	SimilarPosts []SimilarPost `json:"similarPosts,omitempty"`
	// DraftIssue is a public title and body with private details stripped.
	// +optional
	DraftIssue *DraftIssue `json:"draftIssue,omitempty"`
	// FraudScore is from the fraud service, 0 to 1.
	// +optional
	FraudScore *float64 `json:"fraudScore,omitempty"`
	// LikelySecurityIssue flags submissions that should be moved to the
	// security track with the to-security-report subresource.
	// +optional
	LikelySecurityIssue bool `json:"likelySecurityIssue,omitempty"`
}

// SimilarPost is a post that may cover the same feedback.
type SimilarPost struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
}

// DraftIssue is a proposed public issue.
type DraftIssue struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// SubmissionReviewOutcome records the staff decision.
type SubmissionReviewOutcome struct {
	// Reviewer is the staff member who decided. Staff only.
	// +optional
	Reviewer   string         `json:"reviewer,omitempty"`
	Decision   ReviewDecision `json:"decision"`
	ReviewedAt metav1.Time    `json:"reviewedAt"`
	// PublicTitle and PublicBody are what goes to GitHub on approval.
	// +optional
	PublicTitle string `json:"publicTitle,omitempty"`
	// +optional
	PublicBody string `json:"publicBody,omitempty"`
	// MergedInto is the post name when the decision is Merged.
	// +optional
	MergedInto string `json:"mergedInto,omitempty"`
	// Reason is shown to the submitter when the decision is Rejected.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SubmissionList is a list of Submission.
type SubmissionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Submission `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SubmissionReview is the body of a create on submissions/{name}/review.
// Only staff in the platform context can create one. The response is the
// updated Submission.
type SubmissionReview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SubmissionReviewSpec `json:"spec"`
}

// SubmissionReviewSpec is the decision.
type SubmissionReviewSpec struct {
	Decision ReviewDecision `json:"decision"`
	// PublicTitle and PublicBody are required for Approved. They default to
	// the triage draft in clients.
	// +optional
	PublicTitle string `json:"publicTitle,omitempty"`
	// +optional
	PublicBody string `json:"publicBody,omitempty"`
	// MergedInto is required for Merged.
	// +optional
	MergedInto string `json:"mergedInto,omitempty"`
	// Reason is required for Rejected and shown to the submitter.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SubmissionToSecurityReport is the body of a create on
// submissions/{name}/to-security-report. It moves a misfiled submission to the
// security track: a SecurityReport is created from it, its attachments move,
// and the submission is deleted, in one transaction. The response is the new
// SecurityReport.
type SubmissionToSecurityReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Affected optionally sets spec.affected on the new report.
	// +optional
	Affected string `json:"affected,omitempty"`
}
