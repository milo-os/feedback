package webhook

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestAttachments(t *testing.T) {
	ctx := context.Background()
	files, err := attachments.Open(ctx, "mem://")
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := files.Upload(ctx, attachments.Submission, "u1", "notes.txt", "text/plain", []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	sub := func(ref string) *v1.Submission {
		return &v1.Submission{Spec: v1.SubmissionSpec{
			UserRef: v1.UserReference{Name: "u1"}, Kind: v1.SubmissionKindBug, Title: "t", Body: "b",
			// The client's name and type are ignored.
			Attachments: []v1.AttachmentReference{{UploadRef: ref, Name: "evil.html", ContentType: "text/html"}},
		}}
	}
	a := Attachments{Files: files}

	req := request(t, admissionv1.Create, "submissions", inUserContext("u1"), sub(ref), nil)
	resp := a.Handle(ctx, req)
	if !resp.Allowed {
		t.Fatalf("own upload denied: %v", resp.Result.Message)
	}
	var got v1.Submission
	patched(t, req, resp, &got)
	if at := got.Spec.Attachments[0]; at.Name != "notes.txt" || at.ContentType != "text/plain" {
		t.Errorf("attachment = %+v", at)
	}

	for _, tc := range []struct {
		name     string
		a        Attachments
		resource string
		user     authenticationv1.UserInfo
		ref      string
		denied   string
	}{
		{"someone else's upload", a, "submissions", inUserContext("u2"), ref, "one of your uploads"},
		{"a made-up ref", a, "submissions", inUserContext("u1"), "att-nope", "one of your uploads"},
		{"an upload for the other purpose", a, "securityreports", inUserContext("u1"), ref, "one of your uploads"},
		{"no bucket", Attachments{}, "submissions", inUserContext("u1"), ref, "aren't available"},
		{"staff in the platform context", a, "submissions", authenticationv1.UserInfo{Username: "staff"}, ref, "their own user context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.a.Handle(ctx, request(t, admissionv1.Create, tc.resource, tc.user, sub(tc.ref), nil))
			if resp.Allowed || !strings.Contains(resp.Result.Message, tc.denied) {
				t.Errorf("allowed %v, %q; want denial containing %q", resp.Allowed, resp.Result.Message, tc.denied)
			}
		})
	}

	op := authenticationv1.UserInfo{Username: identity.OperatorUser}
	if resp := a.Handle(ctx, request(t, admissionv1.Create, "securityreports", op, sub("att-moved"), nil)); !resp.Allowed {
		t.Errorf("operator's moved report denied: %v", resp.Result.Message)
	}
}
