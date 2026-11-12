package feedbacksource

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
)

func TestPlatformOnly(t *testing.T) {
	inUser := request.WithUser(context.Background(), &user.DefaultInfo{Name: "u", Extra: map[string][]string{
		"iam.miloapis.com/parent-type": {"User"}, "iam.miloapis.com/parent-name": {"u"}}})
	staff := request.WithUser(context.Background(), &user.DefaultInfo{Name: "staff"})
	if err := platformOnly(staff); err != nil {
		t.Errorf("platform: %v", err)
	}
	// The guard runs before the store, so a nil store is never reached.
	r := &REST{Status: &StatusREST{}}
	for name, err := range map[string]error{
		"get":    func() error { _, err := r.Get(inUser, "x", &metav1.GetOptions{}); return err }(),
		"list":   func() error { _, err := r.List(inUser, &metainternalversion.ListOptions{}); return err }(),
		"watch":  func() error { _, err := r.Watch(inUser, &metainternalversion.ListOptions{}); return err }(),
		"create": func() error { _, err := r.Create(inUser, nil, nil, nil); return err }(),
		"update": func() error { _, _, err := r.Update(inUser, "x", nil, nil, nil, false, nil); return err }(),
		"delete": func() error { _, _, err := r.Delete(inUser, "x", nil, nil); return err }(),
		"status": func() error { _, _, err := r.Status.Update(inUser, "x", nil, nil, nil, false, nil); return err }(),
	} {
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s from the user context: %v", name, err)
		}
	}
}
