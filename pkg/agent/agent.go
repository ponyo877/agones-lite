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

	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/podspec"
	"github.com/ponyo877/scrim/pkg/version"
	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
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
	// Register announces this node and what it can hold. Like every call
	// here that tells the control plane something, it returns once that is
	// saved there, or when ctx is done.
	Register(ctx context.Context, node *scrimv1.Node) error
	// Assigned returns the GameServers placed on the node. current is false
	// while the list is not known to be complete and up to date, for example
	// before the first answer from a remote control plane has arrived.
	Assigned(node string) (servers []*agonesv1.GameServer, current bool)
	// Changes signals whenever an assigned GameServer may have changed.
	Changes() (signal <-chan struct{}, cancel func())
	// Report passes on something that happened to a GameServer.
	Report(ctx context.Context, r gameservers.Report) error
	// SetMetadata adds labels and annotations set through the SDK.
	SetMetadata(ctx context.Context, namespace, name string, labels, annotations map[string]string) error
	// UpdateCounts changes Counters and Lists as a game server asked through
	// the SDK. It returns the GameServer afterwards, or nil when it is gone.
	// change names the change, so that one sent twice is made once.
	UpdateCounts(ctx context.Context, namespace, name, change string, counters []*sidecarv1.CounterUpdate, lists []*sidecarv1.ListUpdate) (*agonesv1.GameServer, error)
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
	PortRange      scrimv1.PortRange
	MaxGameServers int32
	Capacity       scrimv1.Resources

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
	a.warnOfUnmanaged(ctx)

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
		if err := a.cp.Register(ctx, a.node()); err != nil {
			slog.Warn("cannot register the node, will try again", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *Agent) node() *scrimv1.Node {
	node := &scrimv1.Node{}
	node.APIVersion, node.Kind, node.Name = scrimv1.APIVersion, scrimv1.NodeKind, a.cfg.NodeName
	node.Status = scrimv1.NodeStatus{
		Phase:          scrimv1.NodeReady,
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
		return a.orCreateFailed(ctx, action, a.bringUp(ctx, action))
	case Start:
		return a.orCreateFailed(ctx, action, a.startAll(ctx, action, ids(action.Containers)))
	case Remove:
		prefix := podspec.Prefix(action.Key.Namespace, action.Key.Name)
		if err := a.takeDown(ctx, action.Containers, podspec.GracePeriod(action.GameServer), prefix); err != nil {
			return err
		}
		return a.tell(ctx, action, gameservers.ContainersRemoved)
	case Discard:
		prefix := podspec.Prefix(action.Key.Namespace, action.Key.Name)
		return a.takeDown(ctx, action.Containers, discardGrace, prefix)
	case Tell:
		return a.tell(ctx, action, action.Event)
	}
	return nil
}

// tell reports event of the GameServer the action is about: of that very
// one, should another have taken its name meanwhile.
func (a *Agent) tell(ctx context.Context, about Action, event gameservers.Event) error {
	if err := a.cp.Report(ctx, about.report(event, "")); err != nil {
		return unreported{err}
	}
	return nil
}

func (a Action) report(event gameservers.Event, detail string) gameservers.Report {
	return gameservers.Report{Namespace: a.Key.Namespace, Name: a.Key.Name, UID: a.UID, Event: event, Detail: detail}
}

// unreported is a failure to reach the control plane, as opposed to a
// failure to do the work. The work stands, and the next pass reports it.
type unreported struct{ error }

func (u unreported) Unwrap() error { return u.error }

// orCreateFailed reports CreateFailed when the containers could not be
// brought up, which is what err says. Three things are not that. An agent
// that is stopping was only interrupted, one that found its containers
// there already came too late, and one that could not tell the control
// plane how far it got has containers that are fine. The next pass carries
// on from what it finds. CreateFailed is final, so it must not be said of a
// GameServer that may yet come up.
//
// What Docker said goes along. The operator reads the event history; this
// machine's log may be far away.
func (a *Agent) orCreateFailed(ctx context.Context, about Action, err error) error {
	if err == nil || ctx.Err() != nil || errors.As(err, &unreported{}) || a.alreadyThere(ctx, about.Key, err) {
		return err
	}
	return errors.Join(err, a.cp.Report(ctx, about.report(gameservers.CreateFailed, err.Error())))
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

func (a *Agent) bringUp(ctx context.Context, about Action) error {
	gs := about.GameServer
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
	a.sidecars.allow(about.Key, ticket{token: token, uid: gs.UID}) // before the sidecar starts and dials in
	if err := a.tell(ctx, about, gameservers.ContainersCreated); err != nil {
		return err
	}
	return a.startAll(ctx, about, ids)
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
func (a *Agent) startAll(ctx context.Context, about Action, ids []string) error {
	for _, id := range ids {
		if err := a.docker.Start(ctx, id); err != nil {
			return err
		}
	}
	return a.tell(ctx, about, gameservers.ContainersRunning)
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
