package v1alpha1_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestMovedReportName(t *testing.T) {
	const uid = types.UID("6f1c2a7e-3b4d-4c5e-9f60-718293a4b5c6")
	if got := v1.MovedReportName("sub-1", uid); got != "sub-1-"+string(uid) {
		t.Errorf("got %q", got)
	}
	// Too long: shortened, without leaving a dot or dash before the UID.
	long := strings.Repeat("a", 215) + ".b" + strings.Repeat("c", 30)
	got := v1.MovedReportName(long, uid)
	if len(got) > 253 || !strings.HasSuffix(got, "a-"+string(uid)) {
		t.Errorf("got %q (%d)", got, len(got))
	}
	for _, name := range []string{"sub-1", long, strings.Repeat("x.", 120) + "y"} {
		n := v1.MovedReportName(name, uid)
		if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
			t.Errorf("%q: %v", n, errs)
		}
		if n != v1.MovedReportName(name, uid) {
			t.Errorf("%q: not deterministic", name)
		}
	}
}
