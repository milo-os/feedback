package v1alpha1

// Client is the surface a request came from.
// +enum
type Client string

const (
	ClientWebsite  Client = "website"
	ClientPortal   Client = "portal"
	ClientDatumctl Client = "datumctl"
	ClientPatch    Client = "patch"
	// ClientEmail and ClientGitHub are only used for security reports the
	// Security Team logs on behalf of an external reporter.
	ClientEmail  Client = "email"
	ClientGitHub Client = "github"
)

// PostReference names a Post.
type PostReference struct {
	// Name of the Post, e.g. datum-cloud.enhancements.912.
	Name string `json:"name"`
}

// AttachmentReference points at a file uploaded with an AttachmentUpload.
type AttachmentReference struct {
	// Name is the file name shown to reviewers.
	Name string `json:"name"`
	// ContentType is the file's media type.
	// +optional
	ContentType string `json:"contentType,omitempty"`
	// UploadRef is status.uploadRef from the AttachmentUpload.
	UploadRef string `json:"uploadRef"`
}

// GitHubLink identifies a GitHub issue, milestone or discussion.
type GitHubLink struct {
	// URL is the HTML URL on github.com.
	URL string `json:"url"`
	// Number is the issue, milestone or discussion number.
	Number int32 `json:"number"`
}
