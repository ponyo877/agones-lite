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
	"math"
	"reflect"
	"strings"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/ponyo877/agones-lite/pkg/docker"
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
		// Resources of the Pod as a whole are an alpha feature of
		// Kubernetes, and a cluster without it drops the field. Upstream's
		// unity-simple sample sets it, so refusing it would refuse the sample.
		"Resources": ignored,
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

// The largest limits Docker's API can carry. It takes CPU as an int64 of
// billionths of a core, which containers.go makes from thousandths, and
// memory as an int64 of bytes.
var (
	maxCPU    = resource.NewMilliQuantity(math.MaxInt64/nanoPerMilli, resource.DecimalSI)
	maxMemory = resource.NewQuantity(math.MaxInt64, resource.BinarySI)
)

// Check reports what in the Pod definition of a GameServer spec cannot be
// honoured: warnings for fields that are safely ignored, errors for the rest.
// at is where the spec sits in its manifest: "spec" in a GameServer,
// "spec.template.spec" in a Fleet.
//
// The errors include what Kubernetes itself refuses in a Pod, as far as it
// would reach Docker: upstream leaves that to the API server, which is not
// there to create the Pod and fail.
func Check(spec *agonesv1.GameServerSpec, at *field.Path) (warnings []string, errs field.ErrorList) {
	var r findings
	pod := spec.Template.Spec
	path := at.Child("template", "spec")

	r.audit(pod, path, podFields)
	r.gracePeriod(pod.TerminationGracePeriodSeconds, path.Child("terminationGracePeriodSeconds"))
	volumes := r.volumes(pod.Volumes, path.Child("volumes"))
	r.names(pod.Containers, path.Child("containers"))
	for i, c := range pod.Containers {
		r.container(c, path.Child("containers").Index(i), volumes)
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
		if unset(value.Field(i)) {
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

// unset reports whether a field was left out. An empty list or map counts
// as left out: "initContainers: []" asks for nothing.
func unset(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	}
	return v.IsZero()
}

func (r *findings) reject(at *field.Path) {
	r.errs = append(r.errs, field.Forbidden(at, "not supported by agones-lite"))
}

func (r *findings) invalid(at *field.Path, value any, why string) {
	r.errs = append(r.errs, field.Invalid(at, value, why))
}

// gracePeriod refuses a negative period. Kubernetes documents the value as
// non-negative, and Docker takes a negative one to mean "wait for ever".
func (r *findings) gracePeriod(seconds *int64, at *field.Path) {
	if seconds != nil && *seconds < 0 {
		r.invalid(at, *seconds, "must be greater than or equal to 0")
	}
}

// name holds a name against Kubernetes' rule for the names inside a Pod.
// They become part of the names of Docker's containers and volumes.
func (r *findings) name(name string, at *field.Path) {
	for _, problem := range validation.IsDNS1123Label(name) {
		r.invalid(at, name, problem)
	}
}

// names makes sure every container can have a Docker container of its own:
// none is called like another, or like the sidecar we add.
func (r *findings) names(containers []corev1.Container, path *field.Path) {
	seen := map[string]bool{}
	for i, c := range containers {
		at := path.Index(i).Child("name")
		r.name(c.Name, at)
		switch {
		case c.Name == SidecarName:
			r.invalid(at, c.Name, "is the name of the SDK sidecar, which every GameServer gets")
		case seen[c.Name]:
			r.errs = append(r.errs, field.Duplicate(at, c.Name))
		}
		seen[c.Name] = true
	}
}

// volumes checks the volumes of the Pod and returns their names, for the
// mounts to be held against.
func (r *findings) volumes(volumes []corev1.Volume, path *field.Path) map[string]bool {
	names := map[string]bool{}
	for i, v := range volumes {
		at := path.Index(i)
		r.name(v.Name, at.Child("name"))
		if names[v.Name] {
			r.errs = append(r.errs, field.Duplicate(at.Child("name"), v.Name))
		}
		names[v.Name] = true
		r.audit(v.VolumeSource, at, volumeFields)
		r.source(v.VolumeSource, at)
	}
	return names
}

// source makes sure a volume says where its files are. One that does not
// would be mounted from the path "".
func (r *findings) source(source corev1.VolumeSource, at *field.Path) {
	switch {
	case source == corev1.VolumeSource{}:
		r.errs = append(r.errs, field.Required(at, "a volume is an emptyDir or a hostPath"))
	case source.HostPath != nil && source.HostPath.Path == "":
		r.errs = append(r.errs, field.Required(at.Child("hostPath", "path"), ""))
	}
}

func (r *findings) container(c corev1.Container, path *field.Path, volumes map[string]bool) {
	r.audit(c, path, containerFields)
	if !docker.ValidImage(c.Image) {
		r.invalid(path.Child("image"), c.Image, "is not an image reference Docker can read")
	}
	if c.SecurityContext != nil {
		r.audit(*c.SecurityContext, path.Child("securityContext"), securityFields)
	}
	for i, e := range c.Env {
		if e.ValueFrom != nil {
			r.reject(path.Child("env").Index(i).Child("valueFrom"))
		}
	}
	for i, m := range c.VolumeMounts {
		r.mount(m, path.Child("volumeMounts").Index(i), volumes)
	}
	r.resources(c.Resources, path.Child("resources"))
}

// mount makes sure a mount names a volume of the Pod, and a place for it.
func (r *findings) mount(m corev1.VolumeMount, at *field.Path, volumes map[string]bool) {
	r.audit(m, at, mountFields)
	if !volumes[m.Name] {
		r.errs = append(r.errs, field.NotFound(at.Child("name"), m.Name))
	}
	if m.MountPath == "" {
		r.errs = append(r.errs, field.Required(at.Child("mountPath"), ""))
	}
}

// resources allows only CPU and memory: Docker has no notion of the others.
// A limit must also be one Docker's API can carry.
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
	r.atMost(res.Limits.Cpu(), maxCPU, path.Child("limits").Key("cpu"))
	r.atMost(res.Limits.Memory(), maxMemory, path.Child("limits").Key("memory"))
}

func (r *findings) atMost(limit, most *resource.Quantity, at *field.Path) {
	if limit.Cmp(*most) > 0 {
		r.invalid(at, limit.String(), "must be less than or equal to "+most.String()+", the most Docker can be told")
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
