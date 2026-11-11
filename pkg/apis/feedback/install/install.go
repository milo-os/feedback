// Package install registers the feedback API group with a scheme.
package install

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// Install registers the feedback API group and sets its version priority.
func Install(scheme *runtime.Scheme) {
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(scheme.SetVersionPriority(v1alpha1.SchemeGroupVersion))
}
