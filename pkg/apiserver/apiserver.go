// Package apiserver is the management API: plain REST, with resources in the
// same JSON and YAML shape Agones uses.
//
// It also does what the Kubernetes admission webhooks do for Agones. A new
// GameServer gets its defaults, is validated by upstream's own rules, and is
// checked against what Docker can run, before it is stored.
//
// Upstream counterpart: the Kubernetes API server, plus the webhooks that
// pkg/gameservers/controller.go registers.
package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"sigs.k8s.io/yaml"

	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/podspec"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// maxBody is the largest request body accepted.
const maxBody = 1 << 20

// Server serves the management API.
type Server struct {
	servers    store.Typed[*agonesv1.GameServer]
	nodes      store.Typed[*litev1.Node]
	controller *gameservers.Controller
}

// New returns a Server working on s.
func New(s *store.Store, controller *gameservers.Controller) *Server {
	return &Server{servers: store.GameServersOf(s), nodes: store.NodesOf(s), controller: controller}
}

// Handler returns the routes of the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /v1/nodes", s.listNodes)
	mux.HandleFunc("GET /v1/namespaces/{namespace}/gameservers", s.listGameServers)
	mux.HandleFunc("POST /v1/namespaces/{namespace}/gameservers", s.createGameServer)
	mux.HandleFunc("GET /v1/namespaces/{namespace}/gameservers/{name}", s.getGameServer)
	mux.HandleFunc("DELETE /v1/namespaces/{namespace}/gameservers/{name}", s.deleteGameServer)
	return mux
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	respond(w, http.StatusOK, map[string]any{"items": s.nodes.List("")})
}

func (s *Server) listGameServers(w http.ResponseWriter, r *http.Request) {
	list := agonesv1.GameServerList{}
	list.APIVersion, list.Kind = "agones.dev/v1", "GameServerList"
	for _, gs := range s.servers.List(r.PathValue("namespace")) {
		list.Items = append(list.Items, *gs)
	}
	respond(w, http.StatusOK, list)
}

func (s *Server) getGameServer(w http.ResponseWriter, r *http.Request) {
	gs, ok := s.servers.Get(r.PathValue("namespace"), r.PathValue("name"))
	if !ok {
		fail(w, store.ErrNotFound)
		return
	}
	respond(w, http.StatusOK, gs)
}

func (s *Server) createGameServer(w http.ResponseWriter, r *http.Request) {
	gs := &agonesv1.GameServer{}
	if err := decode(r, gs); err != nil {
		fail(w, invalid{err})
		return
	}
	gs.Namespace = r.PathValue("namespace")
	warnings, err := admit(gs)
	if err != nil {
		fail(w, invalid{err})
		return
	}
	created, err := s.servers.Create(gs)
	if err != nil {
		fail(w, err)
		return
	}
	for _, warning := range warnings {
		w.Header().Add("Warning", warning)
	}
	respond(w, http.StatusCreated, created)
}

func (s *Server) deleteGameServer(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if _, ok := s.servers.Get(namespace, name); !ok {
		fail(w, store.ErrNotFound)
		return
	}
	if err := s.controller.Delete(namespace, name); err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

// admit does the work of Agones' admission webhooks: defaults, upstream's
// validation, then our own check of the Pod definition.
func admit(gs *agonesv1.GameServer) (warnings []string, err error) {
	gs.APIVersion, gs.Kind = "agones.dev/v1", "GameServer"
	gs.ApplyDefaults()
	errs := gs.Validate(agones.NoHooks{})
	warnings, unsupported := podspec.Check(gs)
	return warnings, append(errs, unsupported...).ToAggregate()
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
