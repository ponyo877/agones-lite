package podspec

import (
	"fmt"
	"strings"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/ponyo877/agones-lite/pkg/docker"
)

// Labels on every container of a GameServer. The agent finds its containers
// by them, so nothing about a GameServer lives only in the agent's memory.
const (
	LabelNamespace  = "agones-lite.namespace"
	LabelGameServer = "agones-lite.gameserver"
	LabelUID        = "agones-lite.uid"
	// LabelNode names the node whose agent owns the container. An agent only
	// touches its own, so two agents can share one Docker daemon.
	LabelNode = "agones-lite.node"
	LabelRole = "agones-lite.role"
	// LabelToken is on the sidecar only: the secret it shows the agent.
	LabelToken = "agones-lite.token"
)

// Roles a container can have within a GameServer.
const (
	RoleSidecar = "sidecar" // owns the network; the SDK server
	RoleGame    = "game"    // the container named by spec.container
	RoleOther   = "other"   // any further container of the Pod
)

// SidecarName is the sidecar's container name, the same as upstream's.
const SidecarName = "agones-gameserver-sidecar"

// Environment variables the sidecar reads.
const (
	EnvAgent      = "AGONES_LITE_AGENT"
	EnvGameServer = "AGONES_LITE_GAMESERVER"
	EnvToken      = "AGONES_LITE_TOKEN"
	// The SDK port variables are upstream's, also given to the game containers.
	EnvGRPCPort = "AGONES_SDK_GRPC_PORT"
	EnvHTTPPort = "AGONES_SDK_HTTP_PORT"
)

const defaultGracePeriod = 30 * time.Second

// Sidecar is what the agent decides about a GameServer's sidecar.
type Sidecar struct {
	Image string
	// AgentAddress is where the sidecar reaches the agent from inside a container.
	AgentAddress string
	// Token is the secret the sidecar shows the agent.
	Token string
}

// Containers returns the containers that make up gs.
//
// The sidecar comes first. It owns the network and the published ports; the
// containers of the Pod join its network, so all of them share one localhost,
// exactly as the containers of a Pod do.
func Containers(gs *agonesv1.GameServer, sidecar Sidecar) []docker.Spec {
	specs := []docker.Spec{sidecarSpec(gs, sidecar)}
	for _, c := range gs.Spec.Template.Spec.Containers {
		specs = append(specs, gameSpec(gs, c))
	}
	return specs
}

// Name returns the Docker name of one container of gs.
func Name(gs *agonesv1.GameServer, container string) string {
	return VolumePrefix(gs) + container
}

// VolumePrefix starts the name of every Docker object that belongs to gs.
func VolumePrefix(gs *agonesv1.GameServer) string {
	return Prefix(gs.Namespace, gs.Name)
}

// Prefix is VolumePrefix for when only the GameServer's name is known.
func Prefix(namespace, name string) string {
	return "agones-lite_" + namespace + "_" + name + "_"
}

// GracePeriod is how long the containers get to exit before being killed.
func GracePeriod(gs *agonesv1.GameServer) time.Duration {
	if seconds := gs.Spec.Template.Spec.TerminationGracePeriodSeconds; seconds != nil {
		return time.Duration(*seconds) * time.Second
	}
	return defaultGracePeriod
}

func sidecarSpec(gs *agonesv1.GameServer, sidecar Sidecar) docker.Spec {
	labels := labels(gs, RoleSidecar)
	labels[LabelToken] = sidecar.Token
	return docker.Spec{
		Name:  Name(gs, SidecarName),
		Image: sidecar.Image,
		Env: append(sdkPorts(gs),
			EnvAgent+"="+sidecar.AgentAddress,
			EnvGameServer+"="+gs.Namespace+"/"+gs.Name,
			EnvToken+"="+sidecar.Token,
		),
		Labels: labels,
		Ports:  published(gs),
		// Resolves to the host on Linux, Docker Desktop and Colima alike.
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
	}
}

func gameSpec(gs *agonesv1.GameServer, c corev1.Container) docker.Spec {
	role := RoleOther
	if c.Name == gs.Spec.Container {
		role = RoleGame
	}
	spec := docker.Spec{
		Name:             Name(gs, c.Name),
		Image:            c.Image,
		Command:          c.Command,
		Args:             c.Args,
		Env:              append(env(c), sdkPorts(gs)...),
		WorkingDir:       c.WorkingDir,
		Labels:           labels(gs, role),
		ShareNetworkWith: Name(gs, SidecarName),
		NanoCPUs:         c.Resources.Limits.Cpu().MilliValue() * 1_000_000,
		MemoryBytes:      c.Resources.Limits.Memory().Value(),
		Binds:            binds(gs, c),
	}
	applySecurity(&spec, c.SecurityContext)
	return spec
}

func labels(gs *agonesv1.GameServer, role string) map[string]string {
	return map[string]string{
		LabelNamespace:  gs.Namespace,
		LabelGameServer: gs.Name,
		LabelUID:        string(gs.UID),
		LabelNode:       gs.Status.NodeName,
		LabelRole:       role,
	}
}

// sdkPorts tells a container where the SDK server listens, as upstream does.
func sdkPorts(gs *agonesv1.GameServer) []string {
	return []string{
		fmt.Sprintf("%s=%d", EnvGRPCPort, gs.Spec.SdkServer.GRPCPort),
		fmt.Sprintf("%s=%d", EnvHTTPPort, gs.Spec.SdkServer.HTTPPort),
	}
}

func env(c corev1.Container) []string {
	out := make([]string, 0, len(c.Env))
	for _, e := range c.Env {
		out = append(out, e.Name+"="+e.Value)
	}
	return out
}

// published lists the ports players reach from outside the node.
func published(gs *agonesv1.GameServer) []docker.Port {
	var out []docker.Port
	for _, p := range gs.Spec.Ports {
		if p.PortPolicy == agonesv1.None {
			continue
		}
		out = append(out, docker.Port{
			Container: p.ContainerPort,
			Host:      p.HostPort,
			Protocol:  strings.ToLower(string(p.Protocol)),
		})
	}
	return out
}

// binds mounts the container's volumes. An emptyDir becomes a Docker volume
// named after the GameServer, so containers of one GameServer can share it.
func binds(gs *agonesv1.GameServer, c corev1.Container) []string {
	sources := map[string]string{}
	for _, v := range gs.Spec.Template.Spec.Volumes {
		switch {
		case v.HostPath != nil:
			sources[v.Name] = v.HostPath.Path
		case v.EmptyDir != nil:
			sources[v.Name] = VolumePrefix(gs) + v.Name
		}
	}
	var out []string
	for _, m := range c.VolumeMounts {
		bind := sources[m.Name] + ":" + m.MountPath
		if m.ReadOnly {
			bind += ":ro"
		}
		out = append(out, bind)
	}
	return out
}

func applySecurity(spec *docker.Spec, sc *corev1.SecurityContext) {
	if sc == nil {
		return
	}
	if sc.RunAsUser != nil {
		spec.User = fmt.Sprint(*sc.RunAsUser)
	}
	if sc.RunAsGroup != nil && spec.User != "" {
		spec.User += fmt.Sprintf(":%d", *sc.RunAsGroup)
	}
	spec.ReadOnlyRootFS = sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem
}
