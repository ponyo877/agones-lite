package apiserver

import (
	"reflect"
	"slices"
	"strings"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/ponyo877/scrim/pkg/agones"
	"github.com/ponyo877/scrim/pkg/podspec"
)

// Each admit function does for one kind what Kubernetes does between a
// request and the store: the status a client sent is dropped, because status
// belongs to the controllers; then come upstream's defaults, the limits of
// the CRD schema (schema.go), upstream's validation, and our check of the
// Pod definition.

func admitGameServer(gs *agonesv1.GameServer) (warnings []string, errs field.ErrorList) {
	gs.APIVersion, gs.Kind = "agones.dev/v1", "GameServer"
	gs.Status = agonesv1.GameServerStatus{}
	spec := field.NewPath("spec")
	warnings = labelsOnly(&gs.Spec.Template.ObjectMeta, spec.Child("template", "metadata"))
	gs.ApplyDefaults()
	// The defaults add upstream's finalizer every time they run, and they
	// run again when what was admitted is applied again.
	gs.Finalizers = withoutRepeats(gs.Finalizers)
	errs = slices.Concat(
		notForDevelopment(gs.Annotations, field.NewPath("metadata", "annotations")),
		gameServerSpecSchema(&gs.Spec, spec),
		gs.Validate(agones.NoHooks{}),
	)
	ignored, unsupported := podspec.Check(&gs.Spec, spec)
	return append(warnings, ignored...), append(errs, unsupported...)
}

func admitFleet(fleet *agonesv1.Fleet) (warnings []string, errs field.ErrorList) {
	fleet.APIVersion, fleet.Kind = "agones.dev/v1", "Fleet"
	fleet.Status = agonesv1.FleetStatus{}
	template := field.NewPath("spec", "template")
	warnings = labelsOnly(&fleet.Spec.Template.ObjectMeta, template.Child("metadata"))
	pod := &fleet.Spec.Template.Spec.Template.ObjectMeta
	warnings = append(warnings, labelsOnly(pod, template.Child("spec", "template", "metadata"))...)
	fleet.ApplyDefaults()
	errs = slices.Concat(
		notForDevelopment(fleet.Spec.Template.Annotations, template.Child("metadata", "annotations")),
		fleetSchema(fleet),
		fleet.Validate(agones.NoHooks{}),
	)
	ignored, unsupported := podspec.Check(&fleet.Spec.Template.Spec, template.Child("spec"))
	return append(warnings, ignored...), append(errs, unsupported...)
}

// notForDevelopment refuses the annotation that makes a GameServer one for
// local development. Upstream registers such a GameServer without running
// anything, the game being a process on the developer's machine, and skips
// most of its validation for it. Here it would be run in a container like
// any other, unchecked.
func notForDevelopment(annotations map[string]string, at *field.Path) field.ErrorList {
	if _, development := annotations[agonesv1.DevAddressAnnotation]; !development {
		return nil
	}
	return field.ErrorList{field.Forbidden(at.Key(agonesv1.DevAddressAnnotation), "development GameServers are not supported by scrim")}
}

// withoutRepeats keeps the first of every value, in order.
func withoutRepeats(values []string) []string {
	var kept []string
	for _, value := range values {
		if !slices.Contains(kept, value) {
			kept = append(kept, value)
		}
	}
	return kept
}

func admitFleetAutoscaler(fas *autoscalingv1.FleetAutoscaler) (warnings []string, errs field.ErrorList) {
	fas.APIVersion, fas.Kind = "autoscaling.agones.dev/v1", "FleetAutoscaler"
	fas.Status = autoscalingv1.FleetAutoscalerStatus{}
	fas.ApplyDefaults()
	return nil, append(fleetAutoscalerSchema(fas), fas.Validate()...)
}

// labelsOnly empties the metadata of a template of everything but its labels
// and annotations, and returns a warning for each thing it dropped.
//
// Upstream copies a template's whole metadata onto what is made from it. On
// Kubernetes the API server then refuses or resets the parts a child may not
// start with. Here an owner in the template would hide a GameServerSet from
// its own Fleet, which would make another, without end; and a deletion mark
// would have every child deleted as soon as it is made.
func labelsOnly(meta *metav1.ObjectMeta, at *field.Path) (warnings []string) {
	kept := metav1.ObjectMeta{Labels: meta.Labels, Annotations: meta.Annotations}
	given, without := reflect.ValueOf(*meta), reflect.ValueOf(kept)
	for i := range given.NumField() {
		if given.Field(i).IsZero() || !without.Field(i).IsZero() {
			continue
		}
		name, _, _ := strings.Cut(given.Type().Field(i).Tag.Get("json"), ",")
		warnings = append(warnings, at.Child(name).String()+" is ignored")
	}
	*meta = kept
	return warnings
}

// identity checks the names of an object as the Kubernetes API server
// would: a namespace is a DNS label, a name a DNS subdomain.
//
// The names become part of Docker's container and volume names, which
// podspec.Prefix joins with "_". No DNS name holds a "_", so those can
// always be told apart.
func identity(obj metav1.Object) field.ErrorList {
	meta := field.NewPath("metadata")
	errs := named(obj.GetNamespace(), false, apivalidation.ValidateNamespaceName, meta.Child("namespace"))
	if obj.GetNamespace() == "" {
		errs = append(errs, field.Required(meta.Child("namespace"), ""))
	}
	if obj.GetName() == "" && obj.GetGenerateName() == "" {
		return append(errs, field.Required(meta.Child("name"), "name or generateName is required"))
	}
	errs = append(errs, named(obj.GetName(), false, apivalidation.NameIsDNSSubdomain, meta.Child("name"))...)
	return append(errs, named(obj.GetGenerateName(), true, apivalidation.NameIsDNSSubdomain, meta.Child("generateName"))...)
}

// named holds a name that is set against one of Kubernetes' naming rules.
// prefix says the name will have something appended, as a generateName
// has, so it may end in the "-" that a name may not end in.
//
// Kubernetes' rules can be told about a prefix themselves, but then they
// forgive the character before that "-" as well: "gs_-" passes. Kubernetes
// finds out when it checks the generated name. Nothing checks that one
// here, so the "-" is all that is forgiven.
func named(name string, prefix bool, rule apivalidation.ValidateNameFunc, at *field.Path) field.ErrorList {
	if name == "" {
		return nil
	}
	whole := name
	if prefix && len(name) > 1 && strings.HasSuffix(name, "-") {
		whole = name[:len(name)-1] + "a"
	}
	return reported(name, rule(whole, false), at)
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
