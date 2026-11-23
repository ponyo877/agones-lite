package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rest is a game server that talks to the SDK server with plain HTTP, as
// the SDKs for languages without gRPC do.
type rest struct {
	t    *testing.T
	base string // host and port, without a scheme
}

// call sends one request and returns the status and the decoded answer.
// int64 fields come back as strings: that is how JSON carries them.
func (r rest) call(method, path, body string) (int, map[string]any) {
	r.t.Helper()
	req, err := http.NewRequest(method, "http://"+r.base+path, strings.NewReader(body))
	require.NoError(r.t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil // the SDK server is still starting
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(r.t, err)
	answer := map[string]any{}
	require.NoError(r.t, json.Unmarshal(raw, &answer), "%s %s: %s", method, path, raw)
	return resp.StatusCode, answer
}

// ok is call for a request that must succeed.
func (r rest) ok(method, path, body string) map[string]any {
	r.t.Helper()
	code, answer := r.call(method, path, body)
	require.Equal(r.t, http.StatusOK, code, "%s %s: %v", method, path, answer)
	return answer
}

// beat reports health four times a second until the test ends.
func (r rest) beat() {
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			_, _ = http.Post("http://"+r.base+"/health", "application/json", strings.NewReader("{}"))
			time.Sleep(250 * time.Millisecond)
		}
	}()
}

func TestAGameServerLivesItsWholeLifeThroughREST(t *testing.T) {
	b := newBench(t)
	_, httpPort := b.create("rest")
	game := rest{t: t, base: fmt.Sprintf("localhost:%d", httpPort)}
	b.soon(func() bool {
		code, _ := game.call(http.MethodGet, "/gameserver", "")
		return code == http.StatusOK
	}, "the SDK server answers")
	game.beat()

	// Ready, and what the game server learns about itself. Field names are
	// the ones of the proto file, and unset fields are present.
	assert.Empty(t, game.ok(http.MethodPost, "/ready", "{}"))
	b.becomes("rest", agonesv1.GameServerStateReady)
	var self map[string]any
	b.soon(func() bool {
		self = game.ok(http.MethodGet, "/gameserver", "")
		return self["status"].(map[string]any)["state"] == "Ready"
	}, "the game server sees itself Ready")
	meta := self["object_meta"].(map[string]any)
	assert.Equal(t, "rest", meta["name"])
	assert.Equal(t, "0", meta["deletion_timestamp"])
	assert.Equal(t, "203.0.113.10", self["status"].(map[string]any)["address"])

	game.ok(http.MethodPut, "/metadata/label", `{"key": "mode", "value": "ranked"}`)
	game.ok(http.MethodPut, "/metadata/annotation", `{"key": "map", "value": "dust"}`)
	b.soon(func() bool {
		gs := b.stored("rest")
		return gs.Labels["agones.dev/sdk-mode"] == "ranked" && gs.Annotations["agones.dev/sdk-map"] == "dust"
	}, "the label and the annotation arrive")

	game.ok(http.MethodPost, "/reserve", `{"seconds": "30"}`)
	b.becomes("rest", agonesv1.GameServerStateReserved)
	game.ok(http.MethodPost, "/allocate", "{}")
	b.becomes("rest", agonesv1.GameServerStateAllocated)

	// A Counter.
	rooms := map[string]any{"name": "rooms", "count": "1", "capacity": "10"}
	assert.Equal(t, rooms, game.ok(http.MethodGet, "/v1beta1/counters/rooms", ""))
	rooms["count"] = "3"
	assert.Equal(t, rooms, game.ok(http.MethodPatch, "/v1beta1/counters/rooms", `{"countDiff": "2"}`))
	code, refusal := game.call(http.MethodPatch, "/v1beta1/counters/rooms", `{"countDiff": "8"}`)
	assert.Equal(t, http.StatusInternalServerError, code, "eleven rooms do not fit into ten")
	assert.Contains(t, refusal["message"], "out of range")

	// A List. A PATCH changes the fields its body names.
	players := map[string]any{"name": "players", "capacity": "4", "values": []any{"ada"}}
	assert.Equal(t, players, game.ok(http.MethodGet, "/v1beta1/lists/players", ""))
	players["values"] = []any{"ada", "bob"}
	assert.Equal(t, players, game.ok(http.MethodPost, "/v1beta1/lists/players:addValue", `{"value": "bob"}`))
	players["values"] = []any{"bob"}
	assert.Equal(t, players, game.ok(http.MethodPost, "/v1beta1/lists/players:removeValue", `{"value": "ada"}`))
	game.ok(http.MethodPatch, "/v1beta1/lists/players", `{"capacity": "8"}`)
	players["capacity"] = "8"
	assert.Equal(t, players, game.ok(http.MethodGet, "/v1beta1/lists/players", ""))
	b.soon(func() bool {
		status := b.stored("rest").Status
		return status.Counters["rooms"].Count == 3 && status.Lists["players"].Capacity == 8 &&
			assert.ObjectsAreEqual([]string{"bob"}, status.Lists["players"].Values)
	}, "the control plane has the same numbers")

	// The end.
	game.ok(http.MethodPost, "/shutdown", "{}")
	b.soon(func() bool { return b.stored("rest") == nil }, "the GameServer is gone")
}

// watched reads one update of a watch and returns the state it shows.
// Every update is wrapped in "result", the way grpc-gateway sends a stream.
func watched(t *testing.T, raw []byte) string {
	t.Helper()
	update := struct {
		Result struct {
			Status struct{ State string }
		}
	}{}
	require.NoError(t, json.Unmarshal(raw, &update), "%s", raw)
	return update.Result.Status.State
}

func TestWatchingTheGameServerOverHTTPAndWebSocket(t *testing.T) {
	b := newBench(t)
	_, httpPort := b.create("watched")
	game := rest{t: t, base: fmt.Sprintf("localhost:%d", httpPort)}
	b.soon(func() bool {
		code, _ := game.call(http.MethodGet, "/gameserver", "")
		return code == http.StatusOK
	}, "the SDK server answers")
	game.beat()

	// Two watchers: one reads a long HTTP answer line by line, the other
	// has a WebSocket. Both are shown the state of now first.
	stream, err := http.Get("http://" + game.base + "/watch/gameserver")
	require.NoError(t, err)
	defer func() { _ = stream.Body.Close() }()
	lines := bufio.NewReader(stream.Body)
	socket, _, err := websocket.DefaultDialer.Dial("ws://"+game.base+"/watch/gameserver", nil)
	require.NoError(t, err)
	defer func() { _ = socket.Close() }()

	overHTTP := func() string {
		line, err := lines.ReadBytes('\n')
		require.NoError(t, err)
		return watched(t, line)
	}
	overSocket := func() string {
		_, message, err := socket.ReadMessage()
		require.NoError(t, err)
		return watched(t, message)
	}
	assert.Equal(t, "Scheduled", overHTTP())
	assert.Equal(t, "Scheduled", overSocket())

	// Then every change, until the one they are waiting for.
	game.ok(http.MethodPost, "/ready", "{}")
	for _, next := range []func() string{overHTTP, overSocket} {
		b.soon(func() bool { return next() == "Ready" }, "the watcher is shown Ready")
	}
}
