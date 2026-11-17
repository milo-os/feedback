// Package config checks the manifests under config/ against the CRDs and
// each other.
package config

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

const group = v1.GroupName

type obj map[string]any

func (o obj) get(path ...string) any {
	var cur any = map[string]any(o)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func (o obj) str(path ...string) string { s, _ := o.get(path...).(string); return s }
func (o obj) list(path ...string) []any { l, _ := o.get(path...).([]any); return l }

// load reads every object in the YAML files under dir.
func load(t *testing.T, dir string) []obj {
	t.Helper()
	var out []obj
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
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

type crd struct {
	kind, plural string
	contexts     []string
	ownerField   string
	hiddenField  string
	status       bool
}

func loadCRDs(t *testing.T) map[string]crd {
	t.Helper()
	out := map[string]crd{}
	for _, c := range ofKind(load(t, "../../config/crd/bases"), "CustomResourceDefinition") {
		ann := func(k string) string { return c.str("metadata", "annotations", k) }
		status := false
		for _, v := range c.list("spec", "versions") {
			if obj(v.(map[string]any)).get("subresources", "status") != nil {
				status = true
			}
		}
		out[c.str("spec", "names", "plural")] = crd{
			kind:        c.str("spec", "names", "kind"),
			plural:      c.str("spec", "names", "plural"),
			contexts:    strings.Split(ann(v1.ParentContextsAnnotation), ";"),
			ownerField:  ann(v1.OwnerFieldAnnotation),
			hiddenField: ann(v1.HiddenFieldAnnotation),
			status:      status,
		}
	}
	if len(out) == 0 {
		t.Fatal("no CRDs found")
	}
	return out
}

func TestMiloConfig(t *testing.T) {
	crds := loadCRDs(t)
	objs := load(t, "../../config/milo")

	// perms["posts"] = {"get", ...}; perms["posts/status"] = {...}
	perms := map[string][]string{}
	kinds := map[string]string{} // kind -> plural
	for _, pr := range ofKind(objs, "ProtectedResource") {
		plural := pr.str("spec", "plural")
		c, ok := crds[plural]
		if !ok {
			t.Errorf("ProtectedResource for %s has no CRD", plural)
			continue
		}
		if pr.str("spec", "kind") != c.kind {
			t.Errorf("ProtectedResource %s: kind %s, CRD says %s", plural, pr.str("spec", "kind"), c.kind)
		}
		kinds[c.kind] = plural
		for _, v := range pr.list("spec", "permissions") {
			perms[plural] = append(perms[plural], v.(string))
		}
		subs := map[string]bool{}
		for _, s := range pr.list("spec", "subresources") {
			so := obj(s.(map[string]any))
			subs[so.str("name")] = true
			for _, v := range so.list("permissions") {
				perms[plural+"/"+so.str("name")] = append(perms[plural+"/"+so.str("name")], v.(string))
			}
		}
		if subs["status"] != c.status {
			t.Errorf("%s: status subresource declared %v, CRD has it %v", plural, subs["status"], c.status)
		}
		if subs["hidden"] != (c.hiddenField != "") {
			t.Errorf("%s: hidden subresource declared %v, CRD hidden field %q", plural, subs["hidden"], c.hiddenField)
		}
		userParent := slices.ContainsFunc(pr.list("spec", "parentResources"), func(p any) bool {
			return obj(p.(map[string]any)).str("kind") == "User"
		})
		if userParent != slices.Contains(c.contexts, "User") {
			t.Errorf("%s: User parent %v, but CRD contexts are %v", plural, userParent, c.contexts)
		}
	}
	for plural := range crds {
		if _, ok := perms[plural]; !ok {
			t.Errorf("no ProtectedResource for %s", plural)
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
			// Security reports and their triage stay with the Security Team,
			// and reporters' own reports with the user role.
			secret := strings.HasPrefix(rest, "securityreports") || strings.HasPrefix(rest, "securityreporttriages")
			if secret && name != group+"-security-team" && !(name == group+"-user" && strings.HasPrefix(rest, "securityreports.")) {
				t.Errorf("role %s grants %s", name, s)
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
			if !slices.Contains(ro.list("apiGroups"), any(group)) {
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
	for _, u := range []string{identity.OperatorUser, identity.SyncUser, identity.UploadsUser} {
		if !subjects[u] {
			t.Errorf("no ClusterRoleBinding for %s", u)
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
}

// TestOwnerWebhookCoversOwnedKinds checks the owner webhook runs for exactly
// the CRDs Milo limits by owner, so no user-owned kind can be created with
// someone else's name in it.
func TestOwnerWebhookCoversOwnedKinds(t *testing.T) {
	crds := loadCRDs(t)
	var owned []string
	for plural, c := range crds {
		if c.ownerField != "" {
			if c.ownerField != "spec.userRef.name" {
				t.Errorf("%s: owner field %s; the webhook sets spec.userRef.name", plural, c.ownerField)
			}
			owned = append(owned, plural)
		}
	}
	var covered []string
	for _, m := range ofKind(load(t, "../../config/milo/webhook"), "MutatingWebhookConfiguration") {
		for _, w := range m.list("webhooks") {
			wo := obj(w.(map[string]any))
			if wo.str("name") != "owner.feedback.miloapis.com" {
				continue
			}
			for _, r := range wo.list("rules") {
				for _, res := range obj(r.(map[string]any)).list("resources") {
					covered = append(covered, res.(string))
				}
			}
		}
	}
	slices.Sort(owned)
	slices.Sort(covered)
	if !slices.Equal(owned, covered) {
		t.Errorf("owner webhook covers %v, owned kinds are %v", covered, owned)
	}
}
