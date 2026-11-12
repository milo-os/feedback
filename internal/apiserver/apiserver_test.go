package apiserver

import (
	"net/url"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// TestParameterCodec checks query parameters decode for the group's own
// option types and for the standard list options.
func TestParameterCodec(t *testing.T) {
	gv := feedbackv1alpha1.SchemeGroupVersion
	var opts feedbackv1alpha1.AttachmentOptions
	if err := ParameterCodec.DecodeParameters(url.Values{"ref": {"att-1"}}, gv, &opts); err != nil || opts.Ref != "att-1" {
		t.Fatalf("AttachmentOptions: %+v %v", opts, err)
	}
	var list metav1.ListOptions
	if err := ParameterCodec.DecodeParameters(url.Values{"labelSelector": {"a=b"}, "limit": {"5"}}, gv, &list); err != nil ||
		list.LabelSelector != "a=b" || list.Limit != 5 {
		t.Fatalf("ListOptions: %+v %v", list, err)
	}
}
