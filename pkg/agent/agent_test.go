package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/podspec"
)

// fakeDocker records what the agent asks of Docker, in order.
type fakeDocker struct {
	calls      []string
	containers []docker.Container
	images     map[string]bool
	failCreate bool
}

func (f *fakeDocker) List(context.Context, string) ([]docker.Container, error) {
	return f.containers, nil
}

func (f *fakeDocker) HasImage(_ context.Context, image string) (bool, error) {
	return f.images[image], nil
}

func (f *fakeDocker) Pull(_ context.Context, image string) error {
	f.calls = append(f.calls, "pull "+image)
	return nil
}

func (f *fakeDocker) Create(_ context.Context, spec docker.Spec) (string, error) {
	if f.failCreate {
		return "", errors.New("no such image")
	}
	f.calls = append(f.calls, "create "+spec.Name)
	return spec.Name, nil
}

func (f *fakeDocker) Start(_ context.Context, id string) error {
	f.calls = append(f.calls, "start "+id)
	return nil
}

func (f *fakeDocker) Stop(_ context.Context, id string, grace time.Duration) error {
	f.calls = append(f.calls, fmt.Sprintf("stop %s %s", id, grace))
	return nil
}

func (f *fakeDocker) Remove(_ context.Context, id string) error {
	f.calls = append(f.calls, "remove "+id)
	return nil
}

func (f *fakeDocker) Logs(_ context.Context, id string, _ bool, _ int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("log of " + id)), nil
}

func (f *fakeDocker) RemoveVolumes(_ context.Context, prefix string) error {
	f.calls = append(f.calls, "remove volumes "+prefix)
	return nil
}

// fakeControlPlane records what the agent reports.
type fakeControlPlane struct {
	events []gameservers.Event
}

func (f *fakeControlPlane) Register(*litev1.Node) error                    { return nil }
func (f *fakeControlPlane) Assigned(string) ([]*agonesv1.GameServer, bool) { return nil, true }
func (f *fakeControlPlane) Changes() (<-chan struct{}, func())             { return nil, func() {} }
func (f *fakeControlPlane) SetMetadata(string, string, map[string]string, map[string]string) error {
	return nil
}

func (f *fakeControlPlane) Report(r gameservers.Report) error {
	f.events = append(f.events, r.Event)
	return nil
}

func newAgent(d *fakeDocker) (*Agent, *fakeControlPlane) {
	cp := &fakeControlPlane{}
	return New(Config{NodeName: "node-1", SidecarImage: "sidecar:dev", SidecarDial: "host.docker.internal:9447"}, cp, d), cp
}

func placed() *agonesv1.GameServer {
	gs := server("gs", agonesv1.GameServerStateCreating)
	gs.Spec.Container = "game"
	gs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "game", Image: "example/game:1"}}
	return gs
}

var key = Key{Namespace: "default", Name: "gs"}

func TestCreateBringsTheContainersUpInOrder(t *testing.T) {
	d := &fakeDocker{images: map[string]bool{"sidecar:dev": true}}
	a, cp := newAgent(d)

	require.NoError(t, a.execute(context.Background(), Action{Kind: Create, Key: key, GameServer: placed()}))

	assert.Equal(t, []string{
		"pull example/game:1", // the sidecar image is already there
		"create agones-lite_default_gs_agones-gameserver-sidecar",
		"create agones-lite_default_gs_game",
		"start agones-lite_default_gs_agones-gameserver-sidecar", // the network's owner goes first
		"start agones-lite_default_gs_game",
	}, d.calls)
	assert.Equal(t, []gameservers.Event{gameservers.ContainersCreated, gameservers.ContainersRunning}, cp.events)
	assert.NotEmpty(t, a.sidecars.tokens[key], "the sidecar must be let in before it starts")
}

func TestCreateReportsFailure(t *testing.T) {
	a, cp := newAgent(&fakeDocker{images: map[string]bool{"sidecar:dev": true, "example/game:1": true}, failCreate: true})

	err := a.execute(context.Background(), Action{Kind: Create, Key: key, GameServer: placed()})

	assert.Error(t, err)
	assert.Equal(t, []gameservers.Event{gameservers.CreateFailed}, cp.events)
}

func TestRemoveStopsTheGameBeforeItsSidecar(t *testing.T) {
	d := &fakeDocker{}
	a, cp := newAgent(d)
	grace := int64(5)
	gs := placed()
	gs.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace

	err := a.execute(context.Background(), Action{Kind: Remove, Key: key, GameServer: gs, Containers: running("gs")})

	require.NoError(t, err)
	assert.Equal(t, []string{
		"stop gs-game 5s", // the game can still reach the SDK while it shuts down
		"remove gs-sidecar",
		"remove gs-game",
		"remove volumes agones-lite_default_gs_",
	}, d.calls)
	assert.Equal(t, []gameservers.Event{gameservers.ContainersRemoved}, cp.events)
}

func TestDiscardReportsNothing(t *testing.T) {
	d := &fakeDocker{}
	a, cp := newAgent(d)

	require.NoError(t, a.execute(context.Background(), Action{Kind: Discard, Key: key, Containers: running("gs")}))

	assert.Contains(t, d.calls, "remove gs-game")
	assert.Empty(t, cp.events)
}

func TestNeedsPull(t *testing.T) {
	tests := []struct {
		policy  corev1.PullPolicy
		image   string
		present bool
		want    bool
	}{
		{corev1.PullAlways, "game:1", true, true},
		{corev1.PullNever, "game:1", false, false},
		{corev1.PullIfNotPresent, "game:1", true, false},
		{corev1.PullIfNotPresent, "game:1", false, true},
		// Unset follows Kubernetes: ":latest" is always pulled.
		{"", "game:1", true, false},
		{"", "game:1", false, true},
		{"", "game:latest", true, true},
		{"", "game", true, true},
	}
	for _, tc := range tests {
		got := needsPull(tc.policy, tc.image, tc.present)
		assert.Equal(t, tc.want, got, "%q %s present=%v", tc.policy, tc.image, tc.present)
	}
}

func TestClaimKeepsOneActionPerGameServer(t *testing.T) {
	a, _ := newAgent(&fakeDocker{})

	assert.True(t, a.claim(key))
	assert.False(t, a.claim(key), "a second action must wait for the first")
	a.release(key)
	assert.True(t, a.claim(key))
}

func TestSidecarTokensComeBackFromLabels(t *testing.T) {
	a, _ := newAgent(&fakeDocker{})
	sidecar := container("gs", podspec.RoleSidecar, "running", 0)
	sidecar.Labels[podspec.LabelToken] = "secret"

	// As after a restart of the agent: nothing in memory, containers running.
	a.sidecars.update(nil, []docker.Container{sidecar})

	assert.Equal(t, "secret", a.sidecars.tokens[key])
}

func TestLogsReadTheMainContainerUnlessAnotherIsNamed(t *testing.T) {
	sidecar := container("gs", podspec.RoleSidecar, "running", 0)
	sidecar.Name = "agones-lite_default_gs_agones-gameserver-sidecar"
	game := container("gs", podspec.RoleGame, "running", 0)
	game.Name = "agones-lite_default_gs_game"
	a, _ := newAgent(&fakeDocker{containers: []docker.Container{sidecar, game, container("other", podspec.RoleGame, "running", 0)}})

	read := func(req LogRequest) string {
		stream, err := a.Logs(context.Background(), req)
		require.NoError(t, err)
		text, err := io.ReadAll(stream)
		require.NoError(t, err)
		return string(text)
	}
	assert.Equal(t, "log of gs-game", read(LogRequest{Namespace: "default", Name: "gs"}))
	assert.Equal(t, "log of gs-sidecar", read(LogRequest{Namespace: "default", Name: "gs", Container: podspec.SidecarName}))

	_, err := a.Logs(context.Background(), LogRequest{Namespace: "default", Name: "missing"})
	assert.Error(t, err)
}
