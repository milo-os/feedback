package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecurityReportPhase is where a report is in handling.
// +kubebuilder:validation:Enum=Received;Triaged;Fixing;Fixed;Disclosed;NotApplicable;Duplicate
type SecurityReportPhase string

const (
	SecurityReportPhaseReceived      SecurityReportPhase = "Received"
	SecurityReportPhaseTriaged       SecurityReportPhase = "Triaged"
	SecurityReportPhaseFixing        SecurityReportPhase = "Fixing"
	SecurityReportPhaseFixed         SecurityReportPhase = "Fixed"
	SecurityReportPhaseDisclosed     SecurityReportPhase = "Disclosed"
	SecurityReportPhaseNotApplicable SecurityReportPhase = "NotApplicable"
	SecurityReportPhaseDuplicate     SecurityReportPhase = "Duplicate"
)

// Severity is the Security Team's rating.
// +kubebuilder:validation:Enum=critical;high;medium;low;informational
type Severity string

const (
	SeverityCritical      Severity = "critical"
	SeverityHigh          Severity = "high"
	SeverityMedium        Severity = "medium"
	SeverityLow           Severity = "low"
	SeverityInformational Severity = "informational"
)

// RewardKind is how a reporter is thanked.
// +kubebuilder:validation:Enum=none;thanks;swag;credits
type RewardKind string

const (
	RewardKindNone    RewardKind = "none"
	RewardKindThanks  RewardKind = "thanks"
	RewardKindSwag    RewardKind = "swag"
	RewardKindCredits RewardKind = "credits"
)

// SecurityReport is a private vulnerability report. Reporters create and read
// their own in the user context. Only the Security Team role can list every
// report and write status; what the reporter must not see lives on its
// SecurityReportTriage. Reports are never synced to GitHub, indexed or sent
// to the AI gateway, and are audited at Metadata level only.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform;User"
// +kubebuilder:metadata:annotations="iam.miloapis.com/owner-field=spec.userRef.name"
// +kubebuilder:selectablefield:JSONPath=`.spec.userRef.name`
// +kubebuilder:selectablefield:JSONPath=`.status.phase`
// +kubebuilder:selectablefield:JSONPath=`.status.severity`
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.status.severity`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SecurityReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SecurityReportSpec   `json:"spec"`
	Status SecurityReportStatus `json:"status,omitempty"`
}

// SecurityReportSpec is what the reporter submitted.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="reports can't be changed after they are sent"
type SecurityReportSpec struct {
	// UserRef is the reporter, set by the admission webhook. Empty for reports
	// the Security Team logs for someone without a Datum account.
	// +optional
	UserRef *UserReference `json:"userRef,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	Title string `json:"title"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=50000
	Description string `json:"description"`
	// Affected is free text that helps routing, e.g. "connectors".
	// +optional
	// +kubebuilder:validation:MaxLength=200
	Affected string `json:"affected,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=50000
	StepsToReproduce string `json:"stepsToReproduce,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=10000
	Impact string `json:"impact,omitempty"`
	// Attachments reference files uploaded to feedback-uploads.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=5
	Attachments []AttachmentReference `json:"attachments,omitempty"`
	// Credit is how the reporter wants to be credited.
	// +optional
	Credit *SecurityCredit `json:"credit,omitempty"`
	// Reporter is set only on reports the Security Team logs for someone
	// without a Datum account (from email or GitHub private reporting).
	// +optional
	Reporter *ExternalReporter `json:"reporter,omitempty"`
	// +optional
	Context SecurityReportContext `json:"context,omitempty"`
}

// SecurityCredit is the reporter's credit preference.
type SecurityCredit struct {
	// Public lists the reporter on the acknowledgements page once the report
	// is Fixed or Disclosed.
	// +optional
	Public bool `json:"public,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	URL string `json:"url,omitempty"`
}

// ExternalReporter identifies a reporter outside Datum.
type ExternalReporter struct {
	// +optional
	Email string `json:"email,omitempty"`
	// +optional
	GitHubLogin string `json:"githubLogin,omitempty"`
}

// SecurityReportContext is where the report came from.
type SecurityReportContext struct {
	// +optional
	Client Client `json:"client,omitempty"`
	// FiledByAgent is set when an agent (Patch, non-interactive datumctl)
	// filed the report.
	// +optional
	FiledByAgent bool `json:"filedByAgent,omitempty"`
	// +optional
	Organization string `json:"organization,omitempty"`
	// +optional
	ClientVersion string `json:"clientVersion,omitempty"`
}

// SecurityReportStatus is written by the Security Team.
type SecurityReportStatus struct {
	// +optional
	Phase SecurityReportPhase `json:"phase,omitempty"`
	// +optional
	Severity Severity `json:"severity,omitempty"`
	// +optional
	CVSSVector string `json:"cvssVector,omitempty"`
	// Reason is shown to the reporter for NotApplicable and Duplicate.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Timeline is visible to the reporter.
	// +optional
	// +listType=atomic
	Timeline []SecurityTimelineEntry `json:"timeline,omitempty"`
	// +optional
	Advisory *SecurityAdvisory `json:"advisory,omitempty"`
	// +optional
	Reward *SecurityReward `json:"reward,omitempty"`
}

// SecurityTimelineEntry is one phase change.
type SecurityTimelineEntry struct {
	Phase SecurityReportPhase `json:"phase"`
	At    metav1.Time         `json:"at"`
	// +optional
	Note string `json:"note,omitempty"`
}

// SecurityAdvisory is the published advisory.
type SecurityAdvisory struct {
	// +optional
	URL string `json:"url,omitempty"`
	// +optional
	CVE string `json:"cve,omitempty"`
}

// SecurityReward records how the reporter was thanked.
type SecurityReward struct {
	Kind RewardKind `json:"kind"`
	// Amount is the credit amount, for kind credits.
	// +optional
	Amount string `json:"amount,omitempty"`
	// CreditGrantRef names the billing CreditGrant, for kind credits.
	// +optional
	CreditGrantRef *CreditGrantReference `json:"creditGrantRef,omitempty"`
	// +optional
	GrantedAt *metav1.Time `json:"grantedAt,omitempty"`
}

// CreditGrantReference names a billing.miloapis.com CreditGrant.
type CreditGrantReference struct {
	Name string `json:"name"`
}

// SecurityReportTriage is the Security Team's private notes on a report,
// with the same name as the report. Only the Security Team can read or write
// it, so nothing here reaches the reporter.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
// +kubebuilder:printcolumn:name="Assignee",type=string,JSONPath=`.spec.assignee`
// +kubebuilder:printcolumn:name="Duplicate of",type=string,JSONPath=`.spec.duplicateOf`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SecurityReportTriage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SecurityReportTriageSpec `json:"spec,omitempty"`
}

// SecurityReportTriageSpec is Security Team only.
type SecurityReportTriageSpec struct {
	// +optional
	Assignee string `json:"assignee,omitempty"`
	// DuplicateOf is the name of the original report.
	// +optional
	DuplicateOf string `json:"duplicateOf,omitempty"`
	// Tracking links the private issue or draft advisory for the fix.
	// +optional
	Tracking string `json:"tracking,omitempty"`
	// +optional
	Notes string `json:"notes,omitempty"`
}

// SecurityReportTriageList is a list of SecurityReportTriage.
// +kubebuilder:object:root=true
type SecurityReportTriageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SecurityReportTriage `json:"items"`
}

// SecurityReportList is a list of SecurityReport.
// +kubebuilder:object:root=true
type SecurityReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SecurityReport `json:"items"`
}

// SecurityAcknowledgement is a public credit for a fixed or disclosed
// SecurityReport whose reporter chose public credit, with the same name as
// the report. The operator creates and updates it; it holds only what the
// acknowledgements page shows.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.status.name`
// +kubebuilder:printcolumn:name="Month",type=string,JSONPath=`.status.month`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.status.severity`
type SecurityAcknowledgement struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status SecurityAcknowledgementStatus `json:"status,omitempty"`
}

// SecurityAcknowledgementStatus is the public credit.
type SecurityAcknowledgementStatus struct {
	Name string `json:"name"`
	// +optional
	URL string `json:"url,omitempty"`
	// Month is when the report was fixed, as YYYY-MM.
	Month string `json:"month"`
	// +optional
	Severity Severity `json:"severity,omitempty"`
}

// SecurityAcknowledgementList is a list of SecurityAcknowledgement.
// +kubebuilder:object:root=true
type SecurityAcknowledgementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SecurityAcknowledgement `json:"items"`
}
