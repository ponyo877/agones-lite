package apiserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	"github.com/ponyo877/scrim/pkg/agent"
	"github.com/ponyo877/scrim/pkg/agones"
	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/events"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/nodes"
	"github.com/ponyo877/scrim/pkg/podspec"
	"github.com/ponyo877/scrim/pkg/store"
)

// This test sends the management API requests nobody would write on
// purpose: any method, on any path, with any body, one after the other
// against the same server, so that later ones meet what earlier ones left.
//
// Whatever is asked, the answer is the caller's to act on: a status below
// 500. A 5xx says "the server is broken", and a healthy server, which this
// one is, must never say it. A panic in a handler fails the test as well.

// The routes of the API, and what stands in their places: the names of
// what is there, of what is not, and what is no name at all. What is
// likely to lead somewhere is listed more than once, to be drawn more often.
var (
	someRoutes = []string{
		"/healthz",
		"/v1/nodes",
		"/v1/nodes/NAME",
		"/v1/nodes/NAME/OPERATION",
		"/v1/PLURAL",
		"/v1/namespaces/NAMESPACE/PLURAL",
		"/v1/namespaces/NAMESPACE/PLURAL/NAME",
		"/v1/namespaces/NAMESPACE/fleets/NAME/scale",
		"/v1/namespaces/NAMESPACE/gameservers/NAME/logs",
		"/v1/namespaces/NAMESPACE/gameservers/NAME/logs",
		"/v1/namespaces/NAMESPACE/events",
		"/ANYTHING",
	}
	someNamespaces = []string{"default", "default", "default", "other", "a_b", ".."}
	somePlurals    = []string{"gameservers", "gameserversets", "fleets", "fleetautoscalers", "nodes", "events", "pods"}
	someNames      = []string{"running", "waiting", "simple", "scaler", "node-1", "nope", "a_b", "%00"}
	someOperations = []string{"cordon", "drain", "uncordon", "reboot"}
	someQueries    = []string{"", "", "?watch=true", "?kind=Fleet&name=simple", "?container=game", "?container=nope&follow=true&tail=x", "?%zz"}
	someMethods    = []string{http.MethodGet, http.MethodGet, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead}
)

func TestNoRequestMakesTheAPIAnswerWithAServerError(t *testing.T) {
	require.NoError(t, agones.Setup())
	seeds := seedManifests(t)
	dir := scratch(t)
	rapid.Check(t, func(t *rapid.T) {
		h := healthyAPI(t, filepath.Join(dir, fmt.Sprint(time.Now().UnixNano(), ".db")))
		for range rapid.IntRange(1, 30).Draw(t, "requests") {
			method, path, body := rapid.SampledFrom(someMethods).Draw(t, "method"), aPath(t), aBody(t, seeds)
			req, err := http.NewRequest(method, "http://api"+path, strings.NewReader(body))
			if err != nil {
				continue // net/http would not let it in either
			}
			// A watch goes on until its client leaves. This one has left.
			gone, leave := context.WithCancel(context.Background())
			leave()
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req.WithContext(gone))

			require.Less(t, rec.Code, 500, "%s %s\n%s\nanswered: %s", method, path, body, rec.Body)
		}
	})
}

// scratch is a directory for the stores of this test: in memory where the
// system has a place for that, as Linux does. Every one of the hundred
// servers the test starts would otherwise wait for a disk a few times.
func scratch(t *testing.T) string {
	dir, err := os.MkdirTemp("/dev/shm", "scrim-api-")
	if err != nil {
		return t.TempDir()
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// healthyAPI is the management API as the control plane wires it, over a
// store with a node whose agent is connected, a GameServer running on it
// and one still waiting for a node, as only controllers could have stored
// them.
func healthyAPI(t interface {
	require.TestingT
	Cleanup(func())
}, path string) http.Handler {
	s, err := store.Open(path, store.Kinds())
	require.NoError(t, err)
	recorder, err := events.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = recorder.Close()
		_ = s.Close()
	})
	servers := gameservers.NewController(s, recorder)
	local := nodes.NewLocal(s, servers)
	local.Attach("node-1", agentLogs{})
	_, err = store.NodesOf(s).Create(&scrimv1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Status: scrimv1.NodeStatus{Phase: scrimv1.NodeReady}})
	require.NoError(t, err)
	running := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "running"}}
	running.Spec.Container = "game"
	running.Spec.Template.Spec.Containers = []corev1.Container{{Name: "game", Image: "example/game:1"}}
	running.Status.State, running.Status.NodeName = agonesv1.GameServerStateReady, "node-1"
	_, err = store.GameServersOf(s).Create(running)
	require.NoError(t, err)
	waiting := running.DeepCopy()
	waiting.Name, waiting.Status = "waiting", agonesv1.GameServerStatus{State: agonesv1.GameServerStatePortAllocation}
	_, err = store.GameServersOf(s).Create(waiting)
	require.NoError(t, err)
	return New(s, recorder, nodes.NewController(s, servers, recorder, nodes.DefaultGraces), local).Handler()
}

// agentLogs answers for logs the way the agent of a node does: with the
// log of a container it runs, and an error for one it does not.
type agentLogs struct{}

func (agentLogs) Logs(_ context.Context, req agent.LogRequest) (io.ReadCloser, error) {
	if !slices.Contains([]string{"", "game", podspec.SidecarName}, req.Container) {
		return nil, fmt.Errorf("gameserver %s/%s has no such container on node node-1", req.Namespace, req.Name)
	}
	return io.NopCloser(strings.NewReader("ready\n")), nil
}

// aPath is a path of the API, a near miss of one, or no path of it at all.
func aPath(t *rapid.T) string {
	places := strings.NewReplacer(
		"NAMESPACE", rapid.SampledFrom(someNamespaces).Draw(t, "namespace"),
		"PLURAL", rapid.SampledFrom(somePlurals).Draw(t, "plural"),
		"NAME", rapid.SampledFrom(someNames).Draw(t, "name"),
		"OPERATION", rapid.SampledFrom(someOperations).Draw(t, "operation"),
		"ANYTHING", rapid.StringMatching(`[a-zA-Z0-9/._~%:@!$&'()*+,;=-]{0,40}`).Draw(t, "anything"),
	)
	route := rapid.SampledFrom(someRoutes).Draw(t, "route")
	return places.Replace(route) + rapid.SampledFrom(someQueries).Draw(t, "query")
}

// aBody is nothing, a size for a Fleet, anything at all, or one of the
// manifests the other tests know with something done to it.
func aBody(t *rapid.T, manifests []string) string {
	manifest := rapid.SampledFrom(manifests).Draw(t, "manifest")
	lines := strings.Split(manifest, "\n")
	at := rapid.IntRange(0, len(lines)-1).Draw(t, "line")
	bodies := []string{
		"",
		fmt.Sprintf(`{"replicas": %d}`, rapid.IntRange(-1, 3).Draw(t, "replicas")),
		rapid.String().Draw(t, "anything"),
		manifest,
		strings.Join(append(lines[:at:at], lines[at+1:]...), "\n"),             // a line is missing
		strings.Join(append(lines[:at+1:at+1], lines[at:]...), "\n"),           // a line is there twice
		manifest[:rapid.IntRange(0, len(manifest)).Draw(t, "cut")],             // it stops early
		strings.Replace(manifest, "simple", rapid.String().Draw(t, "name"), 1), // it is called something else
	}
	return rapid.SampledFrom(bodies).Draw(t, "body")
}

// What the property test found: asked for a log that cannot be there, the
// API passed the question on to a node and the node's answer back, as a
// fault of the server.
func TestALogThatCannotBeThereIsTheCallersToKnow(t *testing.T) {
	require.NoError(t, agones.Setup())
	h := healthyAPI(t, filepath.Join(t.TempDir(), "state.db"))
	tests := map[string]struct {
		path   string
		status int
	}{
		"of a running game server":              {"running/logs", http.StatusOK},
		"of its sidecar":                        {"running/logs?container=agones-gameserver-sidecar", http.StatusOK},
		"of a container it does not have":       {"running/logs?container=nope", http.StatusNotFound},
		"of a game server that is not there":    {"nope/logs", http.StatusNotFound},
		"of a game server not yet on any node":  {"waiting/logs", http.StatusConflict},
		"of a container of one not on any node": {"waiting/logs?container=nope", http.StatusNotFound},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := call(t, h, http.MethodGet, "/v1/namespaces/default/gameservers/"+tc.path, "")
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
		})
	}
}
