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
func replace(t *testing.T, manifest, old, changed string) string {
	t.Helper()
	require.Contains(t, manifest, old)
	return strings.Replace(manifest, old, changed, 1)
}

// A Fleet's template metadata is copied to every GameServerSet and
// GameServer. An owner or a deletion mark in it would make the children
// look like someone else's, or like they are already going away.
func TestTemplateMetadataKeepsOnlyLabelsAndAnnotations(t *testing.T) {
	h := newServer(t)
	body := replace(t, fleetManifest, "  template:\n    spec:", `  template:
    metadata:
      labels: {tier: gold}
      annotations: {note: kept}
      deletionTimestamp: "2020-01-01T00:00:00Z"
      finalizers: [example.com/hold]
      ownerReferences:
        - {apiVersion: v1, kind: Pod, name: other, uid: x, controller: true}
    spec:`)

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

func TestPodTemplateMetadataKeepsOnlyLabelsAndAnnotations(t *testing.T) {
	h := newServer(t)
	body := replace(t, manifest, "  template:\n    spec:", `  template:
    metadata:
      labels: {tier: gold}
      ownerReferences:
        - {apiVersion: v1, kind: Pod, name: other, uid: x, controller: true}
    spec:`)

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

// Upstream's Validate relies on the CRD schema having run first. Nothing
// runs it here, so its limits are checked by hand.
func TestCreateRejectsWhatTheCRDSchemaWould(t *testing.T) {
	schedule := `policy:
    type: Schedule
    schedule:
      activePeriod: {startCron: "0 1 * * 0", duration: 5h}
      policy:
        type: Buffer
        buffer: {bufferSize: 2, maxReplicas: 10}
`
	tests := map[string]struct {
		method, path, body string
		want               string
	}{
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
		"an unknown way to sync": {
			http.MethodPut, scalerPath,
			replace(t, scalerManifest, "spec:\n", "spec:\n  sync: {type: Sometimes}\n"),
			"spec.sync.type",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := call(t, newServer(t), tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.want+":")
		})
	}
}

// Kubernetes refuses a Pod that asks for one host port twice, and Docker a
// container. Two Static ports may share a number only on different protocols.
func TestStaticPortsOfOneGameServerMustNotClash(t *testing.T) {
	static := func(name string, hostPort int, protocol string) string {
		return fmt.Sprintf("    - {name: %s, portPolicy: Static, containerPort: 7654, hostPort: %d%s}\n", name, hostPort, protocol)
	}
	tests := map[string]struct {
		ports string
		clash string // the field that is refused; empty when the ports are fine
	}{
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
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			body := replace(t, manifest, "    - name: default\n      containerPort: 7654\n", tc.ports)

			rec := call(t, newServer(t), http.MethodPost, gameServersPath, body)

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
	tests := map[string]struct {
		method, path, body string
		want               string
	}{
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
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := call(t, newServer(t), tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.want+":")
		})
	}
}

func TestNamesThatKubernetesAcceptsAreAccepted(t *testing.T) {
	h := newServer(t)
	for _, name := range []string{"name: a", "name: my.game-1", "generateName: simple-"} {
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
