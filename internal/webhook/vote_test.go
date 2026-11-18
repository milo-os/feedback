package webhook

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestVoteCheck(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	posts := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "open"}},
		&v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "hidden"}, Spec: v1.PostSpec{Hidden: true}},
		&v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "merged"}, Status: v1.PostStatus{MergedInto: "open"}},
	).Build()
	c := VoteCheck{Posts: posts}

	verified := inUserContext("u1")
	verified.Extra[identity.EmailVerifiedExtraKey] = authenticationv1.ExtraValue{"true"}
	unverified := inUserContext("u1")
	unverified.Extra[identity.EmailVerifiedExtraKey] = authenticationv1.ExtraValue{"false"}

	onPost := func(post string) *v1.Vote {
		return &v1.Vote{Spec: v1.VoteSpec{PostRef: v1.PostReference{Name: post}, UserRef: v1.UserReference{Name: "u1"}}}
	}
	for _, tc := range []struct {
		name, resource, post string
		user                 authenticationv1.UserInfo
		denied               string
	}{
		{"vote", "votes", "open", verified, ""},
		{"unverified email", "votes", "open", unverified, "verify your email"},
		{"unverified can still follow", "subscriptions", "open", unverified, ""},
		{"missing post", "votes", "nope", verified, "not found"},
		{"hidden post looks missing", "votes", "hidden", verified, "not found"},
		{"merged post", "subscriptions", "merged", verified, `merged into "open"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := c.Handle(context.Background(), request(t, admissionv1.Create, tc.resource, tc.user, onPost(tc.post), nil))
			if tc.denied == "" {
				if !resp.Allowed {
					t.Errorf("denied: %v", resp.Result.Message)
				}
				return
			}
			if resp.Allowed || !strings.Contains(resp.Result.Message, tc.denied) {
				t.Errorf("allowed %v, message %q; want denial containing %q", resp.Allowed, resp.Result.Message, tc.denied)
			}
		})
	}
}
