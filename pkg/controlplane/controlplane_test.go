package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "agones.dev/agones/pkg/allocation/go"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/nodes"
	"github.com/ponyo877/agones-lite/pkg/store"
)

const fleetManifest = `
apiVersion: agones.dev/v1
kind: Fleet
metadata:
  name: simple
spec:
  replicas: 3
  template:
    spec:
      ports:
        - name: default
          containerPort: 7654
      template:
        spec:
          containers:
            - name: game
              image: example/game:1
`

// world is a whole control plane with one simulated node. Nothing real
// runs: the node reports every game server as started and Ready at once.
type world struct {
	t     *testing.T
	cp    *ControlPlane
	api   http.Handler
	store *store.Store
}

func newWorld(t *testing.T) world {
	t.Helper()
	require.NoError(t, agones.Setup())
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	cp, err := New(s, nil, nil, nodes.DefaultGraces)
	require.NoError(t, err)
	node := &litev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: litev1.NodeStatus{
			Phase:     litev1.NodeReady,
			Address:   "203.0.113.10",
			PortRange: litev1.PortRange{Min: 7000, Max: 7100},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cp.Run(ctx)
	}()
	go func() { _ = agent.NewSimulator(node, cp.Agents).Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = s.Close()
	})
	return world{t: t, cp: cp, api: cp.API.Handler(), store: s}
}

func (w world) call(method, path, body string) *httptest.ResponseRecorder {
	w.t.Helper()
	rec := httptest.NewRecorder()
	w.api.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	require.Less(w.t, rec.Code, 300, "%s %s: %s", method, path, rec.Body.String())
	return rec
}

// states counts the Fleet's GameServers by state.
func (w world) states() map[agonesv1.GameServerState]int {
	counts := map[agonesv1.GameServerState]int{}
	for _, gs := range store.GameServersOf(w.store).List("default") {
		counts[gs.Status.State]++
	}
	return counts
}

func (w world) fleet() *agonesv1.Fleet {
	w.t.Helper()
	fleet := &agonesv1.Fleet{}
	require.NoError(w.t, json.Unmarshal(w.call(http.MethodGet, "/v1/namespaces/default/fleets/simple", "").Body.Bytes(), fleet))
	return fleet
}

func (w world) soon(condition func() bool, message string) {
	w.t.Helper()
	require.Eventually(w.t, condition, 30*time.Second, 20*time.Millisecond, message)
}

func (w world) allocate() (*pb.AllocationResponse, error) {
	return w.cp.Allocation.Allocate(context.Background(), &pb.AllocationRequest{
		Namespace: "default",
		GameServerSelectors: []*pb.GameServerSelector{
			{MatchLabels: map[string]string{agonesv1.FleetNameLabel: "simple"}},
		},
	})
}

func TestAFleetFromApplyToAllocationToDeletion(t *testing.T) {
	w := newWorld(t)
	ready, allocated := agonesv1.GameServerStateReady, agonesv1.GameServerStateAllocated

	// Applying the Fleet brings up three Ready game servers.
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest)
	w.soon(func() bool { return w.states()[ready] == 3 }, "three game servers become Ready")
	w.soon(func() bool { return w.fleet().Status.ReadyReplicas == 3 }, "the Fleet reports them")

	// A matchmaker takes one.
	got, err := w.allocate()
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.10", got.GetAddress())
	require.Len(t, got.GetPorts(), 1)
	assert.GreaterOrEqual(t, got.GetPorts()[0].GetPort(), int32(7000))
	w.soon(func() bool {
		status := w.fleet().Status
		return status.AllocatedReplicas == 1 && status.ReadyReplicas == 2
	}, "the Fleet reports one allocated and two ready")

	// Scaling down removes idle servers and never the one in use.
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple/scale", `{"replicas": 1}`)
	w.soon(func() bool {
		states := w.states()
		return states[allocated] == 1 && len(states) == 1
	}, "only the allocated game server is left")

	// A new template rolls out next to the session that is still running.
	updated := strings.Replace(fleetManifest, "example/game:1", "example/game:2", 1)
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", updated)
	w.soon(func() bool {
		states := w.states()
		return states[allocated] == 1 && states[ready] == 2 && w.imagesOfReady() == "example/game:2"
	}, "two new game servers run the new image; the old session goes on")

	// With nothing Ready left to give, the answer is "none available".
	for range 2 {
		_, err = w.allocate()
		require.NoError(t, err)
	}
	_, err = w.allocate()
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))

	// Deleting the Fleet takes everything with it.
	w.call(http.MethodDelete, "/v1/namespaces/default/fleets/simple", "")
	w.soon(func() bool {
		return len(store.GameServersOf(w.store).List("")) == 0 &&
			len(store.GameServerSetsOf(w.store).List("")) == 0 &&
			len(store.FleetsOf(w.store).List("")) == 0
	}, "the Fleet, its sets and its game servers are gone")
}

// imagesOfReady returns the image all Ready game servers run, or "mixed".
func (w world) imagesOfReady() string {
	image := ""
	for _, gs := range store.GameServersOf(w.store).List("default") {
		if gs.Status.State != agonesv1.GameServerStateReady {
			continue
		}
		current := gs.Spec.Template.Spec.Containers[0].Image
		if image != "" && image != current {
			return "mixed"
		}
		image = current
	}
	return image
}

func TestAllocationSkipsACordonedNode(t *testing.T) {
	w := newWorld(t)
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest)
	w.soon(func() bool { return w.states()[agonesv1.GameServerStateReady] == 3 }, "three game servers become Ready")

	w.call(http.MethodPost, "/v1/nodes/node-1/cordon", "")
	_, err := w.allocate()
	assert.Equal(t, codes.ResourceExhausted, status.Code(err), "players are not sent to a node that is being taken out of service")

	w.call(http.MethodPost, "/v1/nodes/node-1/uncordon", "")
	_, err = w.allocate()
	assert.NoError(t, err)
}

func TestDrainingANodeEmptiesItExceptForSessions(t *testing.T) {
	w := newWorld(t)
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest)
	w.soon(func() bool { return w.states()[agonesv1.GameServerStateReady] == 3 }, "three game servers become Ready")
	_, err := w.allocate()
	require.NoError(t, err)

	w.call(http.MethodPost, "/v1/nodes/node-1/drain", "")

	// The only node is draining, so replacements have nowhere to go and wait.
	w.soon(func() bool {
		states := w.states()
		return states[agonesv1.GameServerStateAllocated] == 1 && states[agonesv1.GameServerStateReady] == 0
	}, "idle game servers leave; the session stays")
	// The replacements are the ones still on their way up. Servers that are
	// shutting down may linger for a moment and are not of interest here.
	for _, gs := range store.GameServersOf(w.store).List("default") {
		if gs.Status.State == agonesv1.GameServerStatePortAllocation || gs.Status.State == agonesv1.GameServerStateCreating {
			assert.Empty(t, gs.Status.NodeName, "%s must not be placed on a draining node", gs.Name)
		}
	}
}

const autoscalerManifest = `
apiVersion: autoscaling.agones.dev/v1
kind: FleetAutoscaler
metadata:
  name: simple
spec:
  fleetName: simple
  policy:
    type: Buffer
    buffer:
      bufferSize: 2
      minReplicas: 2
      maxReplicas: 5
  sync:
    type: FixedInterval
    fixedInterval:
      seconds: 1
`

func (w world) autoscaler() *autoscalingv1.FleetAutoscaler {
	w.t.Helper()
	fas := &autoscalingv1.FleetAutoscaler{}
	body := w.call(http.MethodGet, "/v1/namespaces/default/fleetautoscalers/simple", "").Body.Bytes()
	require.NoError(w.t, json.Unmarshal(body, fas))
	return fas
}

func TestTheAutoscalerKeepsABufferOfReadyGameServers(t *testing.T) {
	w := newWorld(t)
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest)
	w.call(http.MethodPut, "/v1/namespaces/default/fleetautoscalers/simple", autoscalerManifest)

	// Nobody is playing: the three replicas of the manifest shrink to the buffer.
	w.soon(func() bool {
		fleet := w.fleet()
		return fleet.Spec.Replicas == 2 && fleet.Status.ReadyReplicas == 2 && fleet.Status.Replicas == 2
	}, "the Fleet shrinks to its buffer of two")

	// Every allocation is answered with one more game server.
	for range 2 {
		_, err := w.allocate()
		require.NoError(t, err)
	}
	w.soon(func() bool {
		fleet := w.fleet()
		return fleet.Spec.Replicas == 4 && fleet.Status.ReadyReplicas == 2
	}, "two in use, two more standing by")
	assert.False(t, w.autoscaler().Status.ScalingLimited)

	// The maximum is the limit, and the status says when it bites.
	for range 2 {
		_, err := w.allocate()
		require.NoError(t, err)
	}
	w.soon(func() bool {
		return w.fleet().Spec.Replicas == 5 && w.autoscaler().Status.ScalingLimited
	}, "four in use would want six, but five is the maximum")
	status := w.autoscaler().Status
	assert.True(t, status.AbleToScale)
	assert.Equal(t, int32(5), status.DesiredReplicas)
}

func TestTheAutoscalerAsksAWebhook(t *testing.T) {
	asked := make(chan *autoscalingv1.FleetAutoscaleReview, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		review := &autoscalingv1.FleetAutoscaleReview{}
		_ = json.NewDecoder(r.Body).Decode(review)
		asked <- review
		review.Response = &autoscalingv1.FleetAutoscaleResponse{UID: review.Request.UID, Scale: true, Replicas: 4}
		_ = json.NewEncoder(rw).Encode(review)
	}))
	defer webhook.Close()

	w := newWorld(t)
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest)
	manifest := strings.Replace(autoscalerManifest, `    type: Buffer
    buffer:
      bufferSize: 2
      minReplicas: 2
      maxReplicas: 5`, `    type: Webhook
    webhook:
      url: `+webhook.URL, 1)
	w.call(http.MethodPut, "/v1/namespaces/default/fleetautoscalers/simple", manifest)

	w.soon(func() bool { return w.fleet().Spec.Replicas == 4 }, "the Fleet takes the size the webhook answered")
	review := <-asked
	assert.Equal(t, "simple", review.Request.Name)
	assert.NotEmpty(t, review.Request.UID)
}

func TestAnAutoscalerWithoutItsFleetSaysSo(t *testing.T) {
	w := newWorld(t)
	w.call(http.MethodPut, "/v1/namespaces/default/fleetautoscalers/simple", autoscalerManifest)

	// It keeps looking, so the Fleet may come later.
	time.Sleep(1500 * time.Millisecond)
	assert.False(t, w.autoscaler().Status.AbleToScale)

	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest)
	w.soon(func() bool { return w.autoscaler().Status.AbleToScale }, "the autoscaler finds the Fleet once it exists")
}
