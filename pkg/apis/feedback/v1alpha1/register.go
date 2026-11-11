package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&FeedbackSource{},
		&FeedbackSourceList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("FeedbackSource"), feedbackSourceFieldLabel)
}

// feedbackSourceFieldLabel lists the field selectors FeedbackSource supports.
func feedbackSourceFieldLabel(label, value string) (string, string, error) {
	switch label {
	case "metadata.name", "spec.github.org":
		return label, value, nil
	}
	return "", "", fmt.Errorf("field label not supported: %s", label)
}
