package v1alpha1

import (
	"fmt"
	"net/url"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group served by the feedback apiserver.
const GroupName = "feedback.miloapis.com"

// SchemeGroupVersion is the group version used to register these objects.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

var (
	// SchemeBuilder collects the functions that add this group's types to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds this group's types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Kind returns a group-qualified GroupKind for the given kind.
func Kind(kind string) schema.GroupKind {
	return SchemeGroupVersion.WithKind(kind).GroupKind()
}

// Resource returns a group-qualified GroupResource for the given resource.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

// KnownTypes lists every object type in the group. The apiserver also
// registers these as the internal version.
func KnownTypes() []runtime.Object {
	return []runtime.Object{
		&FeedbackSource{}, &FeedbackSourceList{},
		&Post{}, &PostList{},
		&PostQuery{}, &PostQueryList{},
		&Vote{}, &VoteList{},
		&Subscription{}, &SubscriptionList{},
		&Submission{}, &SubmissionList{},
		&SubmissionReview{},
		&SubmissionToSecurityReport{},
		&SecurityReport{}, &SecurityReportList{},
		&SecurityAcknowledgement{}, &SecurityAcknowledgementList{},
		&Release{}, &ReleaseList{},
		&ChangelogEntry{}, &ChangelogEntryList{},
		&AttachmentUpload{}, &AttachmentUploadList{},
	}
}

// FieldSelectors lists the field selectors each kind supports, beyond
// metadata.name. Registries must expose the same fields from GetAttrs.
var FieldSelectors = map[string][]string{
	"FeedbackSource": {"spec.github.org"},
	"Post":           {"spec.hidden", "status.state", "status.github.org", "status.github.repository", "status.release"},
	"Vote":           {"spec.postRef.name"},
	"Subscription":   {"spec.postRef.name", "spec.reason"},
	"Submission":     {"spec.kind", "status.phase"},
	"SecurityReport": {"status.phase", "status.severity"},
	"Release":        {"status.shipped"},
}

// ParameterTypes are query parameter types for subresources. They are
// registered with a conversion from url.Values but aren't resources.
func ParameterTypes() []runtime.Object {
	return []runtime.Object{&AttachmentOptions{}}
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, KnownTypes()...)
	scheme.AddKnownTypes(SchemeGroupVersion, ParameterTypes()...)
	if err := scheme.AddConversionFunc((*url.Values)(nil), (*AttachmentOptions)(nil), func(a, b any, _ conversion.Scope) error {
		b.(*AttachmentOptions).Ref = a.(*url.Values).Get("ref")
		return nil
	}); err != nil {
		return err
	}
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return AddFieldLabelConversions(scheme, SchemeGroupVersion)
}

// AddFieldLabelConversions registers FieldSelectors for gv. The apiserver
// calls it again for the internal version.
func AddFieldLabelConversions(scheme *runtime.Scheme, gv schema.GroupVersion) error {
	for kind, fields := range FieldSelectors {
		allowed := append([]string{"metadata.name"}, fields...)
		err := scheme.AddFieldLabelConversionFunc(gv.WithKind(kind), func(label, value string) (string, string, error) {
			if slices.Contains(allowed, label) {
				return label, value, nil
			}
			return "", "", fmt.Errorf("field label not supported for %s: %s (supported: %v)", kind, label, allowed)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
