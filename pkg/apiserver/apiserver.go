// Package apiserver is the management API: plain REST, with resources in the
// same JSON and YAML shape Agones uses.
//
// It also does what the Kubernetes API server and the admission webhooks do
// for Agones. A new resource gets its defaults, is held against the limits
// of its CRD schema, is validated by upstream's own rules, and is checked
// against what Docker can run, before it is stored.
//
//   - apiserver.go: the routes and the helpers every handler shares
//   - resource.go: list, get, create, update and delete, written once for all kinds
//   - admission.go: defaulting and validation, one function per kind
//   - schema.go: the limits of Agones' CRD schemas, which nothing else checks here
//   - watch.go: a list that goes on, as Server-Sent Events
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
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/ponyo877/scrim/pkg/agent"
	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/events"
	"github.com/ponyo877/scrim/pkg/podspec"
	"github.com/ponyo877/scrim/pkg/store"
)

// maxBody is the largest request body accepted.
const maxBody = 1 << 20

// Server serves the management API.
type Server struct {
	servers resource[*agonesv1.GameServer]
	sets    resource[*agonesv1.GameServerSet]
	fleets  resource[*agonesv1.Fleet]
	scalers resource[*autoscalingv1.FleetAutoscaler]
	nodes   store.Typed[*scrimv1.Node]
	events  *events.Recorder
	nodeOps NodeOperator
	logs    LogSource
	saved   func(context.Context) error // returns when the store's file has caught up
	lag     func() time.Duration        // for how long the file has been behind
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
		saved:   s.Sync,
		lag:     s.Lag,
		servers: resource[*agonesv1.GameServer]{
			typed: store.GameServersOf(s),
			blank: func() *agonesv1.GameServer { return &agonesv1.GameServer{} },
			fill:  fillGameServer,
			admit: admitGameServer,
		},
		// GameServerSets belong to their Fleet's controller, so they are read-only here.
		sets: resource[*agonesv1.GameServerSet]{
			typed: store.GameServerSetsOf(s),
		},
		fleets: resource[*agonesv1.Fleet]{
			typed: store.FleetsOf(s),
			blank: func() *agonesv1.Fleet { return &agonesv1.Fleet{} },
			fill:  fillFleet,
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

// unwell is how far behind the file may be before /healthz says so. It is
// ten times what an allocation is refused for: a disk that hiccups is not
// a control plane to restart.
const unwell = 10 * time.Second

// Handler returns the routes of the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
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
	all := http.NewServeMux()
	all.HandleFunc("GET /healthz", s.health)
	all.Handle("/", answerWhenSaved(mux, s.saved))
	return all
}

// health says whether the control plane can do its work, which is more than
// being up. One that cannot save goes on deciding things that nobody will
// be told: it answers its pings, and to whoever needs it, it is gone. That
// is a gray failure, a fault that the check on a system does not see and
// its users do (Huang et al., "Gray Failure: The Achilles' Heel of
// Cloud-Scale Systems", HotOS 2017). So the check asks what the users would.
//
// Unlike every other route this one does not wait for the file: it is the
// one that has to answer when the file is the trouble.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	if lag := s.lag(); lag > unwell {
		respond(w, http.StatusServiceUnavailable, fmt.Sprintf("nothing has been saved for %s", lag.Round(time.Second)))
		return
	}
	respond(w, http.StatusOK, "ok")
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	respond(w, http.StatusOK, items[*scrimv1.Node]{Items: s.nodes.List("")})
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
	request := agent.LogRequest{
		Namespace: r.PathValue("namespace"),
		Name:      r.PathValue("name"),
		Container: query.Get("container"),
		Follow:    query.Get("follow") == "true",
		Tail:      tail,
	}
	if err := s.hasLog(request); err != nil {
		fail(w, err)
		return
	}
	log, err := s.logs.Logs(r.Context(), request)
	if err != nil {
		fail(w, err)
		return
	}
	defer func() { _ = log.Close() }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	stream(w, log)
}

// hasLog says why there is no log to ask a node for: nothing by that name
// could have written one, or it is not on a node yet. A node that is asked
// anyway answers with an error of its own, and that would be passed on as
// a fault of the server, which it is not.
func (s *Server) hasLog(req agent.LogRequest) error {
	gs, ok := s.servers.typed.Get(req.Namespace, req.Name)
	switch {
	case !ok:
		return store.ErrNotFound
	case req.Container != "" && !hasContainer(gs, req.Container):
		return fmt.Errorf("container %q: %w", req.Container, store.ErrNotFound)
	case gs.Status.NodeName == "":
		return notYet{errors.New("the game server is not on a node yet, so it has no log")}
	}
	return nil
}

// hasContainer reports whether gs runs a container of that name: one of
// its Pod, or the sidecar every GameServer gets.
func hasContainer(gs *agonesv1.GameServer, name string) bool {
	if name == podspec.SidecarName {
		return true
	}
	return slices.ContainsFunc(gs.Spec.Template.Spec.Containers, func(c corev1.Container) bool { return c.Name == name })
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
	if _, err := decode(r, &body); err != nil || body.Replicas == nil || *body.Replicas < 0 {
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
//
// It also returns the body as it was written. The Go types cannot tell a
// field that was left out from one set to its zero value, and the defaults
// of the CRD schema hang on that difference.
func decode(r *http.Request, into any) (written map[string]any, err error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(body, &written); err != nil {
		return nil, err
	}
	return written, yaml.UnmarshalStrict(body, into)
}

// invalid marks an error as the caller's mistake.
type invalid struct{ error }

// notYet marks an error as one that time will mend: what was asked for is
// right, but the resource is not that far yet.
type notYet struct{ error }

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("an answer was cut short", "error", err)
	}
}

func fail(w http.ResponseWriter, err error) {
	respond(w, statusOf(err), map[string]string{"error": err.Error()})
}

func statusOf(err error) int {
	var bad invalid
	var early notYet
	switch {
	case errors.As(err, &bad):
		return http.StatusUnprocessableEntity
	case errors.As(err, &early), errors.Is(err, agent.ErrNoLog):
		return http.StatusConflict
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
