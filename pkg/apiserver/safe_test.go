package apiserver

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/podspec"
	"github.com/ponyo877/scrim/pkg/portallocator"
	"github.com/ponyo877/scrim/pkg/store"
)

// safe is the oracle of FuzzAdmit: what must hold of everything admission
// lets through.
//
// Nothing behind admission looks at a resource again. A Fleet's controller
// copies its template into GameServers, the port allocator gives them
// ports, and the agent hands the result to Docker as it is. So admission
// is the one place where a resource that Docker would refuse, or that
// would lead a controller astray, can still be turned away.
//
// Each check returns what is wrong, in words; none of them uses the code
// admission is made of.
func safe(t *testing.T, api *Server, obj store.Object) {
	t.Helper()
	servers := gameServersMadeOf(obj)
	checks := []struct {
		holds string
		wrong []string
	}{
		{"names are DNS names within the limits", badNames(obj, servers)},
		{"numbers are within their ranges", badNumbers(obj)},
		{"template metadata carries nothing but labels and annotations", extraTemplateMetadata(obj)},
		{"Docker can take the containers", badContainers(servers)},
		{"an autoscaler has a policy it can apply", badPolicies(obj)},
		{"admitting the result again changes nothing", changedByAdmission(api, obj)},
		{"a JSON round trip keeps it equal", changedByJSON(obj)},
	}
	for _, check := range checks {
		assert.Empty(t, check.wrong, "of an admitted %T, this must hold: %s", obj, check.holds)
	}
}

// gameServersMadeOf returns the GameServers that will run because obj was
// admitted: obj itself, or what the GameServerSet of a Fleet makes from the
// Fleet's template, the way its controller does. Nothing admits those.
func gameServersMadeOf(obj store.Object) []*agonesv1.GameServer {
	switch obj := obj.(type) {
	case *agonesv1.GameServer:
		gs := obj.DeepCopy()
		gs.Name = storedName(gs)
		return []*agonesv1.GameServer{gs}
	case *agonesv1.Fleet:
		fleet := obj.DeepCopy()
		fleet.Name = storedName(fleet)
		set := fleet.GameServerSet()
		set.Name = storedName(set)
		gs := set.GameServer()
		gs.ApplyDefaults()
		gs.Name = storedName(gs)
		return []*agonesv1.GameServer{gs}
	}
	return nil
}

// storedName is the name the store gives obj on Create: its own, or its
// generateName cut to 58 characters, and five more.
func storedName(obj metav1.Object) string {
	if obj.GetName() != "" {
		return obj.GetName()
	}
	base := obj.GetGenerateName()
	return base[:min(len(base), 58)] + "x7k2p"
}

// Names.

var (
	aLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	aName  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// badNames holds every name against the rule of its place. A namespace and
// the names inside a Pod are DNS labels; the name of a resource is a DNS
// name, and one of 63 characters at most where it becomes a label or part
// of a Docker name: for GameServers, and for the Fleets they are named after.
func badNames(obj store.Object, servers []*agonesv1.GameServer) []string {
	wrong := misnamed("the namespace", obj.GetNamespace(), aLabel, 63)
	wrong = append(wrong, misnamed("the name", storedName(obj), aName, 253)...)
	if scaler, ok := obj.(*autoscalingv1.FleetAutoscaler); ok {
		wrong = append(wrong, misnamed("the Fleet's name", scaler.Spec.FleetName, aName, 63)...)
	}
	for _, gs := range servers {
		wrong = append(wrong, misnamed("the GameServer's name", gs.Name, aName, 63)...)
		wrong = append(wrong, misnamed("spec.container", gs.Spec.Container, aLabel, 63)...)
		for _, c := range gs.Spec.Template.Spec.Containers {
			wrong = append(wrong, misnamed("a container's name", c.Name, aLabel, 63)...)
		}
		for _, v := range gs.Spec.Template.Spec.Volumes {
			wrong = append(wrong, misnamed("a volume's name", v.Name, aLabel, 63)...)
		}
	}
	return wrong
}

func misnamed(what, name string, rule *regexp.Regexp, longest int) []string {
	if rule.MatchString(name) && len(name) <= longest {
		return nil
	}
	return []string{fmt.Sprintf("%s is %q", what, name)}
}

// Numbers.

// badNumbers looks at every number of the resource, wherever it is. None
// is negative, and what is called a port is a port, or 0 for "not set". On
// top of that come the sizes the CRD schema limits.
func badNumbers(obj store.Object) []string {
	var wrong []string
	eachNumber(reflect.ValueOf(obj), "", func(path string, n int64) {
		switch {
		case n < 0:
			wrong = append(wrong, fmt.Sprintf("%s is negative", path))
		case strings.HasSuffix(path, "Port") && n > 65535:
			wrong = append(wrong, fmt.Sprintf("%s is %d, which is not a port", path, n))
		}
	})
	for _, gs := range gameServersMadeOf(obj) {
		wrong = append(wrong, tooMany(gs.Spec)...)
	}
	return wrong
}

func tooMany(spec agonesv1.GameServerSpec) []string {
	var wrong []string
	if len(spec.Counters) > 1000 || len(spec.Lists) > 1000 {
		wrong = append(wrong, fmt.Sprintf("%d counters and %d lists", len(spec.Counters), len(spec.Lists)))
	}
	for name, list := range spec.Lists {
		if list.Capacity > 1000 || len(list.Values) > 1000 {
			wrong = append(wrong, fmt.Sprintf("list %s has a capacity of %d and %d values", name, list.Capacity, len(list.Values)))
		}
	}
	return wrong
}

// eachNumber calls visit for every number under v, with the path of Go
// field names that leads to it. A quantity such as "500m" comes as its
// sign.
func eachNumber(v reflect.Value, path string, visit func(path string, n int64)) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		visit(path, v.Int())
	case reflect.Pointer:
		eachNumber(v.Elem(), path, visit)
	case reflect.Slice:
		for i := range v.Len() {
			eachNumber(v.Index(i), fmt.Sprintf("%s[%d]", path, i), visit)
		}
	case reflect.Map:
		for entry := v.MapRange(); entry.Next(); {
			eachNumber(entry.Value(), fmt.Sprintf("%s[%v]", path, entry.Key()), visit)
		}
	case reflect.Struct:
		eachNumberOfStruct(v, path, visit)
	}
}

func eachNumberOfStruct(v reflect.Value, path string, visit func(path string, n int64)) {
	if quantity, ok := v.Interface().(apiresource.Quantity); ok {
		visit(path, int64(quantity.Sign()))
		return
	}
	for i := range v.NumField() {
		if field := v.Type().Field(i); field.IsExported() {
			eachNumber(v.Field(i), path+"."+field.Name, visit)
		}
	}
}

// Templates.

// extraTemplateMetadata finds what a template says about the objects made
// from it beyond their labels and annotations: an owner, a finalizer, a
// deletion. All of it would be copied onto every child.
func extraTemplateMetadata(obj store.Object) []string {
	templates := map[string]metav1.ObjectMeta{}
	switch obj := obj.(type) {
	case *agonesv1.GameServer:
		templates["spec.template.metadata"] = obj.Spec.Template.ObjectMeta
	case *agonesv1.Fleet:
		templates["spec.template.metadata"] = obj.Spec.Template.ObjectMeta
		templates["spec.template.spec.template.metadata"] = obj.Spec.Template.Spec.Template.ObjectMeta
	}
	var wrong []string
	for where, meta := range templates {
		if !reflect.DeepEqual(meta, metav1.ObjectMeta{Labels: meta.Labels, Annotations: meta.Annotations}) {
			wrong = append(wrong, fmt.Sprintf("%s holds %+v", where, meta))
		}
	}
	return wrong
}

// Containers.

// The least Docker and the kernel take for a container's limits, found by
// asking a Docker 28 daemon, and the longest name of a volume.
const (
	leastMemory = 6 << 20    // "Minimum memory limit allowed is 6MB"
	leastCPU    = 10_000_000 // a hundredth of a core: less is under 1ms of CPU time in every 100ms
	longestName = 255        // a volume is a directory, and that is as long as a file name gets
)

var dockerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// rangesFor gives a node the default range and every other range gs names,
// none of them overlapping, so that the containers of gs are made.
func rangesFor(gs *agonesv1.GameServer) map[string]scrimv1.PortRange {
	ranges := map[string]scrimv1.PortRange{agonesv1.DefaultPortRange: {Min: 7000, Max: 7999}}
	for i, name := range portallocator.Named(gs) {
		if _, has := ranges[name]; !has {
			low := int32(10_000 + 100*i)
			ranges[name] = scrimv1.PortRange{Min: low, Max: low + 99}
		}
	}
	return ranges
}

// badContainers takes each GameServer as far as the agent does, to the
// containers it would ask Docker for, and reports what Docker would
// refuse, or would run differently from what the manifest says.
func badContainers(servers []*agonesv1.GameServer) []string {
	var wrong []string
	for _, gs := range servers {
		gs = gs.DeepCopy()
		gs.Status.NodeName = "node-1"
		if !portallocator.Allocate(gs, rangesFor(gs), map[int32]bool{}) {
			continue // it asks for more ports than a node has, and waits: that is safe
		}
		specs := podspec.Containers(gs, podspec.Sidecar{Image: "example/sidecar:1", AgentAddress: "host.docker.internal:9447", Token: "secret"})
		pod := gs.Spec.Template.Spec
		wrong = append(wrong, badGameContainer(gs, specs)...)
		wrong = append(wrong, badDockerNames(specs)...)
		wrong = append(wrong, badPublishedPorts(specs[0].Ports)...)
		wrong = append(wrong, badGracePeriod(gs)...)
		for i, spec := range specs[1:] {
			wrong = append(wrong, badSpec(spec, pod.Containers[i])...)
		}
	}
	return wrong
}

// badGameContainer: exactly one container is the game, the one the health
// checks and the logs are about.
func badGameContainer(gs *agonesv1.GameServer, specs []docker.Spec) []string {
	games := 0
	for _, spec := range specs {
		if spec.Labels[podspec.LabelRole] == podspec.RoleGame {
			games++
		}
	}
	if games == 1 {
		return nil
	}
	return []string{fmt.Sprintf("%d of its containers are called %q, the game", games, gs.Spec.Container)}
}

// badDockerNames: every container has a name Docker takes, and its own.
func badDockerNames(specs []docker.Spec) []string {
	var wrong []string
	seen := map[string]bool{}
	for _, spec := range specs {
		if !dockerName.MatchString(spec.Name) || len(spec.Name) > longestName || seen[spec.Name] {
			wrong = append(wrong, fmt.Sprintf("a container is named %q", spec.Name))
		}
		seen[spec.Name] = true
	}
	return wrong
}

// badPublishedPorts: every port is a port, on TCP or UDP, and no two ask
// for the same port of the node.
func badPublishedPorts(ports []docker.Port) []string {
	var wrong []string
	taken := map[string]bool{}
	for _, p := range ports {
		onNode := fmt.Sprintf("%d/%s", p.Host, p.Protocol)
		proper := isPort(p.Host) && isPort(p.Container) && (p.Protocol == "tcp" || p.Protocol == "udp")
		if !proper || taken[onNode] {
			wrong = append(wrong, fmt.Sprintf("it publishes %+v", p))
		}
		taken[onNode] = true
	}
	return wrong
}

func isPort(n int32) bool {
	return 1 <= n && n <= 65535
}

// badGracePeriod: the time to stop in is the one the manifest names, and
// one Docker can be told as a number of seconds.
func badGracePeriod(gs *agonesv1.GameServer) []string {
	grace := podspec.GracePeriod(gs)
	seconds := gs.Spec.Template.Spec.TerminationGracePeriodSeconds
	unchanged := seconds == nil || grace.Seconds() == float64(*seconds)
	if unchanged && 0 <= grace && grace <= math.MaxInt32*time.Second {
		return nil
	}
	return []string{fmt.Sprintf("its containers get %s to stop", grace)}
}

// badSpec holds one container of the Pod against what Docker takes.
func badSpec(spec docker.Spec, c corev1.Container) []string {
	return slices.Concat(
		badImage(spec),
		badEnv(spec, c),
		badPaths(spec),
		badUser(spec),
		badBinds(spec),
		badLimits(spec),
	)
}

func badImage(spec docker.Spec) []string {
	if docker.ValidImage(spec.Image) && len(spec.Ports) == 0 && len(spec.ExtraHosts) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("container %s has image %q, ports %v, hosts %v", spec.Name, spec.Image, spec.Ports, spec.ExtraHosts)}
}

// badEnv: Docker takes a variable as "NAME=value" and reads the name up to
// the first "=". Read back that way, each must be what the manifest says.
func badEnv(spec docker.Spec, c corev1.Container) []string {
	var wrong []string
	for i, e := range c.Env {
		name, value, _ := strings.Cut(spec.Env[i], "=")
		if name == "" || name != e.Name || value != e.Value || strings.Contains(spec.Env[i], "\x00") {
			wrong = append(wrong, fmt.Sprintf("container %s gets %q for the variable %q=%q", spec.Name, spec.Env[i], e.Name, e.Value))
		}
	}
	return wrong
}

// badPaths: no text handed to a process can hold a NUL, and Docker wants
// the working directory from the root.
func badPaths(spec docker.Spec) []string {
	var wrong []string
	if spec.WorkingDir != "" && !strings.HasPrefix(spec.WorkingDir, "/") {
		wrong = append(wrong, fmt.Sprintf("container %s works in %q", spec.Name, spec.WorkingDir))
	}
	for _, word := range slices.Concat(spec.Command, spec.Args, []string{spec.WorkingDir}) {
		if strings.Contains(word, "\x00") {
			wrong = append(wrong, fmt.Sprintf("container %s is started with %q", spec.Name, word))
		}
	}
	return wrong
}

// badUser: a user is "uid" or "uid:gid", each a number Linux has room for.
func badUser(spec docker.Spec) []string {
	if spec.User == "" {
		return nil
	}
	ids := strings.Split(spec.User, ":")
	proper := len(ids) <= 2
	for _, id := range ids {
		_, err := strconv.ParseUint(id, 10, 31)
		proper = proper && err == nil
	}
	if proper {
		return nil
	}
	return []string{fmt.Sprintf("container %s runs as %q", spec.Name, spec.User)}
}

// badBinds: a bind is "source:target" or "source:target:ro". The source is
// a path from the root or the name of a volume, the target a path from the
// root and not the root itself, and no two binds have the same target.
func badBinds(spec docker.Spec) []string {
	var wrong []string
	targets := map[string]bool{}
	for _, bind := range spec.Binds {
		parts := strings.Split(bind, ":")
		if !properBind(parts) || targets[parts[1]] {
			wrong = append(wrong, fmt.Sprintf("container %s binds %q", spec.Name, bind))
			continue
		}
		targets[parts[1]] = true
	}
	return wrong
}

func properBind(parts []string) bool {
	if len(parts) < 2 || len(parts) > 3 || (len(parts) == 3 && parts[2] != "ro") {
		return false
	}
	source, target := parts[0], parts[1]
	volume := dockerName.MatchString(source) && len(source) <= longestName && strings.HasPrefix(source, "scrim_")
	return (volume || strings.HasPrefix(source, "/")) && strings.HasPrefix(target, "/") && target != "/"
}

func badLimits(spec docker.Spec) []string {
	cpu := spec.NanoCPUs == 0 || spec.NanoCPUs >= leastCPU
	memory := spec.MemoryBytes == 0 || spec.MemoryBytes >= leastMemory
	if cpu && memory {
		return nil
	}
	return []string{fmt.Sprintf("container %s is limited to %d billionths of a CPU and %d bytes", spec.Name, spec.NanoCPUs, spec.MemoryBytes)}
}

// Autoscalers.

// badPolicies: a policy says what it is and brings the settings for it, at
// every level of a Schedule or a Chain, and the autoscaler looks at its
// Fleet at least once in a while. A policy without its settings would be
// found out only when it is first applied, and then every time.
func badPolicies(obj store.Object) []string {
	scaler, ok := obj.(*autoscalingv1.FleetAutoscaler)
	if !ok {
		return nil
	}
	wrong := badPolicy(scaler.Spec.Policy, "spec.policy")
	if sync := scaler.Spec.Sync; sync == nil || sync.FixedInterval.Seconds < 1 {
		wrong = append(wrong, fmt.Sprintf("spec.sync is %+v", sync))
	}
	return wrong
}

func badPolicy(p autoscalingv1.FleetAutoscalerPolicy, at string) []string {
	settings := map[autoscalingv1.FleetAutoscalerPolicyType]bool{
		autoscalingv1.BufferPolicyType:   p.Buffer != nil,
		autoscalingv1.WebhookPolicyType:  p.Webhook != nil && p.Webhook.Service == nil && onTheWeb(p.Webhook.URL),
		autoscalingv1.CounterPolicyType:  p.Counter != nil,
		autoscalingv1.ListPolicyType:     p.List != nil,
		autoscalingv1.SchedulePolicyType: p.Schedule != nil,
		autoscalingv1.ChainPolicyType:    len(p.Chain) > 0,
	}
	var wrong []string
	if !settings[p.Type] {
		wrong = append(wrong, fmt.Sprintf("%s is of type %q, without the settings for it", at, p.Type))
	}
	if p.Schedule != nil {
		wrong = append(wrong, badPolicy(p.Schedule.Policy, at+".schedule.policy")...)
	}
	for i, entry := range p.Chain {
		wrong = append(wrong, badPolicy(entry.FleetAutoscalerPolicy, fmt.Sprintf("%s.chain[%d]", at, i))...)
	}
	return wrong
}

// onTheWeb: a webhook is somewhere a request can be sent. There are no
// Services here to find it by.
func onTheWeb(address *string) bool {
	if address == nil {
		return false
	}
	parsed, err := url.Parse(*address)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// Twice, and through JSON.

// changedByAdmission admits the result once more, as the manifest it is.
// "apply" does that every time a manifest is applied again, and what it
// gets must be what is there: or every apply would be a change.
func changedByAdmission(api *Server, obj store.Object) []string {
	manifest, err := json.Marshal(obj)
	if err != nil {
		return []string{err.Error()}
	}
	var again store.Object
	var ok bool
	switch obj.(type) {
	case *agonesv1.GameServer:
		again, ok = admitted(api.servers, obj.GetNamespace(), manifest)
	case *agonesv1.Fleet:
		again, ok = admitted(api.fleets, obj.GetNamespace(), manifest)
	case *autoscalingv1.FleetAutoscaler:
		again, ok = admitted(api.scalers, obj.GetNamespace(), manifest)
	}
	if !ok {
		return []string{"it is refused the second time: " + string(manifest)}
	}
	return difference(obj, again)
}

// changedByJSON writes the result as the store writes it to its file, and
// reads it back. What is in memory must be what is in the file.
func changedByJSON(obj store.Object) []string {
	written, err := json.Marshal(obj)
	if err != nil {
		return []string{err.Error()}
	}
	read := reflect.New(reflect.TypeOf(obj).Elem()).Interface()
	if err := json.Unmarshal(written, read); err != nil {
		return []string{err.Error()}
	}
	return difference(obj, read)
}

// difference compares the way Kubernetes does: an empty list is no list,
// and a time is the instant it names.
func difference(was, now any) []string {
	if apiequality.Semantic.DeepEqual(was, now) {
		return nil
	}
	before, _ := json.Marshal(was)
	after, _ := json.Marshal(now)
	return []string{fmt.Sprintf("it was %s\nand is %s\n(if these are the same, they differ in what JSON does not keep, like a fraction of a second)", before, after)}
}
