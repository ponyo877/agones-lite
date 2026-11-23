package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/podspec"
)

const (
	// reconcilePeriod is how often Docker is checked even when nothing was
	// announced, which is how container exits are noticed.
	reconcilePeriod = 2 * time.Second
	// discardGrace is how long containers nobody wants get to exit.
	discardGrace = 10 * time.Second
)

// ControlPlane is the agent's view of the control plane. In one process it
// is a handful of direct calls (pkg/nodes.Local); between machines it will
// be gRPC.
type ControlPlane interface {
	// Register announces this node and what it can hold.
	Register(node *litev1.Node) error
	// Assigned returns the GameServers placed on the node.
	Assigned(node string) []*agonesv1.GameServer
	// Changes signals whenever an assigned GameServer may have changed.
	Changes() (signal <-chan struct{}, cancel func())
	// Report passes on something that happened to a GameServer.
	Report(gameservers.Report) error
	// SetMetadata adds labels and annotations set through the SDK.
	SetMetadata(namespace, name string, labels, annotations map[string]string) error
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
	// SidecarListen is where the agent waits for sidecars to connect, and
	// SidecarDial is how a container reaches that same listener.
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

// Run registers the node and reconciles until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.cp.Register(a.node()); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	listener, err := net.Listen("tcp", a.cfg.SidecarListen)
	if err != nil {
		return fmt.Errorf("listen for sidecars: %w", err)
	}
	go a.sidecars.serve(ctx, listener)

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

func (a *Agent) node() *litev1.Node {
	node := &litev1.Node{}
	node.APIVersion, node.Kind, node.Name = litev1.APIVersion, litev1.NodeKind, a.cfg.NodeName
	node.Status = litev1.NodeStatus{
		Phase:          litev1.NodeReady,
		Address:        a.cfg.Address,
		Capacity:       a.cfg.Capacity,
		MaxGameServers: a.cfg.MaxGameServers,
		PortRange:      a.cfg.PortRange,
	}
	return node
}

// reconcile is one pass: look at both sides, plan, and start the actions.
func (a *Agent) reconcile(ctx context.Context) {
	desired := a.cp.Assigned(a.cfg.NodeName)
	actual, err := a.docker.List(ctx, podspec.LabelGameServer)
	if err != nil {
		slog.Error("cannot list containers", "error", err)
		return
	}
	a.sidecars.update(desired, actual)
	for _, action := range Plan(desired, actual) {
		a.start(ctx, action)
	}
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
		return a.create(ctx, action.Key, action.GameServer)
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

// create brings the containers up, and reports CreateFailed when it cannot.
func (a *Agent) create(ctx context.Context, key Key, gs *agonesv1.GameServer) error {
	err := a.bringUp(ctx, key, gs)
	if err == nil {
		return nil
	}
	return errors.Join(err, a.tell(key, gameservers.CreateFailed))
}

func (a *Agent) bringUp(ctx context.Context, key Key, gs *agonesv1.GameServer) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	specs := podspec.Containers(gs, podspec.Sidecar{Image: a.cfg.SidecarImage, AgentAddress: a.cfg.SidecarDial, Token: token})
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
	for _, id := range ids {
		if err := a.docker.Start(ctx, id); err != nil {
			return err
		}
	}
	return a.tell(key, gameservers.ContainersRunning)
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
