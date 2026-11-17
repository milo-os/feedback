package v1alpha1_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"

	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	install.Install(s)
	return s
}

// TestSamplesDecodeStrictly decodes every sample with unknown and duplicate
// fields rejected, so the samples can't drift from the types. Every top-level
// kind except list types needs a sample.
func TestSamplesDecodeStrictly(t *testing.T) {
	scheme := newScheme(t)
	dec := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme,
		json.SerializerOptions{Yaml: true, Strict: true})

	files, err := filepath.Glob("../../../../config/samples/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no samples found: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			_, gvk, err := dec.Decode(data, nil, nil)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			seen[gvk.Kind] = true
		})
	}
	for _, obj := range feedbackv1alpha1.KnownTypes() {
		kind := reflect.TypeOf(obj).Elem().Name()
		if strings.HasSuffix(kind, "List") {
			continue
		}
		if !seen[kind] {
			t.Errorf("no sample in config/samples for %s", kind)
		}
	}
}
