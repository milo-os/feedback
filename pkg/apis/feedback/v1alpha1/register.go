package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group of every feedback resource.
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

// KnownTypes lists every object type in the group.
func KnownTypes() []runtime.Object {
	return []runtime.Object{
		&FeedbackSource{}, &FeedbackSourceList{},
		&Post{}, &PostList{},
		&Release{}, &ReleaseList{},
		&ChangelogEntry{}, &ChangelogEntryList{},
		&Vote{}, &VoteList{},
		&Subscription{}, &SubscriptionList{},
		&Submission{}, &SubmissionList{},
		&SubmissionReview{}, &SubmissionReviewList{},
		&SecurityReport{}, &SecurityReportList{},
		&SecurityReportTriage{}, &SecurityReportTriageList{},
		&SecurityAcknowledgement{}, &SecurityAcknowledgementList{},
		&FeedbackProfile{}, &FeedbackProfileList{},
		&FeedbackDigest{}, &FeedbackDigestList{},
	}
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, KnownTypes()...)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
