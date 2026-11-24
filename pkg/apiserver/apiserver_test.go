package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/agones"
	"github.com/ponyo877/agones-lite/pkg/store"
	"github.com/ponyo877/agones-lite/test/testenv"
)

const manifest = `
apiVersion: agones.dev/v1
kind: GameServer
metadata:
  name: simple
spec:
  ports:
    - name: default
      containerPort: 7654
  template:
    spec:
      nodeSelector:
        pool: games
      containers:
        - name: game
          image: example/game:1
`

func newServer(t *testing.T) http.Handler {
	t.Helper()
	require.NoError(t, agones.Setup())
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return New(s, nil, nil, nil).Handler()
}

func call(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestCreateAppliesDefaultsAndWarns(t *testing.T) {
	h := newServer(t)

	rec := call(t, h, http.MethodPost, "/v1/namespaces/default/gameservers", manifest)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"spec.template.spec.nodeSelector is ignored"}, rec.Header().Values("Warning"))
	gs := &agonesv1.GameServer{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), gs))
	assert.Equal(t, "default", gs.Namespace)
	assert.Equal(t, agonesv1.GameServerStatePortAllocation, gs.Status.State)
	assert.Equal(t, agonesv1.Dynamic, gs.Spec.Ports[0].PortPolicy, "upstream's default port policy")
	assert.Equal(t, "game", gs.Spec.Container)

	again := call(t, h, http.MethodPost, "/v1/namespaces/default/gameservers", manifest)
	assert.Equal(t, http.StatusConflict, again.Code)
}

func TestCreateRejectsWhatCannotRun(t *testing.T) {
	h := newServer(t)
	for name, tc := range whatCannotRun() {
		t.Run(name, func(t *testing.T) {
			rec := call(t, h, http.MethodPost, "/v1/namespaces/default/gameservers", tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.want)
		})
	}
}

// whatCannotRun are GameServers that must be refused, each with a word the
// refusal must hold.
func whatCannotRun() map[string]struct{ body, want string } {
	return map[string]struct{ body, want string }{
		"a typo in a field name":       {strings.Replace(manifest, "containerPort", "containerPrt", 1), "containerPrt"},
		"upstream validation":          {strings.Replace(manifest, "containerPort: 7654", "containerPort: 7654\n      hostPort: 7000", 1), "hostPort"},
		"a field Docker cannot honour": {strings.Replace(manifest, "nodeSelector:\n        pool: games", "hostNetwork: true", 1), "hostNetwork"},
		"an image Docker cannot read":  {strings.Replace(manifest, "example/game:1", `"example/game:"`, 1), "containers[0].image"},
		"a Pod Kubernetes would refuse": {
			strings.Replace(manifest, "nodeSelector:\n        pool: games", "terminationGracePeriodSeconds: -1", 1),
			"terminationGracePeriodSeconds",
		},
	}
}

func TestGetListAndDelete(t *testing.T) {
	h := newServer(t)
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPost, "/v1/namespaces/default/gameservers", manifest).Code)

	assert.Equal(t, http.StatusOK, call(t, h, http.MethodGet, "/v1/namespaces/default/gameservers/simple", "").Code)
	assert.Equal(t, http.StatusNotFound, call(t, h, http.MethodGet, "/v1/namespaces/default/gameservers/nope", "").Code)

	var list struct {
		Items []agonesv1.GameServer `json:"items"`
	}
	rec := call(t, h, http.MethodGet, "/v1/namespaces/default/gameservers", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list.Items, 1)

	assert.Equal(t, http.StatusAccepted, call(t, h, http.MethodDelete, "/v1/namespaces/default/gameservers/simple", "").Code)
	assert.Equal(t, http.StatusNotFound, call(t, h, http.MethodDelete, "/v1/namespaces/default/gameservers/nope", "").Code)

	rec = call(t, h, http.MethodGet, "/v1/namespaces/default/gameservers/simple", "")
	gs := &agonesv1.GameServer{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), gs))
	assert.NotNil(t, gs.DeletionTimestamp, "the record stays until the controller has cleaned up")
}

// The upstream samples are the compatibility promise, so they must be accepted as they are.
// Every GameServer sample in the Agones repository must be accepted as it
// is: that is the promise to people who come from Agones.
func TestAcceptsTheUpstreamSamples(t *testing.T) {
	samples, _ := filepath.Glob("../../third_party/agones/examples/*/gameserver*.yaml")
	root, _ := filepath.Glob("../../third_party/agones/examples/gameserver.yaml")
	samples = append(samples, root...)
	if len(samples) == 0 {
		testenv.Missing(t, "the Agones submodule is not checked out")
	}
	for _, sample := range samples {
		if strings.Contains(sample, "allocation") {
			continue // a request to the allocator, not a GameServer
		}
		t.Run(strings.TrimPrefix(sample, "../../third_party/agones/"), func(t *testing.T) {
			body, err := os.ReadFile(sample)
			require.NoError(t, err)
			rec := call(t, newServer(t), http.MethodPost, "/v1/namespaces/default/gameservers", string(body))
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		})
	}
}

const fleetManifest = `
apiVersion: agones.dev/v1
kind: Fleet
metadata:
  name: simple
spec:
  replicas: 2
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

func getFleet(t *testing.T, h http.Handler) *agonesv1.Fleet {
	t.Helper()
	rec := call(t, h, http.MethodGet, "/v1/namespaces/default/fleets/simple", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fleet := &agonesv1.Fleet{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), fleet))
	return fleet
}

func TestApplyingAFleetTwiceUpdatesIt(t *testing.T) {
	h := newServer(t)

	first := call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	created := getFleet(t, h)
	assert.Equal(t, int64(1), created.Generation)
	assert.Equal(t, "25%", created.Spec.Strategy.RollingUpdate.MaxSurge.String(), "upstream's default")

	same := call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest)
	require.Equal(t, http.StatusOK, same.Code, same.Body.String())
	assert.Equal(t, int64(1), getFleet(t, h).Generation, "nothing changed, so no new generation")

	changed := strings.Replace(fleetManifest, "example/game:1", "example/game:2", 1)
	require.Equal(t, http.StatusOK, call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/simple", changed).Code)
	updated := getFleet(t, h)
	assert.Equal(t, int64(2), updated.Generation)
	assert.Equal(t, created.UID, updated.UID)
	assert.Equal(t, "example/game:2", updated.Spec.Template.Spec.Template.Spec.Containers[0].Image)
}

func TestScaleChangesOnlyTheSize(t *testing.T) {
	h := newServer(t)
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/simple", fleetManifest).Code)

	rec := call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/simple/scale", `{"replicas": 5}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int32(5), getFleet(t, h).Spec.Replicas)

	assert.Equal(t, http.StatusUnprocessableEntity, call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/simple/scale", `{"replicas": -1}`).Code)
	assert.Equal(t, http.StatusNotFound, call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/nope/scale", `{"replicas": 1}`).Code)
}

func TestAFleetWithAnUnsupportedPodFieldIsRejected(t *testing.T) {
	h := newServer(t)
	bad := strings.Replace(fleetManifest, "containers:", "hostNetwork: true\n          containers:", 1)

	rec := call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/simple", bad)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "spec.template.spec.template.spec.hostNetwork")
}

func TestGameServerSetsAreReadOnly(t *testing.T) {
	h := newServer(t)

	assert.Equal(t, http.StatusOK, call(t, h, http.MethodGet, "/v1/namespaces/default/gameserversets", "").Code)
	assert.Equal(t, http.StatusMethodNotAllowed, call(t, h, http.MethodPost, "/v1/namespaces/default/gameserversets", "{}").Code)
}

func TestAcceptsTheUpstreamFleetSamples(t *testing.T) {
	for _, sample := range []string{"examples/simple-game-server/fleet.yaml", "examples/fleet.yaml"} {
		t.Run(sample, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("../../third_party/agones", sample))
			if err != nil {
				testenv.Missing(t, "the Agones submodule is not checked out")
			}
			var head struct {
				Metadata struct{ Name string } `json:"metadata"`
			}
			require.NoError(t, yaml.Unmarshal(body, &head))
			rec := call(t, newServer(t), http.MethodPut, "/v1/namespaces/default/fleets/"+head.Metadata.Name, string(body))
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		})
	}
}

func TestAcceptsTheUpstreamFleetAutoscalerSamples(t *testing.T) {
	samples := []string{
		"examples/fleetautoscaler.yaml", "examples/counterfleetautoscaler.yaml", "examples/listfleetautoscaler.yaml",
		"examples/schedulefleetautoscaler.yaml", "examples/chainfleetautoscaler.yaml",
	}
	for _, sample := range samples {
		t.Run(sample, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("../../third_party/agones", sample))
			if err != nil {
				testenv.Missing(t, "the Agones submodule is not checked out")
			}
			var head struct {
				Metadata struct{ Name string } `json:"metadata"`
			}
			require.NoError(t, yaml.Unmarshal(body, &head))
			rec := call(t, newServer(t), http.MethodPut, "/v1/namespaces/default/fleetautoscalers/"+head.Metadata.Name, string(body))
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		})
	}
}

// A node that cannot hand over a log yet, or any more, is not a fault of
// the server, and must not be answered as one.
func TestALogThatIsNotThereToReadIsAConflictAndNotAServerError(t *testing.T) {
	notConnected := fmt.Errorf("gameserver default/gs: its node %q is not connected: %w", "node-1", agent.ErrNoLog)

	assert.Equal(t, http.StatusConflict, statusOf(notConnected))
	assert.Equal(t, http.StatusInternalServerError, statusOf(errors.New("docker: the daemon went away")))
}
