package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
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
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// fakeDocker records what the agent asks of Docker, in order.
type fakeDocker struct {
	calls      []string
	containers []docker.Container
	images     map[string]bool
	taken      []string // container names in use
	failCreate bool
	failStart  bool
	onList     func() // runs while the agent waits for the list
	onCreate   func() // runs while the agent waits for a container to be created
}

func (f *fakeDocker) List(context.Context, string) ([]docker.Container, error) {
	if f.onList != nil {
		f.onList()
	}
	return f.containers, nil
}

func (f *fakeDocker) HasImage(_ context.Context, image string) (bool, error) {
	return f.images[image], nil
}

func (f *fakeDocker) Pull(_ context.Context, image string) error {
	f.calls = append(f.calls, "pull "+image)
	return nil
}

func (f *fakeDocker) Create(ctx context.Context, spec docker.Spec) (string, error) {
	if f.failCreate {
		return "", errors.New("no such image")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.onCreate != nil {
		f.onCreate()
	}
	f.calls = append(f.calls, "create "+spec.Name)
	if slices.Contains(f.taken, spec.Name) {
		inUse := &docker.Error{Status: http.StatusConflict, Message: "Conflict. The container name is already in use"}
		return "", fmt.Errorf("create %s: %w", spec.Name, inUse)
	}
	f.taken = append(f.taken, spec.Name)
	return spec.Name, nil
}

func (f *fakeDocker) Start(_ context.Context, id string) error {
	if f.failStart {
		return errors.New("no such file or directory")
	}
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
	placed  []*agonesv1.GameServer
	events  []gameservers.Event
	reserve time.Duration // of the last report
}

func (f *fakeControlPlane) Register(*litev1.Node) error                    { return nil }
func (f *fakeControlPlane) Assigned(string) ([]*agonesv1.GameServer, bool) { return f.placed, true }
func (f *fakeControlPlane) Changes() (<-chan struct{}, func())             { return nil, func() {} }
func (f *fakeControlPlane) SetMetadata(string, string, map[string]string, map[string]string) error {
	return nil
}

func (f *fakeControlPlane) UpdateCounts(string, string, string, []*sidecarv1.CounterUpdate, []*sidecarv1.ListUpdate) (*agonesv1.GameServer, error) {
	return nil, nil
}

func (f *fakeControlPlane) Report(r gameservers.Report) error {
	f.events = append(f.events, r.Event)
	f.reserve = r.Reserve
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

func TestStartFinishesWhatACreateLeftUnstarted(t *testing.T) {
	d := &fakeDocker{}
	a, cp := newAgent(d)
	unstarted := []docker.Container{
		container("gs", podspec.RoleSidecar, "created", 0),
		container("gs", podspec.RoleGame, "created", 0),
	}

	require.NoError(t, a.execute(context.Background(), Action{Kind: Start, Key: key, Containers: unstarted}))

	assert.Equal(t, []string{"start gs-sidecar", "start gs-game"}, d.calls)
	assert.Equal(t, []gameservers.Event{gameservers.ContainersRunning}, cp.events)
}

func TestStartReportsFailure(t *testing.T) {
	a, cp := newAgent(&fakeDocker{failStart: true})

	err := a.execute(context.Background(), Action{Kind: Start, Key: key, Containers: running("gs")})

	assert.Error(t, err)
	assert.Equal(t, []gameservers.Event{gameservers.CreateFailed}, cp.events, "or it would be tried again for ever")
}

func TestACreateCutShortByShutdownIsNotReportedAsFailed(t *testing.T) {
	a, cp := newAgent(&fakeDocker{images: map[string]bool{"sidecar:dev": true, "example/game:1": true}})
	stopping, stop := context.WithCancel(context.Background())
	stop()

	err := a.execute(stopping, Action{Kind: Create, Key: key, GameServer: placed()})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, cp.events, "Error is final, and the next agent can still bring the GameServer up")
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

// settle waits until the actions the agent started have finished.
func settle(t *testing.T, a *Agent) {
	t.Helper()
	require.Eventually(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.busy) == 0
	}, 5*time.Second, time.Millisecond)
}

func TestAPassThatListedBeforeACreateWasDoneDoesNotCreateAgain(t *testing.T) {
	d := &fakeDocker{images: map[string]bool{"sidecar:dev": true, "example/game:1": true}}
	a, cp := newAgent(d)
	cp.placed = []*agonesv1.GameServer{placed()}
	begun, finish := make(chan struct{}), make(chan struct{})
	d.onCreate = sync.OnceFunc(func() { close(begun); <-finish })

	a.reconcile(context.Background()) // starts the create, and Docker takes its time
	<-begun
	// The next pass asks Docker for its list. The answer is put together
	// before any container exists and arrives when the create is done.
	d.onList = sync.OnceFunc(func() { close(finish); settle(t, a) })
	a.reconcile(context.Background())
	settle(t, a)

	assert.Equal(t, []string{
		"create agones-lite_default_gs_agones-gameserver-sidecar",
		"create agones-lite_default_gs_game",
		"start agones-lite_default_gs_agones-gameserver-sidecar",
		"start agones-lite_default_gs_game",
	}, d.calls, "the list said there was nothing, but it was too old to say")
	assert.Equal(t, []gameservers.Event{gameservers.ContainersCreated, gameservers.ContainersRunning}, cp.events)
}

func TestACreateThatFindsItsContainersThereAlreadyFailsNothing(t *testing.T) {
	sidecar := "agones-lite_default_gs_agones-gameserver-sidecar"
	images := map[string]bool{"sidecar:dev": true, "example/game:1": true}
	create := Action{Kind: Create, Key: key, GameServer: placed()}

	// Whatever led to a second create, the first one's containers are there.
	a, cp := newAgent(&fakeDocker{images: images, taken: []string{sidecar}, containers: running("gs")})
	err := a.execute(context.Background(), create)
	assert.ErrorContains(t, err, "already in use")
	assert.Empty(t, cp.events, "a GameServer that is coming up fine must not be made to fail")

	// A name held by a container that is not ours will not come free.
	a, cp = newAgent(&fakeDocker{images: images, taken: []string{sidecar}})
	err = a.execute(context.Background(), create)
	assert.ErrorContains(t, err, "already in use")
	assert.Equal(t, []gameservers.Event{gameservers.CreateFailed}, cp.events)
}

func TestAPlanMadeWhileAnActionRanWaitsForTheNextPass(t *testing.T) {
	// What a pass saw: the GameServer still Creating, and containers.
	// Taken alone that means leftovers of a crash, to be cleared away.
	d := &fakeDocker{containers: running("gs")}
	a, cp := newAgent(d)
	cp.placed = []*agonesv1.GameServer{placed()}
	// But a create was under way, and it finished while Docker was asked.
	require.True(t, a.claim(key))
	d.onList = func() { a.release(key) }

	a.reconcile(context.Background())
	settle(t, a)
	assert.Empty(t, d.calls, "the containers the create has just started must not be taken down")

	// A pass that looks with nothing else going on can trust what it sees.
	d.onList = nil
	a.reconcile(context.Background())
	settle(t, a)
	assert.Contains(t, d.calls, "remove gs-game")
}

func TestAReserveTooLongForADurationIsPassedOnAsTheLongest(t *testing.T) {
	const longest = time.Duration(math.MaxInt64/int64(time.Second)) * time.Second // 292 years
	tests := map[string]struct {
		seconds int64
		want    time.Duration
	}{
		"a minute":                    {seconds: 60, want: time.Minute},
		"until told otherwise":        {seconds: 0, want: 0},
		"negative counts as zero":     {seconds: -5, want: 0},
		"the last second that fits":   {seconds: 9_223_372_036, want: longest},
		"one more wraps to minus":     {seconds: 9_223_372_037, want: longest},
		"enough more wraps to 0.3s":   {seconds: 18_446_744_074, want: longest},
		"far enough below wraps too":  {seconds: -10_000_000_000, want: 0},
		"as many as an int64 can say": {seconds: math.MaxInt64, want: longest},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			a, cp := newAgent(&fakeDocker{})

			err := a.sidecars.pass(key, &sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_RESERVE, ReserveSeconds: tc.seconds})

			require.NoError(t, err)
			assert.Equal(t, []gameservers.Event{gameservers.SDKReserve}, cp.events)
			assert.Equal(t, tc.want, cp.reserve)
		})
	}
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
