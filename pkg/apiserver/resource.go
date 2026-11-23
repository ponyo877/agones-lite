package apiserver

import (
	"net/http"
	"time"

	"github.com/ponyo877/agones-lite/pkg/store"
)

// resource serves one kind of object under /v1/namespaces/{namespace}/<plural>.
// Its optional functions say what the kind allows.
type resource[T store.Object] struct {
	typed store.Typed[T]
	blank func() T
	// admit gives a new or changed object its defaults and validates it.
	// A kind without admit is read-only.
	admit func(T) (warnings []string, err error)
	// carry copies into incoming what a client may not change. A kind
	// without carry can be created and deleted, but not updated.
	carry func(stored, incoming T)
}

func (r resource[T]) mount(mux *http.ServeMux, plural string) {
	base := "/v1/namespaces/{namespace}/" + plural
	mux.HandleFunc("GET /v1/"+plural, r.list) // across all namespaces
	mux.HandleFunc("GET "+base, r.list)
	mux.HandleFunc("GET "+base+"/{name}", r.get)
	if r.admit == nil {
		return
	}
	mux.HandleFunc("POST "+base, r.create)
	mux.HandleFunc("DELETE "+base+"/{name}", r.remove)
	if r.carry != nil {
		mux.HandleFunc("PUT "+base+"/{name}", r.put)
	}
}

func (r resource[T]) list(w http.ResponseWriter, req *http.Request) {
	respond(w, http.StatusOK, items[T]{Items: r.typed.List(req.PathValue("namespace"))})
}

func (r resource[T]) get(w http.ResponseWriter, req *http.Request) {
	r.show(w, req.PathValue("namespace"), req.PathValue("name"))
}

func (r resource[T]) show(w http.ResponseWriter, namespace, name string) {
	obj, ok := r.typed.Get(namespace, name)
	if !ok {
		fail(w, store.ErrNotFound)
		return
	}
	respond(w, http.StatusOK, obj)
}

func (r resource[T]) create(w http.ResponseWriter, req *http.Request) {
	obj, warnings, err := r.read(req)
	if err != nil {
		fail(w, invalid{err})
		return
	}
	saved, err := r.typed.Create(obj)
	reply(w, http.StatusCreated, saved, warnings, err)
}

// put creates the object, or replaces it when it exists: what "apply" needs.
func (r resource[T]) put(w http.ResponseWriter, req *http.Request) {
	obj, warnings, err := r.read(req)
	if err != nil {
		fail(w, invalid{err})
		return
	}
	stored, exists := r.typed.Get(obj.GetNamespace(), obj.GetName())
	if !exists {
		saved, err := r.typed.Create(obj)
		reply(w, http.StatusCreated, saved, warnings, err)
		return
	}
	keepIdentity(stored, obj)
	r.carry(stored, obj)
	saved, err := r.typed.Update(obj)
	reply(w, http.StatusOK, saved, warnings, err)
}

// remove starts a deletion. The object stays until its controller is done.
func (r resource[T]) remove(w http.ResponseWriter, req *http.Request) {
	namespace, name := req.PathValue("namespace"), req.PathValue("name")
	if _, ok := r.typed.Get(namespace, name); !ok {
		fail(w, store.ErrNotFound)
		return
	}
	if err := r.typed.MarkDeleted(namespace, name, time.Now()); err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

// read decodes the body, names the object after the URL, and admits it.
func (r resource[T]) read(req *http.Request) (T, []string, error) {
	obj := r.blank()
	if err := decode(req, obj); err != nil {
		return obj, nil, err
	}
	obj.SetNamespace(req.PathValue("namespace"))
	if name := req.PathValue("name"); name != "" {
		obj.SetName(name)
	}
	warnings, err := r.admit(obj)
	return obj, warnings, err
}

// keepIdentity carries over the metadata only the store may set. A client
// that sends no resourceVersion means "replace whatever is there".
func keepIdentity(stored, incoming store.Object) {
	incoming.SetUID(stored.GetUID())
	incoming.SetCreationTimestamp(stored.GetCreationTimestamp())
	incoming.SetGeneration(stored.GetGeneration())
	if incoming.GetResourceVersion() == "" {
		incoming.SetResourceVersion(stored.GetResourceVersion())
	}
}

func reply(w http.ResponseWriter, status int, saved any, warnings []string, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	for _, warning := range warnings {
		w.Header().Add("Warning", warning)
	}
	respond(w, status, saved)
}
