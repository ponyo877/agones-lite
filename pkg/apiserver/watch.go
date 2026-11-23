package apiserver

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ponyo877/agones-lite/pkg/store"
)

// Synced is the event that ends the first part of a watch: everything that
// existed has been sent, and what follows are changes.
const Synced = "Synced"

// watch is a list that never ends: every object as it is now, then Synced,
// then every change. The answer is a stream of Server-Sent Events, each
// named after what happened (Added, Modified, Deleted) and carrying the
// object as JSON.
//
// A client that falls too far behind loses changes, as any watcher of the
// store does, and should then watch again from the start.
func (r resource[T]) watch(w http.ResponseWriter, req *http.Request) {
	namespace := req.PathValue("namespace")
	objects, changes, cancel := r.typed.ListAndWatch(namespace)
	defer cancel()

	stream := newEventStream(w)
	for _, obj := range objects {
		stream.send(string(store.Added), obj)
	}
	stream.send(Synced, struct{}{})
	for stream.err == nil {
		select {
		case <-req.Context().Done():
			return
		case change := <-changes:
			if namespace == "" || change.Key.Namespace == namespace {
				stream.send(string(change.Type), change.Object)
			}
		}
	}
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
