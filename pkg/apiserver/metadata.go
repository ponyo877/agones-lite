package apiserver

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ownPrefix marks the annotations scrim keeps for itself.
const ownPrefix = "scrim/"

// metadataPatch is a JSON merge patch (RFC 7386) of the labels and
// annotations: a key with a value sets it, a key with null removes it, and
// a key left out stays as it is.
type metadataPatch struct {
	Metadata struct {
		Labels      map[string]*string `json:"labels"`
		Annotations map[string]*string `json:"annotations"`
	} `json:"metadata"`
}

// relabel changes the labels and annotations of a GameServer, and nothing
// else, as "kubectl label" and "kubectl annotate" do. What the game server
// set through the SDK stays, unless the patch names it.
//
// The rest of a GameServer cannot be changed. Its spec is what its
// containers were made from, and its status belongs to the controllers.
func (s *Server) relabel(w http.ResponseWriter, r *http.Request) {
	var patch metadataPatch
	if _, err := decode(r, &patch); err != nil {
		fail(w, invalid{fmt.Errorf("only metadata.labels and metadata.annotations of a GameServer can be changed: %w", err)})
		return
	}
	if errs := ownKeys(patch); len(errs) > 0 {
		fail(w, invalid{errs.ToAggregate()})
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	var errs field.ErrorList
	err := s.servers.typed.Change(namespace, name, func(gs *agonesv1.GameServer) bool {
		labels := merged(gs.Labels, patch.Metadata.Labels)
		annotations := merged(gs.Annotations, patch.Metadata.Annotations)
		errs = validMetadata(labels, annotations)
		if len(errs) > 0 || (maps.Equal(labels, gs.Labels) && maps.Equal(annotations, gs.Annotations)) {
			return false
		}
		gs.Labels, gs.Annotations = labels, annotations
		return true
	})
	switch {
	case err != nil:
		fail(w, err)
	case len(errs) > 0:
		fail(w, invalid{errs.ToAggregate()})
	default:
		s.servers.show(w, namespace, name)
	}
}

// merged returns current with patch applied, as a new map.
func merged(current map[string]string, patch map[string]*string) map[string]string {
	out := maps.Clone(current)
	for key, value := range patch {
		if value == nil {
			delete(out, key)
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[key] = *value
	}
	return out
}

// ownKeys refuses a patch that names one of scrim's own annotations. They
// say which requests a game server has made, and which allocation it was
// given to; changed by hand, they would say what is not so.
func ownKeys(patch metadataPatch) field.ErrorList {
	var errs field.ErrorList
	at := field.NewPath("metadata", "annotations")
	for _, key := range slices.Sorted(maps.Keys(patch.Metadata.Annotations)) {
		if strings.HasPrefix(key, ownPrefix) {
			errs = append(errs, field.Forbidden(at.Key(key), "the annotations under scrim/ are scrim's to write"))
		}
	}
	return errs
}

// validMetadata holds the labels and annotations to Kubernetes' rules.
func validMetadata(labels, annotations map[string]string) field.ErrorList {
	at := field.NewPath("metadata")
	return append(
		metav1validation.ValidateLabels(labels, at.Child("labels")),
		apivalidation.ValidateAnnotations(annotations, at.Child("annotations"))...,
	)
}
