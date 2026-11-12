package main

import (
	"reflect"
	"strings"
	"testing"

	apiopenapi "k8s.io/apiserver/pkg/endpoints/openapi"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	openapiutil "k8s.io/kube-openapi/pkg/util"
	"k8s.io/kube-openapi/pkg/validation/spec"

	feedbackapiserver "go.miloapis.com/feedback/internal/apiserver"
	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
	"go.miloapis.com/feedback/pkg/generated/openapi"
)

const typesPkg = "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"

func definitionNamer() func(string) (string, spec.Extensions) {
	namer := apiopenapi.NewDefinitionNamer(feedbackapiserver.Scheme)
	return func(name string) (string, spec.Extensions) {
		if strings.Contains(name, "/") {
			name = openapiutil.ToRESTFriendlyName(name)
		}
		return namer.GetDefinitionName(name)
	}
}

// TestOpenAPIGVKExtensions checks every kind has the
// x-kubernetes-group-version-kind extension. Without it server-side apply
// fails with "no corresponding type".
func TestOpenAPIGVKExtensions(t *testing.T) {
	getName := definitionNamer()
	defs := openapi.GetOpenAPIDefinitions(func(string) spec.Ref { return spec.Ref{} })

	for _, obj := range append(feedbackv1alpha1.KnownTypes(), feedbackv1alpha1.ParameterTypes()...) {
		kind := reflect.TypeOf(obj).Elem().Name()
		t.Run(kind, func(t *testing.T) {
			key := typesPkg + "." + kind
			if _, ok := defs[key]; !ok {
				// Types with their own OpenAPIModelName are keyed by it.
				key = openapiutil.ToRESTFriendlyName(key)
			}
			if _, ok := defs[key]; !ok {
				t.Fatalf("%s has no OpenAPI definition; run task generate", key)
			}
			_, ext := getName(key)
			gvks, ok := ext["x-kubernetes-group-version-kind"].([]any)
			if !ok {
				t.Fatalf("no GVK extension for %s", key)
			}
			for _, g := range gvks {
				m, _ := g.(map[string]any)
				if m["group"] == feedbackv1alpha1.GroupName && m["version"] == "v1alpha1" && m["kind"] == kind {
					return
				}
			}
			t.Fatalf("GVK %s/v1alpha1 %s missing from %v", feedbackv1alpha1.GroupName, kind, gvks)
		})
	}
}

// TestOpenAPIRefsResolve checks every $ref points at a definition that
// exists under its REST-friendly name, which is what the server publishes.
func TestOpenAPIRefsResolve(t *testing.T) {
	getName := definitionNamer()
	defs := openapi.GetOpenAPIDefinitions(func(name string) spec.Ref {
		n, _ := getName(name)
		return spec.MustCreateRef("#/components/schemas/" + openapicommon.EscapeJsonPointer(n))
	})
	published := map[string]bool{}
	for k := range defs {
		n, _ := getName(k)
		published[n] = true
	}

	for key, def := range defs {
		if !strings.HasPrefix(key, typesPkg) {
			continue
		}
		for _, dep := range def.Dependencies {
			n, _ := getName(dep)
			if !published[n] {
				t.Errorf("%s depends on %s (%s), which is not published", key, dep, n)
			}
		}
	}
}

// TestOpenAPIEnums checks +enum markers reach the schema, so clients get the
// allowed values.
func TestOpenAPIEnums(t *testing.T) {
	defs := openapi.GetOpenAPIDefinitions(func(string) spec.Ref { return spec.Ref{} })
	cases := []struct {
		typ, field string
		want       []string
	}{
		{"PostStatus", "state", []string{"backlog", "closed", "in-progress", "none", "on-deck", "planning", "shipped"}},
		{"SubmissionStatus", "phase", []string{"Approved", "Merged", "PendingReview", "Rejected"}},
		{"PostQuerySpec", "sort", []string{"newest", "recentlyChanged", "relevance", "top", "trending"}},
	}
	for _, tc := range cases {
		prop, ok := defs[typesPkg+"."+tc.typ].Schema.Properties[tc.field]
		if !ok {
			t.Fatalf("%s.%s not in schema", tc.typ, tc.field)
		}
		var got []string
		for _, v := range prop.Enum {
			got = append(got, v.(string))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s.%s enum = %v, want %v", tc.typ, tc.field, got, tc.want)
		}
	}
}
