package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// MetadataOnly are the kinds Milo's audit policy logs without request or
// response bodies (datum-cloud/infra, the Milo audit policy), because they
// carry private context or vulnerability details. Activity rules for them can
// only use the verb, object reference and audit annotations.
var MetadataOnly = []string{"submissions", "submissionreviews", "securityreports", "securityreporttriages", "feedbackdigests"}

// NoActivity are the kinds that must have no ActivityPolicy. The activity
// service files entries under the actor's context with no per-entry access
// check, so platform-timeline readers would learn a report exists and what
// staff did with it.
var NoActivity = []string{"securityreports", "securityreporttriages"}

// postRefAnnotation is the audit annotation config/milo/activity/post-ref.yaml
// adds to vote and subscription deletes.
const postRefAnnotation = "post-ref.feedback.miloapis.com/post"

// templateRE is the activity service's summary template pattern
// (internal/cel/policy.go in milo-os/activity).
var templateRE = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)

type link struct {
	text string
	ref  any
}

// auditEnv is the activity service's CEL environment for audit rules
// (NewAuditEnvironment in milo-os/activity internal/cel/environment.go): the
// standard library only, so no split or substring. link records its calls in
// links when links isn't nil.
func auditEnv(links *[]link) (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("audit", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("actor", cel.StringType),
		cel.Variable("actorRef", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("kind", cel.StringType),
		cel.Function("link",
			cel.Overload("link_string_dyn", []*cel.Type{cel.StringType, cel.DynType}, cel.StringType,
				cel.BinaryBinding(func(text, r ref.Val) ref.Val {
					if links != nil {
						native, err := r.ConvertToNative(reflect.TypeOf(map[string]any{}))
						if err != nil {
							native = r.Value()
						}
						*links = append(*links, link{fmt.Sprint(text.Value()), native})
					}
					return types.String(fmt.Sprint(text.Value()))
				}),
			),
		),
	)
}

// auditVars is the activity service's BuildAuditVars: absent nested maps
// become empty ones so has() on their fields is safe.
func auditVars(event map[string]any) map[string]any {
	a := map[string]any{}
	for k, v := range event {
		a[k] = v
	}
	for _, f := range []string{"objectRef", "user", "responseStatus", "responseObject", "requestObject"} {
		if _, ok := a[f]; !ok {
			a[f] = map[string]any{}
		}
	}
	user := obj(a).str("user", "username")
	return map[string]any{
		"audit":    a,
		"actor":    user,
		"actorRef": map[string]any{"type": "user", "name": user},
		"kind":     obj(a).str("objectRef", "resource"),
	}
}

// rendered is what the activity service makes of an audit event.
type rendered struct {
	rule, summary string
	links         []link
}

// render evaluates a policy's audit rules against event the way the activity
// processor does: top to bottom, first match wins, then the summary
// template. ok is false when no rule matches.
func render(t *testing.T, policy obj, event map[string]any) (r rendered, ok bool) {
	t.Helper()
	vars := auditVars(event)
	eval := func(env *cel.Env, expr string) ref.Val {
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatalf("%q: %v", expr, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		out, _, err := prg.Eval(vars)
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		return out
	}
	env, err := auditEnv(&r.links)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range policy.list("spec", "auditRules") {
		ro := obj(rule.(map[string]any))
		if eval(env, ro.str("match")).Value() != true {
			continue
		}
		r.rule = ro.str("name")
		r.summary = templateRE.ReplaceAllStringFunc(ro.str("summary"), func(m string) string {
			return fmt.Sprint(eval(env, templateRE.FindStringSubmatch(m)[1]).Value())
		})
		return r, true
	}
	return r, false
}

// policies returns the ActivityPolicies by the plural of the kind they cover.
func policies(t *testing.T) map[string]obj {
	t.Helper()
	plurals := map[string]string{}
	for plural, c := range loadCRDs(t) {
		plurals[c.kind] = plural
	}
	out := map[string]obj{}
	for _, p := range ofKind(load(t, "../../config/milo/activity"), "ActivityPolicy") {
		out[plurals[p.str("spec", "resource", "kind")]] = p
	}
	return out
}

// TestActivityPolicies compiles every rule's match expression and summary
// template in the activity service's CEL environment, checks each policy
// names a feedback CRD, and checks that rules don't read bodies that won't be
// there: none for metadata-only kinds, and none for deletes, whose audit
// event has a Status response and a DeleteOptions request.
func TestActivityPolicies(t *testing.T) {
	crds := loadCRDs(t)
	kinds := map[string]string{}
	for plural, c := range crds {
		kinds[c.kind] = plural
	}
	env, err := auditEnv(nil)
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
		for _, n := range NoActivity {
			if n == plural {
				t.Errorf("%s: %s must have no activity: platform-timeline readers would learn reports exist", name, plural)
			}
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
			readsBody := strings.Contains(match+summary, "requestObject") || strings.Contains(match+summary, "responseObject")
			if metadataOnly && readsBody {
				t.Errorf("%s: %s is logged at Metadata only, so the rule can't read request or response bodies", where, plural)
			}
			if strings.Contains(match, "'delete'") && readsBody {
				t.Errorf("%s: a delete's audit event has a Status response and DeleteOptions request, not the object", where)
			}
		}
	}
}

// TestDeleteActivityRenders runs vote and subscription delete audit events
// through the policies. The events in testdata/audit were written by
// envtest's kube-apiserver (TestDeleteActivityInAnAPIServer), with the user
// changed to a person in their own context in Milo: the request is
// DeleteOptions, the response a Status, and neither has the object's spec.
// The post comes from post-ref.yaml's audit annotation.
func TestDeleteActivityRenders(t *testing.T) {
	ps := policies(t)
	for _, tc := range []struct {
		file, plural, rule, summary, post string
		noAnnotation                      bool
	}{
		{file: "vote-delete.json", plural: "votes", rule: "unvote",
			summary: "alice@example.com removed their vote on datum-cloud.enhancements.912", post: "datum-cloud.enhancements.912"},
		{file: "subscription-delete.json", plural: "subscriptions", rule: "unfollow",
			summary: "alice@example.com stopped following datum-cloud.enhancements.912", post: "datum-cloud.enhancements.912"},
		// Without the annotation (post-ref.yaml not installed yet, or its
		// expression failed) the entry still appears, without the post.
		{file: "vote-delete.json", plural: "votes", rule: "unvote-post-unknown", summary: "alice@example.com removed a vote", noAnnotation: true},
		{file: "subscription-delete.json", plural: "subscriptions", rule: "unfollow-post-unknown", summary: "alice@example.com stopped following a post", noAnnotation: true},
	} {
		t.Run(fmt.Sprintf("%s annotation=%v", tc.file, !tc.noAnnotation), func(t *testing.T) {
			event := readEvent(t, filepath.Join("testdata", "audit", tc.file))
			if tc.noAnnotation {
				delete(event["annotations"].(map[string]any), postRefAnnotation)
			}
			checkDeleteEventShape(t, event)
			got, ok := render(t, ps[tc.plural], event)
			if !ok {
				t.Fatal("no rule matched")
			}
			if got.rule != tc.rule || got.summary != tc.summary {
				t.Errorf("got rule %s %q, want %s %q", got.rule, got.summary, tc.rule, tc.summary)
			}
			if tc.post != "" {
				var ref map[string]any
				if len(got.links) == 1 {
					ref, _ = got.links[0].ref.(map[string]any)
				}
				if obj(ref).str("name") != tc.post || obj(ref).str("resource") != "posts" {
					t.Errorf("links %v, want one to post %s", got.links, tc.post)
				}
			}
		})
	}
}

// readEvent reads an audit.k8s.io/v1 Event as the activity processor sees
// it: JSON decoded into a map.
func readEvent(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return event
}

// checkDeleteEventShape checks event is a custom resource delete as the API
// server logs it, so the rules are tested against what really arrives.
func checkDeleteEventShape(t *testing.T, event map[string]any) {
	t.Helper()
	e := obj(event)
	if e.str("verb") != "delete" || e.str("responseObject", "kind") != "Status" || e.str("responseObject", "status") != "Success" {
		t.Errorf("not a successful delete with a Status response: verb %q, response %v", e.str("verb"), e.get("responseObject"))
	}
	if e.get("responseObject", "spec") != nil || e.get("requestObject", "spec") != nil {
		t.Errorf("a delete's audit event has no spec")
	}
	if k := e.str("requestObject", "kind"); k != "" && k != "DeleteOptions" {
		t.Errorf("request is %s, want DeleteOptions or none", k)
	}
}
