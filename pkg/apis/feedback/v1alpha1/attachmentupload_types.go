package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AttachmentPurpose says what an upload will be attached to. Security report
// uploads are stored under a separate prefix that only the reporter and the
// Security Team can read.
// +enum
type AttachmentPurpose string

const (
	AttachmentPurposeSubmission     AttachmentPurpose = "Submission"
	AttachmentPurposeSecurityReport AttachmentPurpose = "SecurityReport"
)

// +genclient
// +genclient:nonNamespaced
// +genclient:onlyVerbs=create
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// AttachmentUpload stores a file in private object storage and returns a
// reference to use in a Submission or SecurityReport. Create it in the user
// context before creating the resource that uses it. It is not stored as an
// API object; the file is only served back to its uploader and staff (or the
// Security Team, for security reports).
//
// The design proposed attachments subresources on submissions and security
// reports, but a subresource needs its parent to exist first. Uploads happen
// before the parent is created, so they are a create-only resource instead.
type AttachmentUpload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AttachmentUploadSpec   `json:"spec"`
	Status AttachmentUploadStatus `json:"status,omitempty"`
}

// AttachmentUploadSpec is the file.
type AttachmentUploadSpec struct {
	Purpose AttachmentPurpose `json:"purpose"`
	// Name is the file name.
	Name string `json:"name"`
	// ContentType must be an allowed type, e.g. image/png or text/plain.
	ContentType string `json:"contentType"`
	// Data is the file content, base64-encoded in JSON. Size limits apply.
	Data []byte `json:"data"`
}

// AttachmentUploadStatus identifies the stored file.
type AttachmentUploadStatus struct {
	// UploadRef goes in AttachmentReference.uploadRef.
	// +optional
	UploadRef string `json:"uploadRef,omitempty"`
	// +optional
	Size int64 `json:"size,omitempty"`
	// SHA256 is the hex digest of the content.
	// +optional
	SHA256 string `json:"sha256,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// AttachmentUploadList is required by the API machinery; uploads cannot be
// listed.
type AttachmentUploadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AttachmentUpload `json:"items"`
}
