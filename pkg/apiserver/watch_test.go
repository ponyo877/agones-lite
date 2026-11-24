package apiserver

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/store"
)

// watcher reads a watch the way a client does: over a real connection,
// one event at a time.
type watcher struct {
	t     *testing.T
	lines *bufio.Scanner
}

func watch(t *testing.T, h http.Handler, path string) watcher {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	resp, err := http.Get(server.URL + path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	return watcher{t: t, lines: bufio.NewScanner(resp.Body)}
}

// next returns the name of the next event and the name of its object.
func (w watcher) next() (event, object string) {
	w.t.Helper()
	for w.lines.Scan() {
		field, value, _ := strings.Cut(w.lines.Text(), ": ")
		switch field {
		case "event":
			event = value
		case "data":
			_, rest, _ := strings.Cut(value, `"name":"`)
			object, _, _ = strings.Cut(rest, `"`)
			return event, object
		}
	}
	require.Fail(w.t, "the watch ended", w.lines.Err())
	return "", ""
}

func TestAWatchIsAListThatGoesOn(t *testing.T) {
	h := newServer(t)
	fleet := strings.Replace(fleetManifest, "name: simple", "name: before", 1)
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/before", fleet).Code)
	w := watch(t, h, "/v1/namespaces/default/fleets?watch=true")

	// First what there is, then the sign that this was all of it.
	event, name := w.next()
	assert.Equal(t, []string{"Added", "before"}, []string{event, name})
	event, _ = w.next()
	assert.Equal(t, Synced, event)

	// From then on, every change as it happens.
	fleet = strings.Replace(fleetManifest, "name: simple", "name: after", 1)
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/after", fleet).Code)
	event, name = w.next()
	assert.Equal(t, []string{"Added", "after"}, []string{event, name})

	call(t, h, http.MethodPut, "/v1/namespaces/default/fleets/after/scale", `{"replicas": 5}`)
	event, name = w.next()
	assert.Equal(t, []string{"Modified", "after"}, []string{event, name})

	// A change somewhere else is none of this watcher's business.
	other := strings.Replace(fleetManifest, "name: simple", "name: elsewhere", 1)
	require.Equal(t, http.StatusCreated, call(t, h, http.MethodPut, "/v1/namespaces/other/fleets/elsewhere", other).Code)
	call(t, h, http.MethodDelete, "/v1/namespaces/default/fleets/before", "")
	event, name = w.next()
	assert.Equal(t, "before", name)
	assert.Contains(t, []string{"Modified", "Deleted"}, event, "marked for deletion, or gone already")
}

// The store closes the channel of a watcher that fell too far behind. The
// stream must then end, so that the client lists and watches again instead
// of trusting a copy with a hole in it.
func TestAWatchEndsWhenTheStoreGivesUpOnIt(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := newEventStream(rec)
	changes := make(chan store.Event, 1)
	last := &agonesv1.Fleet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "last"}}
	changes <- store.Event{Type: store.Modified, Key: store.Key{Namespace: "default", Name: "last"}, Object: last}
	close(changes)

	ended := make(chan struct{})
	go func() {
		defer close(ended)
		stream.relay(context.Background(), changes, "")
	}()

	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		require.Fail(t, "the stream went on after the watch was closed")
	}
	assert.Equal(t, 1, strings.Count(rec.Body.String(), "event: "), "what was told before the close is still sent, and nothing after")
	assert.Contains(t, rec.Body.String(), "event: Modified\ndata: {\"metadata\":{\"name\":\"last\"")
}
