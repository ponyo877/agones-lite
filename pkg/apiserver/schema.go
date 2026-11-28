package apiserver

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// On Kubernetes the API server holds a resource against the OpenAPI schema
// of its CRD before any webhook sees it: it fills in the schema's defaults
// and enforces its limits. Upstream's ApplyDefaults and Validate count on
// that. They never give a Counter its capacity, nor ask whether a number is
// negative or an enum is one of its values. Nothing else does that here, so
// this file repeats the schema for the fields scrim reads.
//
// The schema is install/helm/agones/templates/crds in the Agones repository.

const (
	// maxCounts is the most Counters, or Lists, one GameServer may have.
	maxCounts = 1000
	// maxListItems is the largest List: gameservers.lists.maxItems, a Helm
	// value upstream, at its default.
	maxListItems = 1000
	// defaultCapacity is the capacity of a Counter or List that names none.
	defaultCapacity = 1000
)

// The defaults. Go cannot tell a capacity of 0 from one that was left out,
// so these look at the manifest as it was written.

func fillGameServer(gs *agonesv1.GameServer, written map[string]any) {
	fillCountersAndLists(&gs.Spec, dig(written, "spec"))
}

func fillFleet(fleet *agonesv1.Fleet, written map[string]any) {
	fillCountersAndLists(&fleet.Spec.Template.Spec, dig(written, "spec", "template", "spec"))
	for i := range fleet.Spec.Priorities {
		if fleet.Spec.Priorities[i].Order == "" {
			// The schema's default. Without an order, upstream's
			// comparison is not an order at all: see ascendingByDefault
			// in pkg/gameserversets.
			fleet.Spec.Priorities[i].Order = agonesv1.GameServerPriorityAscending
		}
	}
}

// fillCountersAndLists gives every Counter and List whose capacity the
// manifest leaves out the default one, and every List without values an
// empty one. written is the GameServer spec of the manifest.
func fillCountersAndLists(spec *agonesv1.GameServerSpec, written any) {
	for name, counter := range spec.Counters {
		if dig(written, "counters", name, "capacity") == nil {
			counter.Capacity = defaultCapacity
			spec.Counters[name] = counter
		}
	}
	for name, list := range spec.Lists {
		if dig(written, "lists", name, "capacity") == nil {
			list.Capacity = defaultCapacity
		}
		// Kubernetes drops a null before it fills in defaults, so values
		// left out and values: null both end up as [].
		if list.Values == nil {
			list.Values = []string{}
		}
		spec.Lists[name] = list
	}
}

// dig follows path into a manifest. It returns nil where the manifest says
// nothing: the key is missing, or stands there without a value.
func dig(written any, path ...string) any {
	for _, key := range path {
		object, _ := written.(map[string]any)
		written = object[key]
	}
	return written
}

// The limits. Each function is one object of the schema, and reads like it.
//
// A zero value passes every check on an optional field. That is how Go
// spells "not set", and a field that is not set is not checked.

func gameServerSpecSchema(spec *agonesv1.GameServerSpec, at *field.Path) field.ErrorList {
	return slices.Concat(
		dnsLabel(spec.Container, at.Child("container")),
		oneOf(spec.Scheduling, at.Child("scheduling"), apis.Packed, apis.Distributed),
		each(spec.Ports, at.Child("ports"), gameServerPortSchema),
		staticPortsSchema(spec.Ports, at.Child("ports")),
		oneOf(spec.SdkServer.LogLevel, at.Child("sdkServer", "logLevel"),
			agonesv1.SdkServerLogLevelError, agonesv1.SdkServerLogLevelInfo, agonesv1.SdkServerLogLevelDebug, agonesv1.SdkServerLogLevelTrace),
		portNumber(spec.SdkServer.GRPCPort, at.Child("sdkServer", "grpcPort")),
		portNumber(spec.SdkServer.HTTPPort, at.Child("sdkServer", "httpPort")),
		healthSchema(spec.Health, at.Child("health")),
		atMost(len(spec.Counters), maxCounts, at.Child("counters")),
		eachKey(spec.Counters, at.Child("counters"), counterSchema),
		atMost(len(spec.Lists), maxCounts, at.Child("lists")),
		eachKey(spec.Lists, at.Child("lists"), listSchema),
	)
}

func gameServerPortSchema(p agonesv1.GameServerPort, at *field.Path) field.ErrorList {
	return slices.Concat(
		oneOf(p.PortPolicy, at.Child("portPolicy"), agonesv1.Dynamic, agonesv1.Static, agonesv1.Passthrough, agonesv1.None),
		oneOf(p.Protocol, at.Child("protocol"), corev1.ProtocolUDP, corev1.ProtocolTCP, agonesv1.ProtocolTCPUDP),
		portNumber(p.ContainerPort, at.Child("containerPort")),
		portNumber(p.HostPort, at.Child("hostPort")),
		staticHostPort(p, at.Child("hostPort")),
	)
}

// staticHostPort asks a Static port for its host port. Static means that
// the manifest says which port of the node the game server takes. With
// none, Docker would be asked for port 0, takes that to mean any port that
// is free, and nobody would be told which one it was. The CRD schema does
// not ask for it.
func staticHostPort(p agonesv1.GameServerPort, at *field.Path) field.ErrorList {
	if p.PortPolicy != agonesv1.Static || p.HostPort != 0 {
		return nil
	}
	return field.ErrorList{field.Required(at, "a Static port says which port of the node it takes")}
}

// staticPortsSchema refuses two Static ports on one host port and protocol.
// This one is not in the CRD schema: upstream leaves it to Kubernetes, which
// refuses the Pod. Here the ports would be handed out as asked, and Docker
// would refuse the container.
func staticPortsSchema(ports []agonesv1.GameServerPort, at *field.Path) field.ErrorList {
	var errs field.ErrorList
	taken := map[hostPort]bool{}
	for i, p := range ports {
		if p.PortPolicy != agonesv1.Static {
			continue
		}
		wanted := hostPortsOf(p)
		if slices.ContainsFunc(wanted, func(h hostPort) bool { return taken[h] }) {
			errs = append(errs, field.Duplicate(at.Index(i).Child("hostPort"), p.HostPort))
		}
		for _, h := range wanted {
			taken[h] = true
		}
	}
	return errs
}

// hostPort is a port of the node under one protocol. The same number under
// TCP and under UDP are two ports.
type hostPort struct {
	number   int32
	protocol corev1.Protocol
}

// hostPortsOf returns what a Static port takes on the node: both protocols
// for TCPUDP, and UDP when it names none, as upstream defaults it.
func hostPortsOf(p agonesv1.GameServerPort) []hostPort {
	switch p.Protocol {
	case agonesv1.ProtocolTCPUDP:
		return []hostPort{{p.HostPort, corev1.ProtocolTCP}, {p.HostPort, corev1.ProtocolUDP}}
	case "":
		return []hostPort{{p.HostPort, corev1.ProtocolUDP}}
	}
	return []hostPort{{p.HostPort, p.Protocol}}
}

func healthSchema(h agonesv1.Health, at *field.Path) field.ErrorList {
	return slices.Concat(
		atLeast(h.InitialDelaySeconds, 0, at.Child("initialDelaySeconds")),
		atLeast(h.PeriodSeconds, 0, at.Child("periodSeconds")),
		atLeast(h.FailureThreshold, 0, at.Child("failureThreshold")), // 1 in the schema; 0 is "not set"
	)
}

func counterSchema(c agonesv1.CounterStatus, at *field.Path) field.ErrorList {
	return slices.Concat(
		atLeast(c.Count, 0, at.Child("count")),
		atLeast(c.Capacity, 0, at.Child("capacity")),
	)
}

func listSchema(l agonesv1.ListStatus, at *field.Path) field.ErrorList {
	return slices.Concat(
		within(l.Capacity, 0, maxListItems, at.Child("capacity")),
		atMost(len(l.Values), maxListItems, at.Child("values")),
		distinct(l.Values, at.Child("values")),
	)
}

func fleetSchema(fleet *agonesv1.Fleet) field.ErrorList {
	spec := field.NewPath("spec")
	return slices.Concat(
		atLeast(fleet.Spec.Replicas, 0, spec.Child("replicas")),
		oneOf(fleet.Spec.Scheduling, spec.Child("scheduling"), apis.Packed, apis.Distributed),
		each(fleet.Spec.Priorities, spec.Child("priorities"), prioritySchema),
		gameServerSpecSchema(&fleet.Spec.Template.Spec, spec.Child("template", "spec")),
	)
}

func prioritySchema(p agonesv1.Priority, at *field.Path) field.ErrorList {
	return slices.Concat(
		oneOf(p.Type, at.Child("type"), agonesv1.GameServerPriorityCounter, agonesv1.GameServerPriorityList),
		oneOf(p.Order, at.Child("order"), agonesv1.GameServerPriorityAscending, agonesv1.GameServerPriorityDescending),
	)
}

func fleetAutoscalerSchema(fas *autoscalingv1.FleetAutoscaler) field.ErrorList {
	spec := field.NewPath("spec")
	return slices.Concat(
		fleetNameSchema(fas.Spec.FleetName, spec.Child("fleetName")),
		policySchema(fas.Spec.Policy, spec.Child("policy"), anyPolicy),
		syncSchema(fas.Spec.Sync, spec.Child("sync")),
	)
}

// fleetNamePattern is the schema's own pattern for spec.fleetName.
var fleetNamePattern = regexp.MustCompile(`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`)

func fleetNameSchema(name string, at *field.Path) field.ErrorList {
	switch {
	case name == "":
		return field.ErrorList{field.Required(at, "")}
	case len(name) > validation.LabelValueMaxLength:
		return field.ErrorList{field.TooLong(at, name, validation.LabelValueMaxLength)}
	case !fleetNamePattern.MatchString(name):
		return field.ErrorList{field.Invalid(at, name, "must match "+fleetNamePattern.String())}
	}
	return nil
}

// The policy types each place may hold. A Schedule holds neither a Schedule
// nor a Chain, and a Chain holds no Chain. The schema's Wasm is left out,
// because that policy does not exist here.
var (
	plainPolicies = []autoscalingv1.FleetAutoscalerPolicyType{
		autoscalingv1.BufferPolicyType, autoscalingv1.WebhookPolicyType,
		autoscalingv1.CounterPolicyType, autoscalingv1.ListPolicyType,
	}
	chainedPolicies = append(slices.Clone(plainPolicies), autoscalingv1.SchedulePolicyType)
	anyPolicy       = append(slices.Clone(chainedPolicies), autoscalingv1.ChainPolicyType)
)

// policySchema checks one policy and the policies inside it.
func policySchema(p autoscalingv1.FleetAutoscalerPolicy, at *field.Path, allowed []autoscalingv1.FleetAutoscalerPolicyType) field.ErrorList {
	return slices.Concat(
		required(string(p.Type), at.Child("type")),
		oneOf(p.Type, at.Child("type"), allowed...),
		bufferSchema(p.Buffer, at.Child("buffer")),
		webhookSchema(p.Webhook, at.Child("webhook")),
		counterPolicySchema(p.Counter, at.Child("counter")),
		listPolicySchema(p.List, at.Child("list")),
		scheduleSchema(p.Schedule, at.Child("schedule")),
		chainSchema(p, at.Child("chain")),
	)
}

// chainSchema checks the entries of a Chain, and that it has any. Upstream
// admits a Chain of nothing, which the autoscaler can never apply.
func chainSchema(p autoscalingv1.FleetAutoscalerPolicy, at *field.Path) field.ErrorList {
	if p.Type == autoscalingv1.ChainPolicyType && len(p.Chain) == 0 {
		return field.ErrorList{field.Required(at, "a Chain needs at least one entry")}
	}
	return each(p.Chain, at, chainEntrySchema)
}

func bufferSchema(b *autoscalingv1.BufferPolicy, at *field.Path) field.ErrorList {
	if b == nil {
		return nil
	}
	return slices.Concat(
		atLeast(b.MinReplicas, 0, at.Child("minReplicas")),
		atLeast(b.MaxReplicas, 1, at.Child("maxReplicas")),
	)
}

func counterPolicySchema(c *autoscalingv1.CounterPolicy, at *field.Path) field.ErrorList {
	if c == nil {
		return nil
	}
	return capacitySchema(c.Key, c.MinCapacity, c.MaxCapacity, at)
}

func listPolicySchema(l *autoscalingv1.ListPolicy, at *field.Path) field.ErrorList {
	if l == nil {
		return nil
	}
	return capacitySchema(l.Key, l.MinCapacity, l.MaxCapacity, at)
}

// capacitySchema is the part a Counter policy and a List policy share.
func capacitySchema(key string, minCapacity, maxCapacity int64, at *field.Path) field.ErrorList {
	return slices.Concat(
		required(key, at.Child("key")),
		atLeast(minCapacity, 0, at.Child("minCapacity")),
		atLeast(maxCapacity, 1, at.Child("maxCapacity")),
	)
}

// webhookSchema refuses a webhook the autoscaler could never call. This
// one is not in the CRD schema. Upstream admits a webhook that is a
// Kubernetes Service, of which there are none here, and any text at all
// for a URL; the autoscaler would then fail each time it looks at its Fleet.
func webhookSchema(w *autoscalingv1.URLConfiguration, at *field.Path) field.ErrorList {
	switch {
	case w == nil:
		return nil
	case w.Service != nil:
		return field.ErrorList{field.Forbidden(at.Child("service"), "there are no Services in scrim: give the webhook's url")}
	case w.URL != nil && !isWebAddress(*w.URL):
		return field.ErrorList{field.Invalid(at.Child("url"), *w.URL, "must be an http or https URL")}
	}
	return nil
}

func isWebAddress(text string) bool {
	address, err := url.ParseRequestURI(text)
	return err == nil && (address.Scheme == "http" || address.Scheme == "https") && address.Host != ""
}

// scheduleSchema checks the policy inside a Schedule. Upstream's Validate
// does not look inside one at all, so its checks of a policy are run here.
func scheduleSchema(s *autoscalingv1.SchedulePolicy, at *field.Path) field.ErrorList {
	if s == nil {
		return nil
	}
	return slices.Concat(
		policySchema(s.Policy, at.Child("policy"), plainPolicies),
		s.Policy.ValidatePolicy(at.Child("policy")),
	)
}

func chainEntrySchema(entry autoscalingv1.ChainEntry, at *field.Path) field.ErrorList {
	return policySchema(entry.FleetAutoscalerPolicy, at, chainedPolicies)
}

func syncSchema(sync *autoscalingv1.FleetAutoscalerSync, at *field.Path) field.ErrorList {
	if sync == nil {
		return nil
	}
	return oneOf(sync.Type, at.Child("type"), autoscalingv1.FixedIntervalSyncType)
}

// The checks the schemas are made of.

func required(value string, at *field.Path) field.ErrorList {
	if value != "" {
		return nil
	}
	return field.ErrorList{field.Required(at, "")}
}

func atLeast[N int32 | int64](value, least N, at *field.Path) field.ErrorList {
	if value >= least {
		return nil
	}
	return field.ErrorList{field.Invalid(at, value, fmt.Sprintf("must be greater than or equal to %d", least))}
}

func within(value, least, most int64, at *field.Path) field.ErrorList {
	if least <= value && value <= most {
		return nil
	}
	return field.ErrorList{field.Invalid(at, value, validation.InclusiveRangeError(int(least), int(most)))}
}

func atMost(count, most int, at *field.Path) field.ErrorList {
	if count <= most {
		return nil
	}
	return field.ErrorList{field.TooMany(at, count, most)}
}

func oneOf[S ~string](value S, at *field.Path, allowed ...S) field.ErrorList {
	if value == "" || slices.Contains(allowed, value) {
		return nil
	}
	return field.ErrorList{field.NotSupported(at, value, allowed)}
}

func portNumber(port int32, at *field.Path) field.ErrorList {
	if port == 0 {
		return nil
	}
	return reported(port, validation.IsValidPortNum(int(port)), at)
}

func dnsLabel(name string, at *field.Path) field.ErrorList {
	if name == "" {
		return nil
	}
	return reported(name, validation.IsDNS1123Label(name), at)
}

// distinct is the schema's "x-kubernetes-list-type: set".
func distinct(values []string, at *field.Path) field.ErrorList {
	var errs field.ErrorList
	seen := map[string]bool{}
	for i, value := range values {
		if seen[value] {
			errs = append(errs, field.Duplicate(at.Index(i), value))
		}
		seen[value] = true
	}
	return errs
}

// reported turns the problems apimachinery found with a value into errors.
func reported(value any, problems []string, at *field.Path) field.ErrorList {
	var errs field.ErrorList
	for _, problem := range problems {
		errs = append(errs, field.Invalid(at, value, problem))
	}
	return errs
}

// each checks every item of a list.
func each[T any](list []T, at *field.Path, check func(T, *field.Path) field.ErrorList) field.ErrorList {
	var errs field.ErrorList
	for i, item := range list {
		errs = append(errs, check(item, at.Index(i))...)
	}
	return errs
}

// eachKey checks every entry of a map, in the order of the keys so that the
// same manifest always gets the same answer.
func eachKey[T any](entries map[string]T, at *field.Path, check func(T, *field.Path) field.ErrorList) field.ErrorList {
	var errs field.ErrorList
	for _, key := range slices.Sorted(maps.Keys(entries)) {
		errs = append(errs, check(entries[key], at.Key(key))...)
	}
	return errs
}
