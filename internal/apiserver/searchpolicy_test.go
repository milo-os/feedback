package apiserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"

	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

type indexPolicy struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		TargetResource struct {
			Group, Version, Kind string
		} `json:"targetResource"`
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
		if err := yaml.Unmarshal(data, &p); err != nil {
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
		"Post":           reflect.TypeOf(feedbackv1alpha1.Post{}),
		"Release":        reflect.TypeOf(feedbackv1alpha1.Release{}),
		"ChangelogEntry": reflect.TypeOf(feedbackv1alpha1.ChangelogEntry{}),
	}
	for _, p := range loadIndexPolicies(t) {
		tr := p.Spec.TargetResource
		typ, ok := types[tr.Kind]
		if tr.Group != group || tr.Version != "v1alpha1" || !ok {
			t.Errorf("%s targets %+v; only posts, releases and changelog entries are indexed", p.Metadata.Name, tr)
			continue
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

// TestHiddenPostCondition evaluates the posts policy's condition the way the
// indexer does: metadata, spec and status as dynamic variables.
func TestHiddenPostCondition(t *testing.T) {
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
	for _, c := range []struct {
		hidden bool
		want   bool
	}{{false, true}, {true, false}} {
		p := feedbackv1alpha1.Post{Spec: feedbackv1alpha1.PostSpec{Hidden: c.hidden}}
		raw, _ := json.Marshal(p)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		out, _, err := prg.Eval(map[string]any{"metadata": obj["metadata"], "spec": obj["spec"], "status": obj["status"]})
		if err != nil {
			t.Fatalf("hidden=%v: %v", c.hidden, err)
		}
		if out.Value() != c.want {
			t.Errorf("hidden=%v: indexed=%v, want %v", c.hidden, out.Value(), c.want)
		}
	}
}
