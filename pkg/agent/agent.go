package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/podspec"
	"github.com/ponyo877/agones-lite/pkg/version"
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

const (
	// reconcilePeriod is how often Docker is checked even when nothing was
	// announced, which is how container exits are noticed.
	reconcilePeriod = 2 * time.Second
	// heartbeatPeriod is how often the node is announced again.
	heartbeatPeriod = 10 * time.Second
	// discardGrace is how long containers nobody wants get to exit.
	discardGrace = 10 * time.Second
)

// ControlPlane is the agent's view of the control plane. In one process it
// is a handful of direct calls (pkg/nodes.Local); between machines it will
// be gRPC.
type ControlPlane interface {
	// Register announces this node and what it can hold.
	Register(node *litev1.Node) error
	// Assigned returns the GameServers placed on the node. current is false
	// while the list is not known to be complete and up to date, for example
	// before the first answer from a remote control plane has arrived.
	Assigned(node string) (servers []*agonesv1.GameServer, current bool)
	// Changes signals whenever an assigned GameServer may have changed.
	Changes() (signal <-chan struct{}, cancel func())
	// Report passes on something that happened to a GameServer.
	Report(gameservers.Report) error
	// SetMetadata adds labels and annotations set through the SDK.
	SetMetadata(namespace, name string, labels, annotations map[string]string) error
	// UpdateCounts changes Counters and Lists as a game server asked through
	// the SDK. It returns the GameServer afterwards, or nil when it is gone.
	// change names the change, so that one sent twice is made once.
	UpdateCounts(namespace, name, change string, counters []*sidecarv1.CounterUpdate, lists []*sidecarv1.ListUpdate) (*agonesv1.GameServer, error)
}

// Docker is the part of the Docker client the agent uses.
type Docker interface {
	List(ctx context.Context, label string) ([]docker.Container, error)
	HasImage(ctx context.Context, image string) (bool, error)
	Pull(ctx context.Context, image string) error
	Create(ctx context.Context, spec docker.Spec) (string, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string, grace time.Duration) error
	Remove(ctx context.Context, id string) error
	RemoveVolumes(ctx context.Context, prefix string) error
	Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error)
}

// Config describes this node.
type Config struct {
	NodeName string
	// Address is where players reach game servers on this node.
	Address        string
	PortRange      litev1.PortRange
	MaxGameServers int32
	Capacity       litev1.Resources

	SidecarImage string
	// SidecarListen is where the agent waits for sidecars to connect:
	// "unix:///dir/agent.sock" or "host:port". SidecarDial is how a
	// container reaches a TCP listener; a socket needs none, it is mounted.
	SidecarListen string
	SidecarDial   string
}

// Agent makes Docker on one node match the control plane.
type Agent struct {
	cfg      Config
	cp       ControlPlane
	docker   Docker
	sidecars *sidecarServer

	mu   sync.Mutex
	busy map[Key]bool // GameServers with an action in progress
}

// New returns an agent for the node described by cfg.
func New(cfg Config, cp ControlPlane, d Docker) *Agent {
	return &Agent{cfg: cfg, cp: cp, docker: d, sidecars: newSidecarServer(cp), busy: map[Key]bool{}}
}

// Run announces the node and reconciles until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	listener, err := listen(a.cfg.SidecarListen)
	if err != nil {
		return fmt.Errorf("listen for sidecars: %w", err)
	}
	go a.sidecars.serve(ctx, listener)
	go a.announce(ctx)

	changes, cancel := a.cp.Changes()
	defer cancel()
	tick := time.NewTicker(reconcilePeriod)
	defer tick.Stop()
	for {
		a.reconcile(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
		case <-tick.C:
		}
	}
}

// announce registers the node now and again every heartbeatPeriod. A control
// plane that cannot be reached yet is simply tried again.
func (a *Agent) announce(ctx context.Context) {
	tick := time.NewTicker(heartbeatPeriod)
	defer tick.Stop()
	for {
		if err := a.cp.Register(a.node()); err != nil {
			slog.Warn("cannot register the node, will try again", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *Agent) node() *litev1.Node {
	node := &litev1.Node{}
	node.APIVersion, node.Kind, node.Name = litev1.APIVersion, litev1.NodeKind, a.cfg.NodeName
	node.Status = litev1.NodeStatus{
		Phase:          litev1.NodeReady,
		Address:        a.cfg.Address,
		Capacity:       a.cfg.Capacity,
		MaxGameServers: a.cfg.MaxGameServers,
		PortRange:      a.cfg.PortRange,
		Version:        version.Version,
	}
	return node
}

// reconcile is one pass: look at both sides, plan, and start the actions.
// Without a current answer from the control plane nothing is touched: the
// containers may be exactly what is wanted, and we just cannot know yet.
func (a *Agent) reconcile(ctx context.Context) {
	changing := a.inProgress() // noted before looking, or one could end unseen
	desired, current := a.cp.Assigned(a.cfg.NodeName)
	if !current {
		return
	}
	actual, err := a.docker.List(ctx, podspec.LabelNode+"="+a.cfg.NodeName)
	if err != nil {
		slog.Error("cannot list containers", "error", err)
		return
	}
	a.sidecars.update(desired, actual)
	for _, action := range Plan(desired, actual) {
		// A GameServer that was being worked on while we looked may have
		// been seen half done. What to do about it is left to the next pass.
		if !changing[action.Key] {
			a.start(ctx, action)
		}
	}
}

// inProgress returns the GameServers that have an action running right now.
// Actions are only started by reconcile, so none is added behind its back.
func (a *Agent) inProgress() map[Key]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.busy)
}

// start runs an action in the background, unless its GameServer already has
// one running. Pulling an image can take minutes and must not hold up the rest.
func (a *Agent) start(ctx context.Context, action Action) {
	if !a.claim(action.Key) {
		return
	}
	go func() {
		defer a.release(action.Key)
		if err := a.execute(ctx, action); err != nil {
			slog.Error("action failed", "gameserver", action.Key.String(), "error", err)
		}
	}()
}

func (a *Agent) claim(key Key) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy[key] {
		return false
	}
	a.busy[key] = true
	return true
}

func (a *Agent) release(key Key) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.busy, key)
}

// execute carries out one decision of Plan.
func (a *Agent) execute(ctx context.Context, action Action) error {
	switch action.Kind {
	case Create:
		return a.orCreateFailed(ctx, action.Key, a.bringUp(ctx, action.Key, action.GameServer))
	case Start:
		return a.orCreateFailed(ctx, action.Key, a.startAll(ctx, action.Key, ids(action.Containers)))
	case Remove:
		prefix := podspec.Prefix(action.Key.Namespace, action.Key.Name)
		if err := a.takeDown(ctx, action.Containers, podspec.GracePeriod(action.GameServer), prefix); err != nil {
			return err
		}
		return a.tell(action.Key, gameservers.ContainersRemoved)
	case Discard:
		prefix := podspec.Prefix(action.Key.Namespace, action.Key.Name)
		return a.takeDown(ctx, action.Containers, discardGrace, prefix)
	case Tell:
		return a.tell(action.Key, action.Event)
	}
	return nil
}

func (a *Agent) tell(key Key, event gameservers.Event) error {
	return a.cp.Report(gameservers.Report{Namespace: key.Namespace, Name: key.Name, Event: event})
}

// orCreateFailed reports CreateFailed when the containers could not be
// brought up, which is what err says. Two things are not that. An agent
// that is stopping was only interrupted, and one that found its containers
// there already came too late. The next pass carries on from what it finds.
func (a *Agent) orCreateFailed(ctx context.Context, key Key, err error) error {
	if err == nil || ctx.Err() != nil || a.alreadyThere(ctx, key, err) {
		return err
	}
	return errors.Join(err, a.tell(key, gameservers.CreateFailed))
}

// alreadyThere reports whether err is Docker refusing a name while the
// GameServer has containers. reconcile does not plan a create then, so this
// should not happen. If it does all the same, the GameServer may be coming
// up fine and must not be failed for it.
func (a *Agent) alreadyThere(ctx context.Context, key Key, err error) bool {
	var refused *docker.Error
	if !errors.As(err, &refused) || refused.Status != http.StatusConflict {
		return false
	}
	actual, err := a.docker.List(ctx, podspec.LabelNode+"="+a.cfg.NodeName)
	return err == nil && len(group(actual)[key]) > 0
}

func (a *Agent) bringUp(ctx context.Context, key Key, gs *agonesv1.GameServer) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	address, binds := a.cfg.sidecarAccess()
	specs := podspec.Containers(gs, podspec.Sidecar{Image: a.cfg.SidecarImage, AgentAddress: address, Binds: binds, Token: token})
	if err := a.pullImages(ctx, gs, specs); err != nil {
		return err
	}
	ids, err := a.createAll(ctx, specs)
	if err != nil {
		return err
	}
	a.sidecars.allow(key, token) // before the sidecar starts and dials in
	if err := a.tell(key, gameservers.ContainersCreated); err != nil {
		return err
	}
	return a.startAll(ctx, key, ids)
}

func (a *Agent) createAll(ctx context.Context, specs []docker.Spec) ([]string, error) {
	ids := make([]string, 0, len(specs))
	for _, spec := range specs {
		id, err := a.docker.Create(ctx, spec)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// startAll starts the containers in the order given, which must have the
// sidecar first, and reports that they run. An agent that stops half way
// leaves containers that Plan hands to the next one as a Start.
func (a *Agent) startAll(ctx context.Context, key Key, ids []string) error {
	for _, id := range ids {
		if err := a.docker.Start(ctx, id); err != nil {
			return err
		}
	}
	return a.tell(key, gameservers.ContainersRunning)
}

func ids(containers []docker.Container) []string {
	out := make([]string, 0, len(containers))
	for _, c := range containers {
		out = append(out, c.ID)
	}
	return out
}

func (a *Agent) pullImages(ctx context.Context, gs *agonesv1.GameServer, specs []docker.Spec) error {
	policies := map[string]corev1.PullPolicy{}
	for _, c := range gs.Spec.Template.Spec.Containers {
		policies[c.Image] = c.ImagePullPolicy
	}
	for _, spec := range specs {
		present, err := a.docker.HasImage(ctx, spec.Image)
		if err != nil {
			return err
		}
		if !needsPull(policies[spec.Image], spec.Image, present) {
			continue
		}
		if err := a.docker.Pull(ctx, spec.Image); err != nil {
			return err
		}
	}
	return nil
}

// needsPull follows Kubernetes: an unset policy means "always" for the
// latest tag and "if not present" for any other.
func needsPull(policy corev1.PullPolicy, image string, present bool) bool {
	switch policy {
	case corev1.PullAlways:
		return true
	case corev1.PullNever:
		return false
	case corev1.PullIfNotPresent:
		return !present
	}
	_, tag := docker.SplitTag(image)
	return !present || tag == "latest"
}

// takeDown stops the game containers first, so they can still reach the SDK
// while they shut down, then removes everything that belonged to the GameServer.
func (a *Agent) takeDown(ctx context.Context, containers []docker.Container, grace time.Duration, prefix string) error {
	for _, c := range containers {
		if c.Labels[podspec.LabelRole] == podspec.RoleSidecar {
			continue
		}
		if err := a.docker.Stop(ctx, c.ID, grace); err != nil {
			return err
		}
	}
	for _, c := range containers {
		if err := a.docker.Remove(ctx, c.ID); err != nil {
			return err
		}
	}
	return a.docker.RemoveVolumes(ctx, prefix)
}

func newToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
