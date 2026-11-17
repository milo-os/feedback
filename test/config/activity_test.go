package config

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
)

// MetadataOnly are the kinds Milo's audit policy logs without request or
// response bodies (datum-cloud/infra, the Milo audit policy), because they
// carry private context or vulnerability details. Activity rules for them can
// only use the verb and object reference.
var MetadataOnly = []string{"submissions", "submissionreviews", "securityreports", "securityreporttriages", "feedbackdigests"}

var templateRE = regexp.MustCompile(`{{\s*(.*?)\s*}}`)

// TestActivityPolicies compiles every rule's match expression and summary
// template, checks each policy names a feedback CRD, and checks that rules for
// metadata-only kinds don't read bodies that won't be there.
func TestActivityPolicies(t *testing.T) {
	crds := loadCRDs(t)
	kinds := map[string]string{}
	for plural, c := range crds {
		kinds[c.kind] = plural
	}
	env, err := cel.NewEnv(
		cel.Variable("audit", cel.DynType),
		cel.Variable("actor", cel.StringType),
		cel.Function("link", cel.Overload("link_dyn_dyn", []*cel.Type{cel.DynType, cel.DynType}, cel.StringType)),
	)
	if err != nil {
		t.Fatal(err)
	}
	compile := func(where, expr string) {
		if _, iss := env.Compile(expr); iss.Err() != nil {
			t.Errorf("%s: %q: %v", where, expr, iss.Err())
		}
	}

	policies := ofKind(load(t, "../../config/milo/activity"), "ActivityPolicy")
	if len(policies) == 0 {
		t.Fatal("no ActivityPolicies")
	}
	for _, p := range policies {
		name := p.str("metadata", "name")
		if p.str("spec", "resource", "apiGroup") != group {
			t.Errorf("%s: not for %s", name, group)
		}
		plural, ok := kinds[p.str("spec", "resource", "kind")]
		if !ok {
			t.Errorf("%s: no CRD for kind %s", name, p.str("spec", "resource", "kind"))
			continue
		}
		metadataOnly := false
		for _, m := range MetadataOnly {
			metadataOnly = metadataOnly || m == plural
		}
		for _, r := range p.list("spec", "auditRules") {
			ro := obj(r.(map[string]any))
			where := name + "/" + ro.str("name")
			match, summary := ro.str("match"), ro.str("summary")
			compile(where+" match", match)
			for _, sub := range templateRE.FindAllStringSubmatch(summary, -1) {
				compile(where+" summary", sub[1])
			}
			if !strings.Contains(match, "!audit.user.username.startsWith('system:')") {
				t.Errorf("%s: doesn't leave out the service's own writes", where)
			}
			if metadataOnly && (strings.Contains(match+summary, "requestObject") || strings.Contains(match+summary, "responseObject")) {
				t.Errorf("%s: %s is logged at Metadata only, so the rule can't read request or response bodies", where, plural)
			}
		}
	}
}
