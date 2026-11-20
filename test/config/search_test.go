package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	sigsyaml "sigs.k8s.io/yaml"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

type indexPolicy struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		TargetResource struct {
			Group, Version, Kind string
		} `json:"targetResource"`
		Visibility string `json:"visibility"`
		Conditions []struct {
			Name, Expression string
		} `json:"conditions"`
		Fields []struct {
			Path       string `json:"path"`
			Searchable bool   `json:"searchable"`
		} `json:"fields"`
	} `json:"spec"`
}

func loadIndexPolicies(t *testing.T) []indexPolicy {
	t.Helper()
	files, _ := filepath.Glob("../../config/milo/search/*.yaml")
	var out []indexPolicy
	for _, f := range files {
		if strings.HasSuffix(f, "kustomization.yaml") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var p indexPolicy
		if err := sigsyaml.Unmarshal(data, &p); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, p)
	}
	if len(out) != 3 {
		t.Fatalf("want policies for Post, Release and ChangelogEntry, got %d", len(out))
	}
	return out
}

// resolve walks a policy field path (.a.b, [*]) through Go types by JSON
// name, so a typo or renamed field fails here rather than indexing nothing.
func resolve(typ reflect.Type, path string) (reflect.Type, bool) {
	for _, seg := range strings.Split(strings.ReplaceAll(strings.TrimPrefix(path, "."), "[*]", ".[*]"), ".") {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if seg == "[*]" {
			if typ.Kind() != reflect.Slice {
				return nil, false
			}
			typ = typ.Elem()
			continue
		}
		f, ok := fieldByJSON(typ, seg)
		if !ok {
			return nil, false
		}
		typ = f
	}
	return typ, true
}

func fieldByJSON(typ reflect.Type, name string) (reflect.Type, bool) {
	if typ.Kind() != reflect.Struct {
		return nil, false
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if f.Anonymous && tag == "" {
			if t, ok := fieldByJSON(f.Type, name); ok {
				return t, true
			}
			continue
		}
		if tag == name {
			return f.Type, true
		}
	}
	return nil, false
}

func TestSearchPolicies(t *testing.T) {
	types := map[string]reflect.Type{
		"Post":           reflect.TypeOf(v1.Post{}),
		"Release":        reflect.TypeOf(v1.Release{}),
		"ChangelogEntry": reflect.TypeOf(v1.ChangelogEntry{}),
	}
	for _, p := range loadIndexPolicies(t) {
		tr := p.Spec.TargetResource
		typ, ok := types[tr.Kind]
		if tr.Group != group || tr.Version != "v1alpha1" || !ok {
			t.Errorf("%s targets %+v; only posts, releases and changelog entries are indexed", p.Metadata.Name, tr)
			continue
		}
		// Everything indexed here is public, so every context may find it.
		if p.Spec.Visibility != "AllContexts" {
			t.Errorf("%s has visibility %q; want AllContexts", p.Metadata.Name, p.Spec.Visibility)
		}
		for _, f := range p.Spec.Fields {
			if _, ok := resolve(typ, f.Path); !ok {
				t.Errorf("%s: %s isn't a field of %s", p.Metadata.Name, f.Path, tr.Kind)
			}
			for _, derived := range []string{".status.votes", ".status.delivery", ".status.posts", ".status.reactions"} {
				if strings.HasPrefix(f.Path, derived) {
					t.Errorf("%s indexes derived field %s", p.Metadata.Name, f.Path)
				}
			}
		}
	}
}

// postCondition compiles the posts policy's condition in the environment
// the search indexer uses: metadata, spec and status as dynamic variables
// (milo-os/search internal/cel NewEnv).
func postCondition(t *testing.T) cel.Program {
	t.Helper()
	var cond string
	for _, p := range loadIndexPolicies(t) {
		if p.Spec.TargetResource.Kind == "Post" {
			if len(p.Spec.Conditions) != 1 {
				t.Fatalf("want one condition, got %v", p.Spec.Conditions)
			}
			cond = p.Spec.Conditions[0].Expression
		}
	}
	env, err := cel.NewEnv(cel.Variable("metadata", cel.DynType), cel.Variable("spec", cel.DynType), cel.Variable("status", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(cond)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	return prg
}

// indexed evaluates the condition the way the indexer does
// (internal/policy/evaluation CachedPolicy.Evaluate): only the top-level
// keys the object has are bound, and an evaluation error counts as no match.
func indexed(prg cel.Program, obj map[string]any) (bool, error) {
	activation := map[string]any{}
	for _, k := range []string{"metadata", "spec", "status"} {
		if v, ok := obj[k]; ok {
			activation[k] = v
		}
	}
	out, _, err := prg.Eval(activation)
	if err != nil {
		return false, err
	}
	v, _ := out.Value().(bool)
	return v, nil
}

// postDefaulter applies the Post CRD's schema defaults to a raw object, as
// Milo does to every Post it stores and returns, and so to the audit event
// the indexer reads.
func postDefaulter(t *testing.T) func(map[string]any) {
	t.Helper()
	data, err := os.ReadFile("../../config/crd/bases/feedback.miloapis.com_posts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := sigsyaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	s, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	return func(obj map[string]any) { structuraldefaulting.Default(obj, s) }
}

// TestHiddenPostCondition checks which posts the indexer keeps, starting from
// raw objects as a client could create them, including one with no spec key.
// The indexer can't evaluate spec.hidden without a spec, so a post with none
// is indexed only because the CRD defaults spec to {}.
func TestHiddenPostCondition(t *testing.T) {
	prg := postCondition(t)
	defaults := postDefaulter(t)
	for _, c := range []struct {
		name string
		raw  string
		want bool
	}{
		{"no spec", `{"metadata":{"name":"a.b.1"},"status":{"title":"t"}}`, true},
		{"empty spec", `{"metadata":{"name":"a.b.2"},"spec":{}}`, true},
		{"visible", `{"metadata":{"name":"a.b.3"},"spec":{"hidden":false}}`, true},
		{"hidden", `{"metadata":{"name":"a.b.4"},"spec":{"hidden":true}}`, false},
	} {
		var obj map[string]any
		if err := json.Unmarshal([]byte(c.raw), &obj); err != nil {
			t.Fatal(err)
		}
		defaults(obj)
		got, err := indexed(prg, obj)
		if err != nil {
			t.Errorf("%s: %v (as served: %v)", c.name, err, obj)
			continue
		}
		if got != c.want {
			t.Errorf("%s: indexed=%v, want %v", c.name, got, c.want)
		}
	}

	// Typed clients always send a spec; check they agree.
	for _, hidden := range []bool{false, true} {
		raw, _ := json.Marshal(v1.Post{Spec: v1.PostSpec{Hidden: hidden}})
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		if got, err := indexed(prg, obj); err != nil || got == hidden {
			t.Errorf("typed post hidden=%v: indexed=%v, err=%v", hidden, got, err)
		}
	}
}
