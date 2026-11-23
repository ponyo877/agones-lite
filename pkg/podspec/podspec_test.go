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
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	"github.com/ponyo877/agones-lite/pkg/agones"
	"github.com/ponyo877/agones-lite/pkg/docker"
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
		t.Skip("the Agones submodule is not checked out")
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

func TestGracePeriod(t *testing.T) {
	assert.Equal(t, 30*time.Second, GracePeriod(server(nil)))

	five := int64(5)
	gs := server(func(p *corev1.PodSpec) { p.TerminationGracePeriodSeconds = &five })
	assert.Equal(t, 5*time.Second, GracePeriod(gs))
}
