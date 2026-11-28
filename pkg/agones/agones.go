// Package agones is the glue for using the upstream Agones API types
// without a Kubernetes cluster.
//
// The types come from agones.dev/agones/pkg/apis. They expect two things a
// cluster would normally provide: initialised feature gates and a set of
// cloud-product hooks. This package supplies both.
package agones

import (
	"agones.dev/agones/pkg"
	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"agones.dev/agones/pkg/util/runtime"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Version is the release of Agones that scrim is compatible with: the
// version of the module in go.mod.
const Version = "1.62.0"

// Setup makes the Agones code behave as in that release.
//
// Upstream treats every feature gate as off until ParseFeatures runs, so this
// must be called once before any Agones type is defaulted or validated.
// Defaulting also writes pkg.Version into the agones.dev/sdk-version
// annotation of GameServers and Fleets. Upstream's builds set it with
// -ldflags; left alone, it says "dev".
func Setup() error {
	pkg.Version = Version
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
