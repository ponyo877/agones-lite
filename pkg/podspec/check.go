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
	"fmt"
	"math"
	"reflect"
	"strings"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/ponyo877/scrim/pkg/docker"
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
	// A port of a container is a note to the reader, as it is in
	// Kubernetes: what is published is in spec.ports of the GameServer.
	// Publishing from here, with hostPort or hostIP, is rejected.
	portFields = map[string]handling{
		"Name":          understood,
		"ContainerPort": understood,
		"Protocol":      understood,
	}
	volumeFields = map[string]handling{
		"EmptyDir": understood,
		"HostPath": understood,
	}
	// Docker keeps an emptyDir on disk and gives it no limit.
	emptyDirFields = map[string]handling{
		"Medium":    ignored,
		"SizeLimit": ignored,
	}
	// Docker mounts a host path whatever is there, and makes a directory
	// where nothing is: it does not check first, as the type asks for.
	hostPathFields = map[string]handling{
		"Path": understood,
		"Type": ignored,
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
// memory as an int64 of bytes. minMemory is the least memory Docker gives
// a container: it refuses to create one with less.
var (
	maxCPU    = resource.NewMilliQuantity(math.MaxInt64/nanoPerMilli, resource.DecimalSI)
	maxMemory = resource.NewQuantity(math.MaxInt64, resource.BinarySI)
	minMemory = resource.NewQuantity(6<<20, resource.BinarySI)
)

const (
	// maxGracePeriod is the longest time to stop in, in seconds. Docker is
	// told the seconds and makes a time.Duration of them, as GracePeriod
	// does: a number of 32 bits leaves room for both.
	maxGracePeriod = math.MaxInt32
	// maxID is the largest user or group Linux has a number for.
	maxID = math.MaxInt32
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
	r.errs = append(r.errs, field.Forbidden(at, "not supported by scrim"))
}

func (r *findings) invalid(at *field.Path, value any, why string) {
	r.errs = append(r.errs, field.Invalid(at, value, why))
}

// gracePeriod refuses a negative period, and one too long to tell Docker.
// Kubernetes documents the value as non-negative, and Docker takes a
// negative one to mean "wait for ever".
func (r *findings) gracePeriod(seconds *int64, at *field.Path) {
	switch {
	case seconds == nil:
	case *seconds < 0:
		r.invalid(at, *seconds, "must be greater than or equal to 0")
	case *seconds > maxGracePeriod:
		r.invalid(at, *seconds, fmt.Sprintf("must be less than or equal to %d", maxGracePeriod))
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
	if source == (corev1.VolumeSource{}) {
		r.errs = append(r.errs, field.Required(at, "a volume is an emptyDir or a hostPath"))
	}
	if source.EmptyDir != nil {
		r.audit(*source.EmptyDir, at.Child("emptyDir"), emptyDirFields)
	}
	if source.HostPath != nil {
		r.hostPath(*source.HostPath, at.Child("hostPath"))
	}
}

// hostPath checks a volume that is a directory of the node.
func (r *findings) hostPath(hostPath corev1.HostPathVolumeSource, at *field.Path) {
	if hostPath.Type != nil && *hostPath.Type == corev1.HostPathUnset {
		hostPath.Type = nil // "" asks for no check, which is what Docker does
	}
	r.audit(hostPath, at, hostPathFields)
	r.path(hostPath.Path, at.Child("path"))
}

// path holds a path against what Docker takes in a mount, which is written
// "source:target". The path starts at the root: Docker reads any other
// source as the name of a volume, and refuses any other target. And it has
// no ":" in it, which would end it early.
func (r *findings) path(path string, at *field.Path) {
	switch {
	case path == "":
		r.errs = append(r.errs, field.Required(at, ""))
	case !strings.HasPrefix(path, "/"):
		r.invalid(at, path, `must start with "/"`)
	case strings.ContainsAny(path, ":\x00"):
		r.invalid(at, path, `must not contain ":"`)
	}
}

func (r *findings) container(c corev1.Container, path *field.Path, volumes map[string]bool) {
	r.audit(c, path, containerFields)
	if !docker.ValidImage(c.Image) {
		r.invalid(path.Child("image"), c.Image, "is not an image reference Docker can read")
	}
	if c.WorkingDir != "" {
		r.path(c.WorkingDir, path.Child("workingDir"))
	}
	r.words(c.Command, path.Child("command"))
	r.words(c.Args, path.Child("args"))
	r.env(c.Env, path.Child("env"))
	r.ports(c.Ports, path.Child("ports"))
	r.security(c.SecurityContext, path.Child("securityContext"))
	r.mounts(c.VolumeMounts, path.Child("volumeMounts"), volumes)
	r.resources(c.Resources, path.Child("resources"))
}

// words refuses a NUL in what a process is started with. No process can be
// given one, and Docker only finds out when it starts the container.
func (r *findings) words(words []string, path *field.Path) {
	for i, word := range words {
		r.word(word, path.Index(i))
	}
}

func (r *findings) word(word string, at *field.Path) {
	if strings.Contains(word, "\x00") {
		r.invalid(at, word, "must not contain a NUL character")
	}
}

// env checks the variables of a container. Docker takes each as
// "NAME=value" and reads the name up to the first "=", so a name with one
// in it would be another name. Values from elsewhere in Kubernetes
// (valueFrom) have nowhere to come from.
func (r *findings) env(vars []corev1.EnvVar, path *field.Path) {
	for i, e := range vars {
		at := path.Index(i)
		if e.ValueFrom != nil {
			r.reject(at.Child("valueFrom"))
		}
		switch {
		case e.Name == "":
			r.errs = append(r.errs, field.Required(at.Child("name"), ""))
		case strings.ContainsAny(e.Name, "=\x00"):
			r.invalid(at.Child("name"), e.Name, `must not contain "="`)
		}
		r.word(e.Value, at.Child("value"))
	}
}

// ports checks the ports a container lists.
func (r *findings) ports(ports []corev1.ContainerPort, path *field.Path) {
	for i, p := range ports {
		at := path.Index(i)
		r.audit(p, at, portFields)
		for _, problem := range validation.IsValidPortNum(int(p.ContainerPort)) {
			r.invalid(at.Child("containerPort"), p.ContainerPort, problem)
		}
	}
}

// security checks who the container runs as. Docker sets the user and the
// group together, as "uid:gid": a group alone would also make the user
// root, whoever the image says it is.
func (r *findings) security(sc *corev1.SecurityContext, path *field.Path) {
	if sc == nil {
		return
	}
	r.audit(*sc, path, securityFields)
	r.id(sc.RunAsUser, path.Child("runAsUser"))
	r.id(sc.RunAsGroup, path.Child("runAsGroup"))
	if sc.RunAsGroup != nil && sc.RunAsUser == nil {
		r.errs = append(r.errs, field.Required(path.Child("runAsUser"), "Docker cannot set runAsGroup without it"))
	}
}

// id holds the number of a user or a group against the range Linux has.
func (r *findings) id(id *int64, at *field.Path) {
	if id != nil && (*id < 0 || *id > maxID) {
		r.invalid(at, *id, validation.InclusiveRangeError(0, maxID))
	}
}

// mounts makes sure each mount names a volume of the Pod and a place of
// its own for it, which is not the root: Docker mounts nothing there.
func (r *findings) mounts(mounts []corev1.VolumeMount, path *field.Path, volumes map[string]bool) {
	taken := map[string]bool{}
	for i, m := range mounts {
		at := path.Index(i)
		r.audit(m, at, mountFields)
		if !volumes[m.Name] {
			r.errs = append(r.errs, field.NotFound(at.Child("name"), m.Name))
		}
		r.path(m.MountPath, at.Child("mountPath"))
		switch {
		case m.MountPath == "/":
			r.invalid(at.Child("mountPath"), m.MountPath, "must not be the root")
		case taken[m.MountPath]:
			r.errs = append(r.errs, field.Duplicate(at.Child("mountPath"), m.MountPath))
		}
		taken[m.MountPath] = true
	}
}

// resources allows only CPU and memory: Docker has no notion of the others.
// A limit must also be one Docker's API can carry, and Docker accepts.
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
	r.atLeast(res.Limits.Memory(), minMemory, path.Child("limits").Key("memory"))
}

func (r *findings) atMost(limit, most *resource.Quantity, at *field.Path) {
	if limit.Cmp(*most) > 0 {
		r.invalid(at, limit.String(), "must be less than or equal to "+most.String()+", the most Docker can be told")
	}
}

// atLeast holds a limit that is set against the least Docker accepts. A
// limit of zero is no limit, and a negative one is upstream's to refuse.
func (r *findings) atLeast(limit, least *resource.Quantity, at *field.Path) {
	if limit.Sign() > 0 && limit.Cmp(*least) < 0 {
		r.invalid(at, limit.String(), "must be greater than or equal to "+least.String()+", the least Docker accepts")
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
