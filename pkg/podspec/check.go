// Package podspec turns the Kubernetes Pod definition inside a GameServer
// into Docker containers.
//
// Docker can express only part of a Pod. check.go decides, field by field,
// what to do with the rest; containers.go does the translation.
//
// Upstream counterpart: GameServer.Pod in pkg/apis/agones/v1/gameserver.go
// builds the Pod, and the kubelet runs it.
package podspec

import (
	"reflect"
	"strings"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// handling says what to do with a Pod field that is set.
type handling int

const (
	rejected   handling = iota // the zero value: any field not listed below
	understood                 // carried over to Docker
	ignored                    // dropped with a warning; running without it changes nothing
)

// The fields we know, by Go field name. Everything else is rejected, because
// silently dropping a field we do not know could change how the game runs.
var (
	podFields = map[string]handling{
		"Containers":                    understood,
		"Volumes":                       understood,
		"TerminationGracePeriodSeconds": understood,

		// Scheduling belongs to pkg/scheduling here.
		"NodeSelector":              ignored,
		"Affinity":                  ignored,
		"Tolerations":               ignored,
		"TopologySpreadConstraints": ignored,
		"SchedulerName":             ignored,
		"PriorityClassName":         ignored,
		// There is no Kubernetes API for the game server to talk to.
		"ServiceAccountName":           ignored,
		"DeprecatedServiceAccount":     ignored,
		"AutomountServiceAccountToken": ignored,
		// Registry credentials come from each node's Docker configuration.
		"ImagePullSecrets": ignored,
		// Containers are never restarted, as with upstream's default.
		"RestartPolicy": ignored,
	}
	containerFields = map[string]handling{
		"Name":            understood,
		"Image":           understood,
		"Command":         understood,
		"Args":            understood,
		"WorkingDir":      understood,
		"Env":             understood,
		"Ports":           understood,
		"Resources":       understood,
		"VolumeMounts":    understood,
		"ImagePullPolicy": understood,
		"SecurityContext": understood,

		"TerminationMessagePath":   ignored,
		"TerminationMessagePolicy": ignored,
	}
	securityFields = map[string]handling{
		"RunAsUser":              understood,
		"RunAsGroup":             understood,
		"ReadOnlyRootFilesystem": understood,
	}
	volumeFields = map[string]handling{
		"EmptyDir": understood,
		"HostPath": understood,
	}
	mountFields = map[string]handling{
		"Name":      understood,
		"MountPath": understood,
		"ReadOnly":  understood,
	}
	resourceFields = map[string]handling{
		"Limits":   understood,
		"Requests": understood,
	}
)

// Check reports what in the Pod definition of a GameServer spec cannot be
// honoured: warnings for fields that are safely ignored, errors for the rest.
// at is where the spec sits in its manifest: "spec" in a GameServer,
// "spec.template.spec" in a Fleet.
func Check(spec *agonesv1.GameServerSpec, at *field.Path) (warnings []string, errs field.ErrorList) {
	var r findings
	pod := spec.Template.Spec
	path := at.Child("template", "spec")

	r.audit(pod, path, podFields)
	for i, c := range pod.Containers {
		r.container(c, path.Child("containers").Index(i))
	}
	for i, v := range pod.Volumes {
		r.audit(v.VolumeSource, path.Child("volumes").Index(i), volumeFields)
	}
	return r.warnings, r.errs
}

type findings struct {
	warnings []string
	errs     field.ErrorList
}

// audit looks at every field of the struct v that is set and files it
// according to known.
func (r *findings) audit(v any, path *field.Path, known map[string]handling) {
	value := reflect.ValueOf(v)
	for i := range value.NumField() {
		if value.Field(i).IsZero() {
			continue
		}
		f := value.Type().Field(i)
		at := path.Child(jsonName(f))
		switch known[f.Name] {
		case ignored:
			r.warnings = append(r.warnings, at.String()+" is ignored")
		case rejected:
			r.reject(at)
		}
	}
}

func (r *findings) reject(at *field.Path) {
	r.errs = append(r.errs, field.Forbidden(at, "not supported by agones-lite"))
}

func (r *findings) container(c corev1.Container, path *field.Path) {
	r.audit(c, path, containerFields)
	if c.SecurityContext != nil {
		r.audit(*c.SecurityContext, path.Child("securityContext"), securityFields)
	}
	for i, e := range c.Env {
		if e.ValueFrom != nil {
			r.reject(path.Child("env").Index(i).Child("valueFrom"))
		}
	}
	for i, m := range c.VolumeMounts {
		r.audit(m, path.Child("volumeMounts").Index(i), mountFields)
	}
	r.resources(c.Resources, path.Child("resources"))
}

// resources allows only CPU and memory: Docker has no notion of the others.
func (r *findings) resources(res corev1.ResourceRequirements, path *field.Path) {
	r.audit(res, path, resourceFields)
	lists := map[string]corev1.ResourceList{"limits": res.Limits, "requests": res.Requests}
	for kind, list := range lists {
		for name := range list {
			if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
				r.reject(path.Child(kind).Key(string(name)))
			}
		}
	}
}

// jsonName is the name the field has in YAML, so messages match what users wrote.
func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" {
		return f.Name
	}
	return name
}
