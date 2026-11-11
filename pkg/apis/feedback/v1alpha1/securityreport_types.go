package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecurityReportPhase is where a report is in handling.
// +enum
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
// +enum
type Severity string

const (
	SeverityCritical      Severity = "critical"
	SeverityHigh          Severity = "high"
	SeverityMedium        Severity = "medium"
	SeverityLow           Severity = "low"
	SeverityInformational Severity = "informational"
)

// RewardKind is how a reporter is thanked.
// +enum
type RewardKind string

const (
	RewardKindNone    RewardKind = "none"
	RewardKindThanks  RewardKind = "thanks"
	RewardKindSwag    RewardKind = "swag"
	RewardKindCredits RewardKind = "credits"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SecurityReport is a private vulnerability report. Reporters create and read
// their own in the user context, without status.internal. Only the Security
// Team role can list reports and write status. Reports are never synced to
// GitHub, embedded, indexed or sent to the AI gateway, and are audited at
// Metadata level only.
type SecurityReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SecurityReportSpec   `json:"spec"`
	Status SecurityReportStatus `json:"status,omitempty"`
}

// SecurityReportSpec is what the reporter submitted.
type SecurityReportSpec struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// Affected is free text that helps routing, e.g. "connectors".
	// +optional
	Affected string `json:"affected,omitempty"`
	// +optional
	StepsToReproduce string `json:"stepsToReproduce,omitempty"`
	// +optional
	Impact string `json:"impact,omitempty"`
	// Attachments reference files uploaded with AttachmentUpload for a
	// security report.
	// +optional
	// +listType=atomic
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
	// Internal is visible to the Security Team only.
	// +optional
	Internal *SecurityReportInternal `json:"internal,omitempty"`
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

// SecurityReportInternal is Security Team only.
type SecurityReportInternal struct {
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

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SecurityReportList is a list of SecurityReport.
type SecurityReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SecurityReport `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +genclient:onlyVerbs=get,list
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SecurityAcknowledgement is a read-only public credit for a fixed or
// disclosed SecurityReport whose reporter chose public credit. It holds only
// what the acknowledgements page shows.
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

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SecurityAcknowledgementList is a list of SecurityAcknowledgement.
type SecurityAcknowledgementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SecurityAcknowledgement `json:"items"`
}
