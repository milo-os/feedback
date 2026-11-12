package apiserver

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"go.miloapis.com/feedback/internal/identity"
	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

const group = feedbackv1alpha1.GroupName

type obj map[string]any

func (o obj) str(path ...string) string {
	var cur any = map[string]any(o)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

func (o obj) list(path ...string) []any {
	var cur any = map[string]any(o)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	l, _ := cur.([]any)
	return l
}

// loadMilo reads every object under config/milo.
func loadMilo(t *testing.T) []obj {
	t.Helper()
	var out []obj
	err := filepath.WalkDir("../../config/milo", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") || d.Name() == "kustomization.yaml" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, doc := range bytes.Split(data, []byte("\n---\n")) {
			var o obj
			if err := yaml.Unmarshal(doc, &o); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if len(o) > 0 {
				out = append(out, o)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ofKind(objs []obj, kind string) []obj {
	var out []obj
	for _, o := range objs {
		if o.str("kind") == kind {
			out = append(out, o)
		}
	}
	return out
}

// TestMiloConfig checks the IAM, RBAC and discovery config agree with the
// API and with each other.
func TestMiloConfig(t *testing.T) {
	objs := loadMilo(t)

	// perms["posts"] = {"get", ...}; perms["posts/status"] = {...}
	perms := map[string][]string{}
	kinds := map[string]string{} // kind -> plural
	for _, pr := range ofKind(objs, "ProtectedResource") {
		plural := pr.str("spec", "plural")
		kinds[pr.str("spec", "kind")] = plural
		for _, v := range pr.list("spec", "permissions") {
			perms[plural] = append(perms[plural], v.(string))
		}
		for _, s := range pr.list("spec", "subresources") {
			so := obj(s.(map[string]any))
			for _, v := range so.list("permissions") {
				perms[plural+"/"+so.str("name")] = append(perms[plural+"/"+so.str("name")], v.(string))
			}
		}
	}

	notResources := []string{"SubmissionReview", "SubmissionToSecurityReport"}
	for _, o := range feedbackv1alpha1.KnownTypes() {
		kind := reflect.TypeOf(o).Elem().Name()
		if strings.HasSuffix(kind, "List") || slices.Contains(notResources, kind) {
			continue
		}
		if kinds[kind] == "" {
			t.Errorf("no ProtectedResource for %s", kind)
		}
	}

	allows := func(resource, verb string) bool { return slices.Contains(perms[resource], verb) }

	roles := map[string]bool{}
	for _, r := range ofKind(objs, "Role") {
		roles[r.str("metadata", "name")] = true
	}
	for _, r := range ofKind(objs, "Role") {
		name := r.str("metadata", "name")
		for _, p := range r.list("spec", "includedPermissions") {
			s := p.(string)
			rest, ok := strings.CutPrefix(s, group+"/")
			i := strings.LastIndex(rest, ".")
			if !ok || i < 0 || !allows(rest[:i], rest[i+1:]) {
				t.Errorf("role %s: %s isn't a declared permission", name, s)
			}
		}
		for _, in := range r.list("spec", "inheritedRoles") {
			if n := obj(in.(map[string]any)).str("name"); !roles[n] {
				t.Errorf("role %s inherits unknown role %s", name, n)
			}
		}
	}

	for _, cr := range ofKind(objs, "ClusterRole") {
		for _, rule := range cr.list("rules") {
			ro := obj(rule.(map[string]any))
			if !slices.ContainsFunc(ro.list("apiGroups"), func(g any) bool { return g == group }) {
				continue
			}
			for _, res := range ro.list("resources") {
				for _, v := range ro.list("verbs") {
					if !allows(res.(string), v.(string)) {
						t.Errorf("ClusterRole %s grants %s %s, which isn't declared", cr.str("metadata", "name"), v, res)
					}
				}
			}
		}
	}

	subjects := map[string]bool{}
	for _, b := range ofKind(objs, "ClusterRoleBinding") {
		for _, s := range b.list("subjects") {
			subjects[obj(s.(map[string]any)).str("name")] = true
		}
	}
	for _, u := range []string{identity.SyncUser, identity.ControllerUser, identity.APIServerUser} {
		if !subjects[u] {
			t.Errorf("no ClusterRoleBinding for %s", u)
		}
	}
	if !slices.Equal(identity.Default.Sync, []string{identity.SyncUser}) || !slices.Equal(identity.Default.Controller, []string{identity.ControllerUser}) {
		t.Errorf("identity defaults don't match the RBAC subjects: %+v", identity.Default)
	}

	discovered := map[string]bool{}
	for _, d := range ofKind(objs, "DiscoveryContextPolicy") {
		for _, rule := range d.list("spec", "rules") {
			for _, r := range obj(rule.(map[string]any)).list("resources") {
				discovered[r.(string)] = true
			}
		}
	}
	for _, plural := range kinds {
		if !discovered[plural] {
			t.Errorf("%s isn't in the DiscoveryContextPolicy", plural)
		}
	}

	for _, b := range ofKind(objs, "PolicyBinding") {
		if n := b.str("spec", "roleRef", "name"); !roles[n] {
			t.Errorf("PolicyBinding %s binds unknown role %s", b.str("metadata", "name"), n)
		}
		if k := b.str("spec", "resourceSelector", "resourceKind", "kind"); kinds[k] == "" {
			t.Errorf("PolicyBinding %s selects unknown kind %s", b.str("metadata", "name"), k)
		}
	}

	// Security reports stay out of every role but the Security Team's and
	// the user's own.
	for _, r := range ofKind(objs, "Role") {
		name := r.str("metadata", "name")
		if name == group+"-security-team" || name == group+"-user" {
			continue
		}
		for _, p := range r.list("spec", "includedPermissions") {
			if strings.HasPrefix(p.(string), group+"/securityreports") {
				t.Errorf("role %s grants %s", name, p)
			}
		}
	}
}
