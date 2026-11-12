package apiserver

import (
	"os"
	"testing"

	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit/policy"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"sigs.k8s.io/yaml"
)

// TestAuditPolicy evaluates the shipped policy: writes are recorded with
// their bodies, except private kinds, which are recorded at Metadata; reads
// are left to Milo.
func TestAuditPolicy(t *testing.T) {
	raw, err := os.ReadFile("../../config/components/audit-logging/audit-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatal(err)
	}
	p, err := policy.LoadPolicyFromBytes([]byte(cm.Data["audit-policy.yaml"]))
	if err != nil {
		t.Fatal(err)
	}
	eval := policy.NewPolicyRuleEvaluator(p)

	u := &user.DefaultInfo{Name: "someone"}
	res := func(verb, resource, sub string) authorizer.Attributes {
		return authorizer.AttributesRecord{User: u, Verb: verb, APIGroup: group, APIVersion: "v1alpha1",
			Resource: resource, Subresource: sub, Name: "x", ResourceRequest: true}
	}
	for _, c := range []struct {
		attrs authorizer.Attributes
		want  auditinternal.Level
	}{
		{res("create", "posts", ""), auditinternal.LevelRequestResponse},
		{res("patch", "posts", "status"), auditinternal.LevelRequestResponse},
		{res("delete", "posts", ""), auditinternal.LevelRequestResponse},
		{res("update", "releases", "status"), auditinternal.LevelRequestResponse},
		{res("create", "changelogentries", ""), auditinternal.LevelRequestResponse},
		{res("create", "votes", ""), auditinternal.LevelRequestResponse},
		{res("delete", "subscriptions", ""), auditinternal.LevelRequestResponse},
		{res("update", "feedbacksources", ""), auditinternal.LevelRequestResponse},
		{res("create", "submissions", ""), auditinternal.LevelMetadata},
		{res("update", "submissions", "status"), auditinternal.LevelMetadata},
		{res("create", "submissions", "review"), auditinternal.LevelMetadata},
		{res("create", "submissions", "to-security-report"), auditinternal.LevelMetadata},
		{res("create", "securityreports", ""), auditinternal.LevelMetadata},
		{res("patch", "securityreports", "status"), auditinternal.LevelMetadata},
		{res("create", "attachmentuploads", ""), auditinternal.LevelMetadata},
		{res("create", "postqueries", ""), auditinternal.LevelMetadata},
		{res("get", "posts", ""), auditinternal.LevelNone},
		{res("list", "submissions", ""), auditinternal.LevelNone},
		{res("get", "securityreports", ""), auditinternal.LevelNone},
		{res("get", "submissions", "attachment"), auditinternal.LevelNone},
		{authorizer.AttributesRecord{User: u, Verb: "get", Path: "/readyz"}, auditinternal.LevelNone},
	} {
		got := eval.EvaluatePolicyRule(c.attrs).Level
		if got != c.want {
			t.Errorf("%s %s/%s: level %s, want %s", c.attrs.GetVerb(), c.attrs.GetResource(), c.attrs.GetSubresource(), got, c.want)
		}
	}
}
