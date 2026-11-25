package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ponyo877/scrim/pkg/store"
)

// Synced is the event that ends the first part of a watch: everything that
// existed has been sent, and what follows are changes.
const Synced = "Synced"

// watch is a list that never ends: every object as it is now, then Synced,
// then every change. The answer is a stream of Server-Sent Events, each
// named after what happened (Added, Modified, Deleted) and carrying the
// object as JSON.
//
// The store gives up on a watcher that falls too far behind, and the stream
// then simply ends. A client that still wants to know watches again, which
// starts with the full list: it never works from a copy with a hole in it.
func (r resource[T]) watch(w http.ResponseWriter, req *http.Request) {
	namespace := req.PathValue("namespace")
	objects, changes, cancel := r.typed.ListAndWatch(namespace)
	defer cancel()

	stream := newEventStream(w)
	for _, obj := range objects {
		stream.send(string(store.Added), obj)
	}
	stream.send(Synced, struct{}{})
	stream.relay(req.Context(), changes, namespace)
}

// eventStream writes Server-Sent Events. After a write that failed it
// writes nothing more, and err says why.
type eventStream struct {
	w     http.ResponseWriter
	flush *http.ResponseController
	err   error
}

func newEventStream(w http.ResponseWriter) *eventStream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	return &eventStream{w: w, flush: http.NewResponseController(w)}
}

// relay sends the changes in namespace, or in all when it is empty, until
// the client leaves or the store closes the watch.
func (s *eventStream) relay(ctx context.Context, changes <-chan store.Event, namespace string) {
	for s.err == nil {
		select {
		case <-ctx.Done():
			return
		case change, open := <-changes:
			if !open {
				return // too far behind: see store.Watch
			}
			if namespace == "" || change.Key.Namespace == namespace {
				s.send(string(change.Type), change.Object)
			}
		}
	}
}

// send writes one event and pushes it out at once: the reader is waiting.
func (s *eventStream) send(event string, body any) {
	if s.err != nil {
		return
	}
	data, err := json.Marshal(body)
	if err == nil {
		_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	}
	if err == nil {
		err = s.flush.Flush()
	}
	s.err = err
}
