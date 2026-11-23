package apiserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/agones-lite/pkg/agones"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
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
	return New(s, gameservers.NewController(s)).Handler()
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
	tests := map[string]struct {
		body string
		want string
	}{
		"a typo in a field name":       {strings.Replace(manifest, "containerPort", "containerPrt", 1), "containerPrt"},
		"upstream validation":          {strings.Replace(manifest, "containerPort: 7654", "containerPort: 7654\n      hostPort: 7000", 1), "hostPort"},
		"a field Docker cannot honour": {strings.Replace(manifest, "nodeSelector:\n        pool: games", "hostNetwork: true", 1), "hostNetwork"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := call(t, h, http.MethodPost, "/v1/namespaces/default/gameservers", tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.want)
		})
	}
}

func TestGetListAndDelete(t *testing.T) {
	h := newServer(t)
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPost, "/v1/namespaces/default/gameservers", manifest).Code)

	assert.Equal(t, http.StatusOK, call(t, h, http.MethodGet, "/v1/namespaces/default/gameservers/simple", "").Code)
	assert.Equal(t, http.StatusNotFound, call(t, h, http.MethodGet, "/v1/namespaces/default/gameservers/nope", "").Code)

	list := agonesv1.GameServerList{}
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
func TestAcceptsTheUpstreamSamples(t *testing.T) {
	for _, sample := range []string{"examples/simple-game-server/gameserver.yaml", "examples/gameserver.yaml"} {
		t.Run(sample, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("../../third_party/agones", sample))
			if err != nil {
				t.Skip("the Agones submodule is not checked out")
			}
			rec := call(t, newServer(t), http.MethodPost, "/v1/namespaces/default/gameservers", string(body))
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		})
	}
}
