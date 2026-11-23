// Package apiserver is the management API: plain REST, with resources in the
// same JSON and YAML shape Agones uses.
//
// It also does what the Kubernetes admission webhooks do for Agones. A new
// resource gets its defaults, is validated by upstream's own rules, and is
// checked against what Docker can run, before it is stored.
//
//   - apiserver.go: the routes and the helpers every handler shares
//   - resource.go: list, get, create, update and delete, written once for all kinds
//   - admission.go: defaulting and validation, one function per kind
//
// Upstream counterpart: the Kubernetes API server, plus the webhooks that the
// controllers in pkg/gameservers, pkg/fleets and others register.
package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"sigs.k8s.io/yaml"

	"github.com/ponyo877/agones-lite/pkg/agent"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// maxBody is the largest request body accepted.
const maxBody = 1 << 20

// Server serves the management API.
type Server struct {
	servers resource[*agonesv1.GameServer]
	sets    resource[*agonesv1.GameServerSet]
	fleets  resource[*agonesv1.Fleet]
	scalers resource[*autoscalingv1.FleetAutoscaler]
	nodes   store.Typed[*litev1.Node]
	events  *events.Recorder
	nodeOps NodeOperator
	logs    LogSource
}

// LogSource reads a game server's log from the node it runs on.
// pkg/nodes.Local is one.
type LogSource interface {
	Logs(ctx context.Context, req agent.LogRequest) (io.ReadCloser, error)
}

// NodeOperator is what the API needs to take a node out of and back into
// service. pkg/nodes.Controller is one.
type NodeOperator interface {
	Cordon(name string, drain bool) error
	Uncordon(name string) error
}

// New returns a Server working on s. recorder is where the event history is
// read from; it may be nil.
func New(s *store.Store, recorder *events.Recorder, nodeOps NodeOperator, logs LogSource) *Server {
	return &Server{
		events:  recorder,
		nodeOps: nodeOps,
		logs:    logs,
		servers: resource[*agonesv1.GameServer]{
			typed: store.GameServersOf(s),
			blank: func() *agonesv1.GameServer { return &agonesv1.GameServer{} },
			admit: admitGameServer,
		},
		// GameServerSets belong to their Fleet's controller, so they are read-only here.
		sets: resource[*agonesv1.GameServerSet]{
			typed: store.GameServerSetsOf(s),
		},
		fleets: resource[*agonesv1.Fleet]{
			typed: store.FleetsOf(s),
			blank: func() *agonesv1.Fleet { return &agonesv1.Fleet{} },
			admit: admitFleet,
			carry: carryFleet,
		},
		scalers: resource[*autoscalingv1.FleetAutoscaler]{
			typed: store.FleetAutoscalersOf(s),
			blank: func() *autoscalingv1.FleetAutoscaler { return &autoscalingv1.FleetAutoscaler{} },
			admit: admitFleetAutoscaler,
			carry: carryFleetAutoscaler,
		},
		nodes: store.NodesOf(s),
	}
}

// Handler returns the routes of the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /v1/nodes", s.listNodes)
	mux.HandleFunc("GET /v1/nodes/{name}", s.getNode)
	mux.HandleFunc("DELETE /v1/nodes/{name}", s.deleteNode)
	mux.HandleFunc("POST /v1/nodes/{name}/{operation}", s.operateNode)
	mux.HandleFunc("GET /v1/namespaces/{namespace}/events", s.listEvents)
	s.servers.mount(mux, "gameservers")
	s.sets.mount(mux, "gameserversets")
	s.fleets.mount(mux, "fleets")
	s.scalers.mount(mux, "fleetautoscalers")
	mux.HandleFunc("PUT /v1/namespaces/{namespace}/fleets/{name}/scale", s.scaleFleet)
	mux.HandleFunc("GET /v1/namespaces/{namespace}/gameservers/{name}/logs", s.gameServerLogs)
	return mux
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	respond(w, http.StatusOK, items[*litev1.Node]{Items: s.nodes.List("")})
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodes.Get("", r.PathValue("name"))
	if !ok {
		fail(w, store.ErrNotFound)
		return
	}
	respond(w, http.StatusOK, node)
}

// deleteNode forgets a node. Its game servers are then treated as lost at
// once, so this is for a machine that is not coming back.
func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	if err := s.nodes.Delete("", r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// operateNode handles cordon, uncordon and drain.
func (s *Server) operateNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.nodes.Get("", name); !ok {
		fail(w, store.ErrNotFound)
		return
	}
	var err error
	switch r.PathValue("operation") {
	case "cordon":
		err = s.nodeOps.Cordon(name, false)
	case "drain":
		err = s.nodeOps.Cordon(name, true)
	case "uncordon":
		err = s.nodeOps.Uncordon(name)
	default:
		err = store.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.getNode(w, r)
}

// gameServerLogs streams what a game server's container wrote. The query
// parameters container, follow and tail choose what, as with "docker logs".
func (s *Server) gameServerLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	tail, _ := strconv.Atoi(query.Get("tail"))
	log, err := s.logs.Logs(r.Context(), agent.LogRequest{
		Namespace: r.PathValue("namespace"),
		Name:      r.PathValue("name"),
		Container: query.Get("container"),
		Follow:    query.Get("follow") == "true",
		Tail:      tail,
	})
	if err != nil {
		fail(w, err)
		return
	}
	defer func() { _ = log.Close() }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	stream(w, log)
}

// stream copies a log to the client as it arrives, instead of when the
// buffer happens to fill up, so that following a log shows lines at once.
func stream(w http.ResponseWriter, log io.Reader) {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 16*1024)
	for {
		n, err := log.Read(buffer)
		if n > 0 {
			_, _ = w.Write(buffer[:n])
		}
		if flusher != nil {
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// listEvents returns the history of one resource, named by the kind and name
// query parameters, newest first.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	history, err := s.events.For(query.Get("kind"), r.PathValue("namespace"), query.Get("name"))
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusOK, items[events.Event]{Items: history})
}

// scaleFleet changes only the size of a Fleet, as "kubectl scale" would.
func (s *Server) scaleFleet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Replicas *int32 `json:"replicas"`
	}
	if err := decode(r, &body); err != nil || body.Replicas == nil || *body.Replicas < 0 {
		fail(w, invalid{errors.New(`the body must be {"replicas": N} with N of 0 or more`)})
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	err := s.fleets.typed.Change(namespace, name, func(fleet *agonesv1.Fleet) bool {
		if fleet.Spec.Replicas == *body.Replicas {
			return false
		}
		fleet.Spec.Replicas = *body.Replicas
		fleet.Generation++
		return true
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.fleets.show(w, namespace, name)
}

// items is the body of every list response.
type items[T any] struct {
	Items []T `json:"items"`
}

// decode reads a JSON or YAML body. Unknown fields are errors, so a typo in
// a manifest is caught instead of silently ignored.
func decode(r *http.Request, into any) error {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return err
	}
	return yaml.UnmarshalStrict(body, into)
}

// invalid marks an error as the caller's mistake.
type invalid struct{ error }

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, err error) {
	respond(w, statusOf(err), map[string]string{"error": err.Error()})
}

func statusOf(err error) int {
	var bad invalid
	switch {
	case errors.As(err, &bad):
		return http.StatusUnprocessableEntity
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrExists), errors.Is(err, store.ErrConflict):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// Error is how a client reads a failed response back.
type Error struct {
	Status  int
	Message string `json:"error"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}
