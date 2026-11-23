package apiserver

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/ponyo877/agones-lite/pkg/agones"
	"github.com/ponyo877/agones-lite/pkg/podspec"
)

// Each admit function does the work of Agones' admission webhooks for one
// kind: defaults, upstream's validation, then our check of the Pod definition.

func admitGameServer(gs *agonesv1.GameServer) (warnings []string, err error) {
	gs.APIVersion, gs.Kind = "agones.dev/v1", "GameServer"
	gs.ApplyDefaults()
	errs := gs.Validate(agones.NoHooks{})
	warnings, unsupported := podspec.Check(&gs.Spec, field.NewPath("spec"))
	return warnings, append(errs, unsupported...).ToAggregate()
}

func admitFleet(fleet *agonesv1.Fleet) (warnings []string, err error) {
	fleet.APIVersion, fleet.Kind = "agones.dev/v1", "Fleet"
	fleet.ApplyDefaults()
	errs := fleet.Validate(agones.NoHooks{})
	warnings, unsupported := podspec.Check(&fleet.Spec.Template.Spec, field.NewPath("spec", "template", "spec"))
	return warnings, append(errs, unsupported...).ToAggregate()
}

func admitFleetAutoscaler(fas *autoscalingv1.FleetAutoscaler) (warnings []string, err error) {
	fas.APIVersion, fas.Kind = "autoscaling.agones.dev/v1", "FleetAutoscaler"
	fas.ApplyDefaults()
	return nil, fas.Validate().ToAggregate()
}

// carryFleetAutoscaler does for a FleetAutoscaler what carryFleet does for a Fleet.
func carryFleetAutoscaler(stored, incoming *autoscalingv1.FleetAutoscaler) {
	if !apiequality.Semantic.DeepEqual(stored.Spec, incoming.Spec) {
		incoming.Generation++
	}
	incoming.Status = stored.Status
}

// carryFleet keeps the status, which belongs to the controller, and counts a
// new generation when the spec changed.
func carryFleet(stored, incoming *agonesv1.Fleet) {
	if !apiequality.Semantic.DeepEqual(stored.Spec, incoming.Spec) {
		incoming.Generation++
	}
	incoming.Status = stored.Status
}
