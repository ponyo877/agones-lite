// Package agones is the glue for using the upstream Agones API types
// without a Kubernetes cluster.
//
// The types come from agones.dev/agones/pkg/apis. They expect two things a
// cluster would normally provide: initialised feature gates and a set of
// cloud-product hooks. This package supplies both.
package agones

import (
	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"agones.dev/agones/pkg/util/runtime"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Setup fixes the feature gates to the Agones v1.62.0 defaults.
//
// Upstream treats every gate as off until ParseFeatures runs, so this must be
// called once before any Agones type is defaulted or validated.
func Setup() error {
	return runtime.ParseFeatures("")
}

// NoHooks is an agonesv1.APIHooks that adds nothing.
// Upstream uses the hooks for cloud-specific rules such as GKE Autopilot's.
type NoHooks struct{}

var _ agonesv1.APIHooks = NoHooks{}

// ValidateGameServerSpec accepts every spec.
func (NoHooks) ValidateGameServerSpec(*agonesv1.GameServerSpec, *field.Path) field.ErrorList {
	return nil
}

// ValidateScheduling accepts every strategy.
func (NoHooks) ValidateScheduling(apis.SchedulingStrategy, *field.Path) field.ErrorList {
	return nil
}

// MutateGameServerPod leaves the Pod alone.
func (NoHooks) MutateGameServerPod(*agonesv1.GameServerSpec, *corev1.Pod) error {
	return nil
}

// SetEviction leaves the Pod alone.
func (NoHooks) SetEviction(*agonesv1.Eviction, *corev1.Pod) error {
	return nil
}
