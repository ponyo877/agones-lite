package apiserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/agones"
)

const scalerManifest = `
apiVersion: autoscaling.agones.dev/v1
kind: FleetAutoscaler
metadata:
  name: scaler
spec:
  fleetName: simple
  policy:
    type: Buffer
    buffer:
      bufferSize: 2
      maxReplicas: 10
`

const (
	gameServersPath = "/v1/namespaces/default/gameservers"
	fleetPath       = "/v1/namespaces/default/fleets/simple"
	scalerPath      = "/v1/namespaces/default/fleetautoscalers/scaler"
)

// replace changes one place of a manifest, and fails the test when the
// place is not there: a case that changes nothing would pass for nothing.
func replace(t testing.TB, manifest, old, changed string) string {
	t.Helper()
	require.Contains(t, manifest, old)
	return strings.Replace(manifest, old, changed, 1)
}

// A Fleet's template metadata is copied to every GameServerSet and
// GameServer. An owner or a deletion mark in it would make the children
// look like someone else's, or like they are already going away.
func TestTemplateMetadataKeepsOnlyLabelsAndAnnotations(t *testing.T) {
	h := newServer(t)
	body := fleetWithTemplateMetadata(t)

	rec := call(t, h, http.MethodPut, fleetPath, body)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.ElementsMatch(t, []string{
		"spec.template.metadata.deletionTimestamp is ignored",
		"spec.template.metadata.finalizers is ignored",
		"spec.template.metadata.ownerReferences is ignored",
	}, rec.Header().Values("Warning"))
	fleet := getFleet(t, h)
	assert.Equal(t, metav1.ObjectMeta{
		Labels:      map[string]string{"tier": "gold"},
		Annotations: map[string]string{"note": "kept"},
	}, fleet.Spec.Template.ObjectMeta)

	set := fleet.GameServerSet()
	assert.True(t, metav1.IsControlledBy(set, fleet), "or the Fleet would not know its own GameServerSet and make another")
	assert.Nil(t, set.DeletionTimestamp)
	assert.Nil(t, set.GameServer().DeletionTimestamp)
}

// fleetWithTemplateMetadata is a Fleet whose template claims an owner, a
// finalizer and a deletion.
func fleetWithTemplateMetadata(t testing.TB) string {
	return replace(t, fleetManifest, "  template:\n    spec:", `  template:
    metadata:
      labels: {tier: gold}
      annotations: {note: kept}
      deletionTimestamp: "2020-01-01T00:00:00Z"
      finalizers: [example.com/hold]
      ownerReferences:
        - {apiVersion: v1, kind: Pod, name: other, uid: x, controller: true}
    spec:`)
}

// gameServerWithPodMetadata is a GameServer whose Pod template claims an owner.
func gameServerWithPodMetadata(t testing.TB) string {
	return replace(t, manifest, "  template:\n    spec:", `  template:
    metadata:
      labels: {tier: gold}
      ownerReferences:
        - {apiVersion: v1, kind: Pod, name: other, uid: x, controller: true}
    spec:`)
}

func TestPodTemplateMetadataKeepsOnlyLabelsAndAnnotations(t *testing.T) {
	h := newServer(t)
	body := gameServerWithPodMetadata(t)

	rec := call(t, h, http.MethodPost, gameServersPath, body)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Values("Warning"), "spec.template.metadata.ownerReferences is ignored")
	gs := &agonesv1.GameServer{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), gs))
	assert.Equal(t, metav1.ObjectMeta{Labels: map[string]string{"tier": "gold"}}, gs.Spec.Template.ObjectMeta)
}

// numbered is a YAML list of n different strings.
func numbered(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("p%d", i)
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// refusal is a request admission must refuse, and the field it must blame.
// The tables of them are also where FuzzAdmit starts from.
type refusal struct {
	method, path, body string
	want               string
}

// Upstream's Validate relies on the CRD schema having run first. Nothing
// runs it here, so its limits are checked by hand.
func TestCreateRejectsWhatTheCRDSchemaWould(t *testing.T) {
	for name, tc := range refusedByTheSchema(t) {
		t.Run(name, func(t *testing.T) {
			rec := call(t, newServer(t), tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.want+":")
		})
	}
}

func refusedByTheSchema(t testing.TB) map[string]refusal {
	schedule := `policy:
    type: Schedule
    schedule:
      activePeriod: {startCron: "0 1 * * 0", duration: 5h}
      policy:
        type: Buffer
        buffer: {bufferSize: 2, maxReplicas: 10}
`
	return map[string]refusal{
		"a negative counter": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  counters: {rooms: {count: -1, capacity: 10}}\n"),
			"spec.counters[rooms].count",
		},
		"a negative counter capacity": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  counters: {rooms: {count: 0, capacity: -1}}\n"),
			"spec.counters[rooms].capacity",
		},
		"a list larger than a list may be": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  lists: {players: {capacity: 1001}}\n"),
			"spec.lists[players].capacity",
		},
		"a list with more items than a list may hold": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  lists: {players: {capacity: 1000, values: "+numbered(1001)+"}}\n"),
			"spec.lists[players].values",
		},
		"a list that holds an item twice": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  lists: {players: {capacity: 10, values: [ann, bob, ann]}}\n"),
			"spec.lists[players].values[2]",
		},
		"a host port that is not a port": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "containerPort: 7654", "containerPort: 7654\n      portPolicy: Static\n      hostPort: 99999"),
			"spec.ports[0].hostPort",
		},
		"a container port that is not a port": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "containerPort: 7654", "containerPort: 70000"),
			"spec.ports[0].containerPort",
		},
		"an unknown port policy": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "containerPort: 7654", "containerPort: 7654\n      portPolicy: Sometimes"),
			"spec.ports[0].portPolicy",
		},
		"an unknown protocol": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "containerPort: 7654", "containerPort: 7654\n      protocol: SCTP"),
			"spec.ports[0].protocol",
		},
		"an unknown scheduling strategy": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  scheduling: Random\n"),
			"spec.scheduling",
		},
		"an SDK port that is not a port": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  sdkServer: {grpcPort: 70000}\n"),
			"spec.sdkServer.grpcPort",
		},
		"a negative health period": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  health: {disabled: true, periodSeconds: -1}\n"),
			"spec.health.periodSeconds",
		},
		"a fleet of negative size": {
			http.MethodPut, fleetPath,
			replace(t, fleetManifest, "replicas: 2", "replicas: -5"),
			"spec.replicas",
		},
		"a fleet with an unknown scheduling strategy": {
			http.MethodPut, fleetPath,
			replace(t, fleetManifest, "replicas: 2", "replicas: 2\n  scheduling: Random"),
			"spec.scheduling",
		},
		"a fleet priority that is neither Counter nor List": {
			http.MethodPut, fleetPath,
			replace(t, fleetManifest, "replicas: 2", "replicas: 2\n  priorities: [{type: Gauge, key: rooms}]"),
			"spec.priorities[0].type",
		},
		"a fleet priority in an unknown order": {
			http.MethodPut, fleetPath,
			replace(t, fleetManifest, "replicas: 2", "replicas: 2\n  priorities: [{type: Counter, key: rooms, order: Sideways}]"),
			"spec.priorities[0].order",
		},
		"a negative counter in a fleet's template": {
			http.MethodPut, fleetPath,
			replace(t, fleetManifest, "    spec:\n      ports:", "    spec:\n      counters: {rooms: {count: -1}}\n      ports:"),
			"spec.template.spec.counters[rooms].count",
		},
		"an autoscaler without a fleet": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "  fleetName: simple\n", ""),
			"spec.fleetName",
		},
		"an autoscaler for a fleet that could not have that name": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "fleetName: simple", "fleetName: Not_A_Name"),
			"spec.fleetName",
		},
		"an unknown policy": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "type: Buffer", "type: Bufer"),
			"spec.policy.type",
		},
		"a policy without a type": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "    type: Buffer\n", ""),
			"spec.policy.type",
		},
		"a counter policy without a key": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "type: Buffer", "type: Counter\n    counter: {bufferSize: 2, maxCapacity: 10}"),
			"spec.policy.counter.key",
		},
		"a scheduled policy that could never have a replica": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "policy:\n    type: Buffer\n    buffer:\n      bufferSize: 2\n      maxReplicas: 10\n",
				replace(t, schedule, "bufferSize: 2, maxReplicas: 10", "bufferSize: 2, maxReplicas: 0")),
			"spec.policy.schedule.policy.buffer.maxReplicas",
		},
		"a schedule inside a schedule": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "policy:\n    type: Buffer\n    buffer:\n      bufferSize: 2\n      maxReplicas: 10\n",
				replace(t, schedule, "        type: Buffer", "        type: Schedule")),
			"spec.policy.schedule.policy.type",
		},
		"a chain inside a chain": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "type: Buffer", "type: Chain\n    chain: [{id: a, type: Chain}]"),
			"spec.policy.chain[0].type",
		},
		"a port from a range of ports that is not there": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "containerPort: 7654", "containerPort: 7654\n      range: games"),
			"spec.ports[0].range",
		},
		"an unknown log level": {
			http.MethodPost, gameServersPath,
			replace(t, manifest, "spec:\n", "spec:\n  sdkServer: {logLevel: Loud}\n"),
			"spec.sdkServer.logLevel",
		},
		"a webhook that is a Service, of which there are none": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "type: Buffer", "type: Webhook\n    webhook: {service: {name: scaler, namespace: default, path: scale}}"),
			"spec.policy.webhook.service",
		},
		"a webhook at no address a request could go to": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "type: Buffer", "type: Webhook\n    webhook: {url: scaler/scale}"),
			"spec.policy.webhook.url",
		},
		"a scheduled policy without its settings": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "policy:\n    type: Buffer\n    buffer:\n      bufferSize: 2\n      maxReplicas: 10\n",
				replace(t, schedule, "\n        buffer: {bufferSize: 2, maxReplicas: 10}", "")),
			"spec.policy.schedule.policy.buffer",
		},
		"a chain of no policies": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "    type: Buffer\n    buffer:\n      bufferSize: 2\n      maxReplicas: 10\n", "    type: Chain\n"),
			"spec.policy.chain",
		},
		"an unknown way to sync": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "spec:\n", "spec:\n  sync: {type: Sometimes}\n"),
			"spec.sync.type",
		},
	}
}

// Kubernetes refuses a Pod that asks for one host port twice, and Docker a
// container. Two Static ports may share a number only on different protocols.
func TestStaticPortsOfOneGameServerMustNotClash(t *testing.T) {
	for name, tc := range staticPortCases() {
		t.Run(name, func(t *testing.T) {
			rec := call(t, newServer(t), http.MethodPost, gameServersPath, withPorts(t, tc.ports))

			if tc.clash == "" {
				assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				return
			}
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.clash+": Duplicate value")
		})
	}

	// In a Fleet the template has not been given its defaults yet.
	fleet := replace(t, fleetManifest, "        - name: default\n          containerPort: 7654\n",
		"        - {name: a, portPolicy: Static, containerPort: 7654, hostPort: 7000}\n"+
			"        - {name: b, portPolicy: Static, containerPort: 7654, hostPort: 7000, protocol: UDP}\n")
	rec := call(t, newServer(t), http.MethodPut, fleetPath, fleet)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "spec.template.spec.ports[1].hostPort: Duplicate value")
}

// withPorts is the GameServer manifest with other ports.
func withPorts(t testing.TB, ports string) string {
	return replace(t, manifest, "    - name: default\n      containerPort: 7654\n", ports)
}

// static is one Static port of a manifest.
func static(name string, hostPort int, protocol string) string {
	return fmt.Sprintf("    - {name: %s, portPolicy: Static, containerPort: 7654, hostPort: %d%s}\n", name, hostPort, protocol)
}

type staticPortCase struct {
	ports string
	clash string // the field that is refused; empty when the ports are fine
}

func staticPortCases() map[string]staticPortCase {
	return map[string]staticPortCase{
		"the same port twice": {
			static("a", 7000, "") + static("b", 7000, ""), "spec.ports[1].hostPort",
		},
		"the same port and protocol, one of them by default": {
			static("a", 7000, "") + static("b", 7000, ", protocol: UDP"), "spec.ports[1].hostPort",
		},
		"TCPUDP takes both protocols": {
			static("a", 7000, ", protocol: TCPUDP") + static("b", 7001, "") + static("c", 7000, ", protocol: TCP"), "spec.ports[2].hostPort",
		},
		"the same port on different protocols": {
			static("a", 7000, ", protocol: TCP") + static("b", 7000, ", protocol: UDP"), "",
		},
		"different ports": {
			static("a", 7000, "") + static("b", 7001, ""), "",
		},
		"a dynamic port is not checked: it has no host port yet": {
			static("a", 7000, "") + "    - {name: b, containerPort: 7654}\n" + "    - {name: c, containerPort: 7654}\n", "",
		},
	}
}

// The limits themselves are still fine.
func TestCreateAcceptsWhatTheCRDSchemaWould(t *testing.T) {
	h := newServer(t)
	body := replace(t, manifest, "spec:\n", "spec:\n"+
		"  counters: {rooms: {count: 0, capacity: 0}}\n"+
		"  lists: {players: {capacity: 1000, values: "+numbered(1000)+"}}\n"+
		"  health: {disabled: true}\n")
	body = replace(t, body, "containerPort: 7654", "containerPort: 65535\n      portPolicy: Static\n      hostPort: 1")

	rec := call(t, h, http.MethodPost, gameServersPath, body)

	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, scalerPath, scalerManifest).Code)
	zero := replace(t, fleetManifest, "replicas: 2", "replicas: 0")
	assert.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, fleetPath, zero).Code)
}

// The CRD schema also gives defaults, and upstream's own samples lean on
// them: examples/fleet.yaml has a list without a capacity.
func TestCountersAndListsGetTheSchemasDefaultCapacity(t *testing.T) {
	h := newServer(t)
	body := replace(t, manifest, "spec:\n", `spec:
  counters:
    rooms: {count: 1}
    closed: {capacity: 0}
  lists:
    players: {}
    spectators: {capacity: }
    banned: {capacity: 0, values: []}
    friends: {capacity: 5}
`)
	rec := call(t, h, http.MethodPost, gameServersPath, body)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	gs := &agonesv1.GameServer{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), gs))
	assert.Equal(t, map[string]agonesv1.CounterStatus{
		"rooms":  {Count: 1, Capacity: 1000},
		"closed": {Capacity: 0},
	}, gs.Spec.Counters)
	capacities := map[string]int64{}
	for name, list := range gs.Spec.Lists {
		capacities[name] = list.Capacity
	}
	assert.Equal(t, map[string]int64{"players": 1000, "spectators": 1000, "banned": 0, "friends": 5}, capacities)
	assert.Equal(t, gs.Spec.Counters, gs.Status.Counters, "the status starts from the spec, defaults and all")

	fleet := replace(t, fleetManifest, "    spec:\n      ports:", "    spec:\n      lists: {players: {}}\n      ports:")
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, fleetPath, fleet).Code)
	assert.Equal(t, int64(1000), getFleet(t, h).Spec.Template.Spec.Lists["players"].Capacity)
}

// The schema's default. Left empty, the order made the removal order of a
// scale-down depend on the order its GameServers happened to be listed in.
func TestAPriorityWithoutAnOrderIsAscending(t *testing.T) {
	h := newServer(t)
	fleet := replace(t, fleetManifest, "  replicas: 2\n", `  replicas: 2
  priorities:
    - {type: Counter, key: rooms}
    - {type: List, key: players, order: Descending}
`)

	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, fleetPath, fleet).Code)

	assert.Equal(t, []agonesv1.Priority{
		{Type: "Counter", Key: "rooms", Order: "Ascending"},
		{Type: "List", Key: "players", Order: "Descending"},
	}, getFleet(t, h).Spec.Priorities)
}

// Status is the controllers' to write. A client that could set it would
// have a GameServer that is Ready, and so allocatable, with nothing running.
func TestAStatusSentByTheClientIsIgnored(t *testing.T) {
	h := newServer(t)
	body := manifest + `
status:
  state: Ready
  address: 203.0.113.1
  nodeName: n1
  ports: [{name: default, port: 7000}]
`
	rec := call(t, h, http.MethodPost, gameServersPath, body)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	gs := &agonesv1.GameServer{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), gs))
	assert.Equal(t, agonesv1.GameServerStatePortAllocation, gs.Status.State)
	assert.Empty(t, gs.Status.Address)
	assert.Empty(t, gs.Status.NodeName)
	assert.Empty(t, gs.Status.Ports)

	fleet := fleetManifest + "\nstatus: {replicas: 7, readyReplicas: 7}\n"
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, fleetPath, fleet).Code)
	assert.Equal(t, agonesv1.FleetStatus{}, getFleet(t, h).Status)
}

// Names end up in Docker's container and volume names, with "_" between
// the parts. A name that holds a "_" itself would read as someone else's.
func TestNamesMustBeDNSNames(t *testing.T) {
	for name, tc := range refusedForTheirNames(t) {
		t.Run(name, func(t *testing.T) {
			rec := call(t, newServer(t), tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.want+":")
		})
	}
}

func refusedForTheirNames(t testing.TB) map[string]refusal {
	return map[string]refusal{
		"an underscore in a name": {
			http.MethodPost, gameServersPath, replace(t, manifest, "name: simple", "name: gs_x"), "metadata.name",
		},
		"capitals in a name": {
			http.MethodPost, gameServersPath, replace(t, manifest, "name: simple", "name: Simple"), "metadata.name",
		},
		"a name from the URL": {
			http.MethodPut, "/v1/namespaces/default/fleets/a_b", fleetManifest, "metadata.name",
		},
		"an underscore in a generated name": {
			http.MethodPost, gameServersPath, replace(t, manifest, "name: simple", "generateName: gs_"), "metadata.generateName",
		},
		"an underscore before the dash a generated name may end in": {
			http.MethodPost, gameServersPath, replace(t, manifest, "name: simple", "generateName: gs_-"), "metadata.generateName",
		},
		"a generated name that is nothing but a dash": {
			http.MethodPost, gameServersPath, replace(t, manifest, "name: simple", "generateName: \"-\""), "metadata.generateName",
		},
		"an underscore in the namespace": {
			http.MethodPost, "/v1/namespaces/a_b/gameservers", manifest, "metadata.namespace",
		},
		"a dot in the namespace": {
			http.MethodPost, "/v1/namespaces/a.b/gameservers", manifest, "metadata.namespace",
		},
		"no name at all": {
			http.MethodPost, gameServersPath, replace(t, manifest, "  name: simple\n", "  labels: {a: b}\n"), "metadata.name",
		},
		"an autoscaler's name": {
			http.MethodPut, "/v1/namespaces/default/fleetautoscalers/a_b", scalerManifest, "metadata.name",
		},
	}
}

// The routes only match a namespace that is there. Admission does not
// count on that: a resource without one would be found by no list.
func TestAResourceWithoutANamespaceIsRefused(t *testing.T) {
	require.NoError(t, agones.Setup())
	api := New(nil, nil, nil, nil)

	_, ok := admitted(api.servers, "", []byte(manifest))
	assert.False(t, ok)
	_, ok = admitted(api.servers, "default", []byte(manifest))
	assert.True(t, ok)
}

func TestNamesThatKubernetesAcceptsAreAccepted(t *testing.T) {
	h := newServer(t)
	for _, name := range []string{"name: a", "name: my.game-1", "generateName: simple-", "generateName: simple", "generateName: s--"} {
		rec := call(t, h, http.MethodPost, "/v1/namespaces/team-1/gameservers", replace(t, manifest, "name: simple", name))
		assert.Equal(t, http.StatusCreated, rec.Code, "%s: %s", name, rec.Body.String())
	}
}

func TestApplyDoesNotReviveAFleetThatIsBeingDeleted(t *testing.T) {
	h := newServer(t)
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, fleetPath, fleetManifest).Code)
	require.Equal(t, http.StatusAccepted, call(t, h, http.MethodDelete, fleetPath, "").Code)
	marked := getFleet(t, h).DeletionTimestamp
	require.NotNil(t, marked)

	rec := call(t, h, http.MethodPut, fleetPath, replace(t, fleetManifest, "replicas: 2", "replicas: 3"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, marked, getFleet(t, h).DeletionTimestamp, "only its controller ends a deletion, by removing the Fleet")
}

func TestADeletionMarkSentByTheClientIsIgnored(t *testing.T) {
	h := newServer(t)
	body := replace(t, manifest, "name: simple", "name: simple\n  deletionTimestamp: \"2020-01-01T00:00:00Z\"\n  deletionGracePeriodSeconds: 0")

	rec := call(t, h, http.MethodPost, gameServersPath, body)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	gs := &agonesv1.GameServer{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), gs))
	assert.Nil(t, gs.DeletionTimestamp)
	assert.Nil(t, gs.DeletionGracePeriodSeconds)
}

// A Pod is checked as Kubernetes would check it, as far as it reaches
// Docker: upstream leaves that to the API server, which is not there.
func TestCreateRejectsAPodThatCouldNotRun(t *testing.T) {
	for name, tc := range refusedByThePod(t) {
		t.Run(name, func(t *testing.T) {
			rec := call(t, newServer(t), http.MethodPost, gameServersPath, tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.want+":")
		})
	}
}

// inPod is the GameServer manifest with more in its Pod, and inContainer
// with more in its container.
func inPod(t testing.TB, more string) string {
	return replace(t, manifest, "      nodeSelector:\n        pool: games\n", more)
}

func inContainer(t testing.TB, more string) string {
	return replace(t, manifest, "          image: example/game:1\n", "          image: example/game:1\n"+more)
}

func refusedByThePod(t testing.TB) map[string]refusal {
	pod := "spec.template.spec."
	post := func(body, want string) refusal { return refusal{http.MethodPost, gameServersPath, body, pod + want} }
	return map[string]refusal{
		"two containers of one name": post(
			gameIs(t, inContainer(t, "        - {name: game, image: example/logs:1}\n")), "containers[1].name"),
		"a container named like the sidecar": post(
			gameIs(t, inContainer(t, "        - {name: agones-gameserver-sidecar, image: example/logs:1}\n")), "containers[1].name"),
		"a volume that is nothing": post(
			inPod(t, "      volumes: [{name: data}]\n"), "volumes[0]"),
		"a mount of a volume that is nothing": post(
			replace(t, withMounts(t, "{name: data, mountPath: /data}"), "emptyDir: {}", ""), "volumes[0]"),
		"a host path that is no path": post(
			inPod(t, "      volumes: [{name: data, hostPath: {path: \"\"}}]\n"), "volumes[0].hostPath.path"),
		"a mount of a volume that is not there": post(
			inContainer(t, "          volumeMounts: [{name: data, mountPath: /data}]\n"), "containers[0].volumeMounts[0].name"),
		"more memory than Docker can be told": post(
			inContainer(t, "          resources: {limits: {memory: 1e19}}\n"), "containers[0].resources.limits[memory]"),
		"a negative limit": post(
			inContainer(t, "          resources: {limits: {cpu: -1}}\n"), "containers[0].resources.limits[cpu]"),
		"less memory than Docker gives a container": post(
			inContainer(t, "          resources: {limits: {memory: 1Mi}}\n"), "containers[0].resources.limits[memory]"),
		"a port of a container on the node, which only spec.ports publishes": post(
			inContainer(t, "          ports: [{containerPort: 7654, hostPort: 7654}]\n"), "containers[0].ports[0].hostPort"),
		"a port of a container that is not a port": post(
			inContainer(t, "          ports: [{containerPort: 70000}]\n"), "containers[0].ports[0].containerPort"),
		"more time to stop than Docker can be told": post(
			inPod(t, "      terminationGracePeriodSeconds: 9223372037\n"), "terminationGracePeriodSeconds"),
		"a working directory that does not start at the root": post(
			inContainer(t, "          workingDir: srv\n"), "containers[0].workingDir"),
		"a variable without a name": post(
			inContainer(t, "          env: [{value: ranked}]\n"), "containers[0].env[0].name"),
		"a variable with = in its name": post(
			inContainer(t, "          env: [{name: MODE=x, value: ranked}]\n"), "containers[0].env[0].name"),
		"a host path that does not start at the root": post(
			inPod(t, "      volumes: [{name: data, hostPath: {path: maps}}]\n"), "volumes[0].hostPath.path"),
		"a host path with a colon, which ends the path for Docker": post(
			inPod(t, "      volumes: [{name: data, hostPath: {path: \"/srv/a:b\"}}]\n"), "volumes[0].hostPath.path"),
		"a mount path that does not start at the root": post(
			withMounts(t, "{name: data, mountPath: data}"), "containers[0].volumeMounts[0].mountPath"),
		"a mount path with a colon": post(
			withMounts(t, "{name: data, mountPath: \"/a:ro\"}"), "containers[0].volumeMounts[0].mountPath"),
		"a mount on the root": post(
			withMounts(t, "{name: data, mountPath: /}"), "containers[0].volumeMounts[0].mountPath"),
		"two mounts on one path": post(
			withMounts(t, "{name: data, mountPath: /data}, {name: data, mountPath: /data}"), "containers[0].volumeMounts[1].mountPath"),
		"a user Linux has no number for": post(
			inContainer(t, "          securityContext: {runAsUser: -1}\n"), "containers[0].securityContext.runAsUser"),
		"a group Linux has no number for": post(
			inContainer(t, "          securityContext: {runAsUser: 0, runAsGroup: 4294967296}\n"), "containers[0].securityContext.runAsGroup"),
		"a group without a user, which Docker cannot set alone": post(
			inContainer(t, "          securityContext: {runAsGroup: 1000}\n"), "containers[0].securityContext.runAsUser"),
	}
}

// gameIs says which container of a manifest with several is the game.
func gameIs(t testing.TB, body string) string {
	return replace(t, body, "spec:\n", "spec:\n  container: game\n")
}

// withMounts is the GameServer manifest with a volume "data" and the
// given mounts of it.
func withMounts(t testing.TB, mounts string) string {
	body := inPod(t, "      volumes: [{name: data, emptyDir: {}}]\n")
	return replace(t, body, "          image: example/game:1\n", "          image: example/game:1\n          volumeMounts: ["+mounts+"]\n")
}

// What Docker has no word for, but can run without, is told to the user
// and dropped.
func TestVolumeSettingsDockerDoesNotHaveAreIgnoredWithAWarning(t *testing.T) {
	body := inPod(t, `      volumes:
        - {name: fast, emptyDir: {medium: Memory, sizeLimit: 1Gi}}
        - {name: maps, hostPath: {path: /srv/maps, type: Directory}}
`)

	rec := call(t, newServer(t), http.MethodPost, gameServersPath, body)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.ElementsMatch(t, []string{
		"spec.template.spec.volumes[0].emptyDir.medium is ignored",
		"spec.template.spec.volumes[0].emptyDir.sizeLimit is ignored",
		"spec.template.spec.volumes[1].hostPath.type is ignored",
	}, rec.Header().Values("Warning"))
}

// Upstream registers such a GameServer without running anything: the game
// is a process on the developer's machine. Here it would be run in a
// container like any other, and upstream's validation skips most of its
// checks for it.
func TestADevelopmentGameServerIsRefused(t *testing.T) {
	body := developmentGameServer(t)

	rec := call(t, newServer(t), http.MethodPost, gameServersPath, body)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "metadata.annotations[agones.dev/dev-address]:")
}

// developmentGameServer is a GameServer for local development, as
// upstream wants one, with a mistake upstream does not look for in one: it
// does not say which of its two containers is the game.
func developmentGameServer(t testing.TB) string {
	body := replace(t, manifest, "  name: simple\n", "  name: simple\n  annotations: {agones.dev/dev-address: 192.0.2.1}\n")
	body = replace(t, body, "containerPort: 7654\n", "containerPort: 7654\n      portPolicy: Static\n      hostPort: 7654\n")
	return replace(t, body, "          image: example/game:1\n", "          image: example/game:1\n        - {name: logs, image: example/logs:1}\n")
}

// "apply" sends what it got back the next time. Upstream's defaults add
// the finalizer each time they run, so the second time there were two.
func TestWhatAdmissionReturnsIsAdmittedUnchanged(t *testing.T) {
	require.NoError(t, agones.Setup())
	api := New(nil, nil, nil, nil)
	first, ok := admitted(api.servers, "default", []byte(manifest))
	require.True(t, ok)

	assert.Empty(t, changedByAdmission(api, first))
	assert.Equal(t, []string{agonesv1.FinalizerName}, first.Finalizers)
}
