package v1alpha1

// Annotations on feedback CRDs that Milo reads.
const (
	// ParentContextsAnnotation lists the contexts a resource is discoverable
	// in.
	ParentContextsAnnotation = "discovery.miloapis.com/parent-contexts"
	// OwnerFieldAnnotation names the field holding the owner's user ID. Milo
	// limits user-context requests to the caller's objects.
	OwnerFieldAnnotation = "iam.miloapis.com/owner-field"
	// HiddenFieldAnnotation names a boolean that hides an object from
	// callers without get on the hidden subresource.
	HiddenFieldAnnotation = "iam.miloapis.com/hidden-field"
)

// Client is the surface a request came from.
// +kubebuilder:validation:Enum=website;portal;datumctl;patch;email;github
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
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	Name string `json:"name"`
}

// UserReference names a Milo User by its metadata.name.
type UserReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
}

// AttachmentReference points at a file uploaded to feedback-uploads.
type AttachmentReference struct {
	// Name is the file name shown to reviewers. Set by the admission webhook
	// from the upload.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name,omitempty"`
	// ContentType is the file's media type. Set by the admission webhook from
	// the upload.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	ContentType string `json:"contentType,omitempty"`
	// UploadRef is the reference feedback-uploads returned.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UploadRef string `json:"uploadRef"`
}

// GitHubLink identifies a GitHub issue, milestone or discussion.
type GitHubLink struct {
	// URL is the HTML URL on github.com.
	URL string `json:"url"`
	// Number is the issue, milestone or discussion number.
	Number int32 `json:"number"`
}

// OwnedName is the name of a user-owned object about a post: the post name
// and the user's name, so a user has at most one per post.
func OwnedName(post, user string) string {
	return post + "." + user
}
