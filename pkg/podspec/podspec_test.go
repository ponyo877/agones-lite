package podspec

import (
	"os"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	"github.com/ponyo877/agones-lite/pkg/agones"
	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/test/testenv"
)

func server(change func(*corev1.PodSpec)) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gs", UID: "uid-1"}}
	gs.Spec.Container = "game"
	gs.Spec.SdkServer = agonesv1.SdkServer{GRPCPort: 9357, HTTPPort: 9358}
	gs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "game", Image: "example/game:1"}}
	if change != nil {
		change(&gs.Spec.Template.Spec)
	}
	return gs
}

func TestCheck(t *testing.T) {
	yes := true
	tests := map[string]struct {
		change   func(*corev1.PodSpec)
		warnings []string
		errors   []string
	}{
		"a plain container is fine": {},
		"scheduling fields are ignored with a warning": {
			change: func(p *corev1.PodSpec) {
				p.NodeSelector = map[string]string{"pool": "games"}
				p.ServiceAccountName = "game"
			},
			warnings: []string{
				"spec.template.spec.nodeSelector is ignored",
				"spec.template.spec.serviceAccountName is ignored",
			},
		},
		"init containers are rejected": {
			change: func(p *corev1.PodSpec) { p.InitContainers = []corev1.Container{{Name: "init"}} },
			errors: []string{"spec.template.spec.initContainers"},
		},
		"host networking is rejected": {
			change: func(p *corev1.PodSpec) { p.HostNetwork = true },
			errors: []string{"spec.template.spec.hostNetwork"},
		},
		"probes are rejected": {
			change: func(p *corev1.PodSpec) { p.Containers[0].LivenessProbe = &corev1.Probe{} },
			errors: []string{"spec.template.spec.containers[0].livenessProbe"},
		},
		"env from a secret is rejected": {
			change: func(p *corev1.PodSpec) {
				p.Containers[0].Env = []corev1.EnvVar{{Name: "A", Value: "1"}, {Name: "B", ValueFrom: &corev1.EnvVarSource{}}}
			},
			errors: []string{"spec.template.spec.containers[0].env[1].valueFrom"},
		},
		"privileged is rejected, runAsUser is not": {
			change: func(p *corev1.PodSpec) {
				uid := int64(1000)
				p.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &uid, Privileged: &yes}
			},
			errors: []string{"spec.template.spec.containers[0].securityContext.privileged"},
		},
		"only emptyDir and hostPath volumes": {
			change: func(p *corev1.PodSpec) {
				p.Volumes = []corev1.Volume{
					{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
				}
			},
			errors: []string{"spec.template.spec.volumes[1].configMap"},
		},
		"only cpu and memory": {
			change: func(p *corev1.PodSpec) {
				p.Containers[0].Resources.Limits = corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
					"nvidia.com/gpu":   resource.MustParse("1"),
				}
			},
			errors: []string{"spec.template.spec.containers[0].resources.limits[nvidia.com/gpu]"},
		},
		"an empty list or map asks for nothing": {
			change: func(p *corev1.PodSpec) {
				p.InitContainers = []corev1.Container{}
				p.ImagePullSecrets = []corev1.LocalObjectReference{}
				p.NodeSelector = map[string]string{}
				p.Containers[0].EnvFrom = []corev1.EnvFromSource{}
			},
		},
		"a negative grace period is rejected": {
			change: func(p *corev1.PodSpec) { p.TerminationGracePeriodSeconds = new(int64(-1)) },
			errors: []string{"spec.template.spec.terminationGracePeriodSeconds"},
		},
		"limits must fit in what Docker takes": {
			change: func(p *corev1.PodSpec) {
				p.Containers[0].Resources.Limits = corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10000000000"),
					corev1.ResourceMemory: resource.MustParse("1e19"),
				}
			},
			errors: []string{
				"spec.template.spec.containers[0].resources.limits[cpu]",
				"spec.template.spec.containers[0].resources.limits[memory]",
			},
		},
		"the largest limits Docker takes are fine": {
			change: func(p *corev1.PodSpec) {
				p.Containers[0].Resources.Limits = corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("9223372036854m"),
					corev1.ResourceMemory: resource.MustParse("9223372036854775807"),
				}
			},
		},
		"a mount needs a volume and a path": {
			change: func(p *corev1.PodSpec) {
				p.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
				p.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "nope", MountPath: "/data"}, {Name: "tmp"}}
			},
			errors: []string{
				"spec.template.spec.containers[0].volumeMounts[0].name",
				"spec.template.spec.containers[0].volumeMounts[1].mountPath",
			},
		},
		"a volume needs a source, and a name of its own": {
			change: func(p *corev1.PodSpec) {
				tmp := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
				p.Volumes = []corev1.Volume{
					{Name: "nothing"},
					{Name: "nowhere", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{}}},
					{Name: "tmp", VolumeSource: tmp},
					{Name: "tmp", VolumeSource: tmp},
					{Name: "Not A Name", VolumeSource: tmp},
				}
			},
			errors: []string{
				"spec.template.spec.volumes[0]",
				"spec.template.spec.volumes[1].hostPath.path",
				"spec.template.spec.volumes[3].name",
				"spec.template.spec.volumes[4].name",
			},
		},
		"a container needs a name of its own, and not the sidecar's": {
			change: func(p *corev1.PodSpec) {
				p.Containers = append(p.Containers,
					corev1.Container{Name: SidecarName, Image: "example/logs:1"},
					corev1.Container{Name: "game", Image: "example/logs:1"},
					corev1.Container{Name: "Not A Name", Image: "example/logs:1"},
				)
			},
			errors: []string{
				"spec.template.spec.containers[1].name",
				"spec.template.spec.containers[2].name",
				"spec.template.spec.containers[3].name",
			},
		},
		"a port of a container is a note, and cannot publish": {
			change: func(p *corev1.PodSpec) {
				p.Containers[0].Ports = []corev1.ContainerPort{
					{Name: "game", ContainerPort: 7654, Protocol: corev1.ProtocolUDP},
					{ContainerPort: 7654, HostPort: 7654},
					{ContainerPort: 7654, HostIP: "192.0.2.1"},
					{ContainerPort: 70000},
				}
			},
			errors: []string{
				"spec.template.spec.containers[0].ports[1].hostPort",
				"spec.template.spec.containers[0].ports[2].hostIP",
				"spec.template.spec.containers[0].ports[3].containerPort",
			},
		},
		"what Docker's volumes do not have is ignored with a warning": {
			change: func(p *corev1.PodSpec) {
				limit, kind := resource.MustParse("1Gi"), corev1.HostPathDirectory
				p.Volumes = []corev1.Volume{
					{Name: "fast", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &limit}}},
					{Name: "maps", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/maps", Type: &kind}}},
					{Name: "any", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/any", Type: new(corev1.HostPathUnset)}}},
				}
			},
			warnings: []string{
				"spec.template.spec.volumes[0].emptyDir.medium is ignored",
				"spec.template.spec.volumes[0].emptyDir.sizeLimit is ignored",
				"spec.template.spec.volumes[1].hostPath.type is ignored",
			},
		},
		"a path starts at the root and has no colon": {
			change: func(p *corev1.PodSpec) {
				tmp := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
				p.Volumes = []corev1.Volume{
					{Name: "relative", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "maps"}}},
					{Name: "colon", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/a:b"}}},
					{Name: "tmp", VolumeSource: tmp},
				}
				p.Containers[0].WorkingDir = "srv"
				p.Containers[0].VolumeMounts = []corev1.VolumeMount{
					{Name: "tmp", MountPath: "data"}, {Name: "tmp", MountPath: "/a:ro"}, {Name: "tmp", MountPath: "/"},
					{Name: "tmp", MountPath: "/data"}, {Name: "tmp", MountPath: "/data"},
				}
			},
			errors: []string{
				"spec.template.spec.volumes[0].hostPath.path",
				"spec.template.spec.volumes[1].hostPath.path",
				"spec.template.spec.containers[0].workingDir",
				"spec.template.spec.containers[0].volumeMounts[0].mountPath",
				"spec.template.spec.containers[0].volumeMounts[1].mountPath",
				"spec.template.spec.containers[0].volumeMounts[2].mountPath",
				"spec.template.spec.containers[0].volumeMounts[4].mountPath",
			},
		},
		"a variable has a name without =, and nothing a process is given holds a NUL": {
			change: func(p *corev1.PodSpec) {
				p.Containers[0].Env = []corev1.EnvVar{{Value: "x"}, {Name: "A=B", Value: "x"}, {Name: "OK", Value: "a\x00b"}, {Name: "FINE", Value: "a=b"}}
				p.Containers[0].Command = []string{"/game", "a\x00b"}
				p.Containers[0].Args = []string{"\x00"}
			},
			errors: []string{
				"spec.template.spec.containers[0].env[0].name",
				"spec.template.spec.containers[0].env[1].name",
				"spec.template.spec.containers[0].env[2].value",
				"spec.template.spec.containers[0].command[1]",
				"spec.template.spec.containers[0].args[0]",
			},
		},
		"a user and a group are numbers Linux has, and Docker sets them together": {
			change: func(p *corev1.PodSpec) {
				p.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: new(int64(-1)), RunAsGroup: new(int64(1 << 31))}
				p.Containers = append(p.Containers, corev1.Container{Name: "logs", Image: "example/logs:1",
					SecurityContext: &corev1.SecurityContext{RunAsGroup: new(int64(1000))}})
			},
			errors: []string{
				"spec.template.spec.containers[0].securityContext.runAsUser",
				"spec.template.spec.containers[0].securityContext.runAsGroup",
				"spec.template.spec.containers[1].securityContext.runAsUser",
			},
		},
		"Docker gives a container 6Mi of memory or more, and takes no more than 68 years to stop it": {
			change: func(p *corev1.PodSpec) {
				p.TerminationGracePeriodSeconds = new(int64(1 << 31))
				p.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6291455")}
			},
			errors: []string{
				"spec.template.spec.terminationGracePeriodSeconds",
				"spec.template.spec.containers[0].resources.limits[memory]",
			},
		},
		"the least and the longest Docker takes are fine": {
			change: func(p *corev1.PodSpec) {
				p.TerminationGracePeriodSeconds = new(int64(1<<31 - 1))
				p.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Mi"), corev1.ResourceCPU: resource.MustParse("1m")}
				p.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: new(int64(0)), RunAsGroup: new(int64(1<<31 - 1))}
			},
		},
		"an image Docker cannot parse is rejected": {
			change: func(p *corev1.PodSpec) {
				p.Containers[0].Image = "alpine:"
				p.Containers = append(p.Containers, corev1.Container{Name: "none"}, corev1.Container{Name: "caps", Image: "Example/Game"})
			},
			errors: []string{
				"spec.template.spec.containers[0].image",
				"spec.template.spec.containers[1].image",
				"spec.template.spec.containers[2].image",
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			warnings, errs := Check(&server(tc.change).Spec, field.NewPath("spec"))
			assert.ElementsMatch(t, tc.warnings, warnings)
			var fields []string
			for _, e := range errs {
				fields = append(fields, e.Field)
			}
			assert.ElementsMatch(t, tc.errors, fields)
		})
	}
}

// The upstream sample must pass untouched: that is the point of the subset.
func TestCheckAcceptsTheUpstreamSample(t *testing.T) {
	require.NoError(t, agones.Setup())
	raw, err := os.ReadFile("../../third_party/agones/examples/simple-game-server/gameserver.yaml")
	if err != nil {
		testenv.Missing(t, "the Agones submodule is not checked out")
	}
	gs := &agonesv1.GameServer{}
	require.NoError(t, yaml.UnmarshalStrict(raw, gs))
	gs.ApplyDefaults()

	warnings, errs := Check(&gs.Spec, field.NewPath("spec"))

	assert.Empty(t, warnings)
	assert.Empty(t, errs)
}

func TestContainers(t *testing.T) {
	gs := server(func(p *corev1.PodSpec) {
		uid := int64(1000)
		p.Containers[0].Env = []corev1.EnvVar{{Name: "MODE", Value: "ranked"}}
		p.Containers[0].Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		}
		p.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &uid}
		p.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}, {Name: "maps", MountPath: "/maps", ReadOnly: true}}
		p.Volumes = []corev1.Volume{
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "maps", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/maps"}}},
		}
		p.Containers = append(p.Containers, corev1.Container{Name: "logs", Image: "example/logs:1"})
	})
	gs.Spec.Ports = []agonesv1.GameServerPort{
		{Name: "game", PortPolicy: agonesv1.Dynamic, ContainerPort: 7654, HostPort: 7000, Protocol: corev1.ProtocolUDP},
		{Name: "debug", PortPolicy: agonesv1.None, ContainerPort: 6060, Protocol: corev1.ProtocolTCP},
	}

	specs := Containers(gs, Sidecar{Image: "sidecar:dev", AgentAddress: "host.docker.internal:9447", Token: "secret"})

	require.Len(t, specs, 3)
	sidecar, game, logs := specs[0], specs[1], specs[2]

	assert.Equal(t, "agones-lite_default_gs_agones-gameserver-sidecar", sidecar.Name)
	assert.Equal(t, "sidecar:dev", sidecar.Image)
	assert.Equal(t, []docker.Port{{Container: 7654, Host: 7000, Protocol: "udp"}}, sidecar.Ports, "None ports are not published")
	assert.Empty(t, sidecar.ShareNetworkWith, "the sidecar owns the network")
	assert.Contains(t, sidecar.Env, "AGONES_LITE_AGENT=host.docker.internal:9447")
	assert.Contains(t, sidecar.Env, "AGONES_LITE_GAMESERVER=default/gs")
	assert.Contains(t, sidecar.Env, "AGONES_LITE_TOKEN=secret")
	assert.Equal(t, RoleSidecar, sidecar.Labels[LabelRole])
	assert.Equal(t, "secret", sidecar.Labels[LabelToken])

	assert.Equal(t, "agones-lite_default_gs_game", game.Name)
	assert.Equal(t, sidecar.Name, game.ShareNetworkWith)
	assert.Empty(t, game.Ports, "only the network's owner publishes ports")
	assert.Equal(t, []string{"MODE=ranked", "AGONES_SDK_GRPC_PORT=9357", "AGONES_SDK_HTTP_PORT=9358"}, game.Env)
	assert.Equal(t, int64(500_000_000), game.NanoCPUs)
	assert.Equal(t, int64(64<<20), game.MemoryBytes)
	assert.Equal(t, "1000", game.User)
	assert.Equal(t, []string{"agones-lite_default_gs_tmp:/tmp", "/srv/maps:/maps:ro"}, game.Binds)
	assert.Equal(t, RoleGame, game.Labels[LabelRole])
	assert.Empty(t, game.Labels[LabelToken], "the game container must not learn the sidecar's token")

	assert.Equal(t, RoleOther, logs.Labels[LabelRole])
	assert.Equal(t, "uid-1", logs.Labels[LabelUID])
}

// The agent removes a GameServer's volumes by the start of their names, so
// the prefix of one GameServer must never start the names of another's.
// The separator sees to that: no name the management API admits holds it.
func TestAPrefixBelongsToOneGameServer(t *testing.T) {
	assert.Equal(t, "agones-lite_default_gs_", Prefix("default", "gs"))

	assert.NotEmpty(t, validation.IsDNS1123Label("default"+separator+"gs"), "a namespace cannot hold the separator")
	assert.NotEmpty(t, validation.IsDNS1123Subdomain("gs"+separator+"x"), "a name cannot hold the separator")
	for _, neighbour := range []string{"gs-x", "gs.x", "gsx"} {
		assert.NotContains(t, Prefix("default", neighbour), Prefix("default", "gs"), neighbour)
	}
}

// Linux enforces no CPU limit below a hundredth of a core. The kubelet
// rounds a smaller one up; Docker would pass it on, and the container
// would fail to start.
func TestASmallCPULimitIsRaisedToTheLeastLinuxEnforces(t *testing.T) {
	tests := map[string]int64{"1m": 10_000_000, "9m": 10_000_000, "10m": 10_000_000, "11m": 11_000_000, "2": 2_000_000_000, "0": 0}
	for limit, want := range tests {
		gs := server(func(p *corev1.PodSpec) {
			p.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limit)}
		})

		assert.Equal(t, want, Containers(gs, Sidecar{})[1].NanoCPUs, "a limit of %s", limit)
	}
	assert.Zero(t, Containers(server(nil), Sidecar{})[1].NanoCPUs, "no limit")
}

func TestGracePeriod(t *testing.T) {
	assert.Equal(t, 30*time.Second, GracePeriod(server(nil)))

	five := int64(5)
	gs := server(func(p *corev1.PodSpec) { p.TerminationGracePeriodSeconds = &five })
	assert.Equal(t, 5*time.Second, GracePeriod(gs))
}
