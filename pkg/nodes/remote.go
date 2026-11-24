package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/auth"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	nodev1 "github.com/ponyo877/agones-lite/proto/node/v1"
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

const (
	// reconnectDelay is how long an agent waits before dialling again.
	reconnectDelay = 2 * time.Second
	// callTimeout bounds every single call, so a dead connection is noticed.
	callTimeout = 10 * time.Second
)

// redial is how gRPC spaces its attempts to connect. Its own limit is two
// minutes; a control plane that comes back after a long break should be
// found again within seconds, because until then its game servers cannot
// be allocated.
var redial = grpc.ConnectParams{
	Backoff:           backoff.Config{BaseDelay: time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 5 * time.Second},
	MinConnectTimeout: 5 * time.Second,
}

// Remote is an agent's link to a control plane on another machine. It
// satisfies agent.ControlPlane by speaking the protocol in proto/node/v1.
//
// It keeps a copy of the GameServers placed on its node, fed by one
// long-lived Watch call. While that call is down the copy is stale, and
// Assigned says so.
type Remote struct {
	client nodev1.NodesClient
	conn   *grpc.ClientConn
	logs   LogReader // reads logs on this node; set with Serve

	mu      sync.Mutex
	servers map[types.NamespacedName]*agonesv1.GameServer
	current bool
	changed chan struct{}
}

// Dial prepares a link to the control plane at address ("host:port"),
// authenticated by the join token. Nothing is sent until Run or a call.
func Dial(address, joinToken string) (*Remote, error) {
	fingerprint, _, ok := strings.Cut(joinToken, ".")
	if !ok {
		return nil, errors.New("the join token has the wrong shape; copy it from `agones-lite token show join`")
	}
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(credentials.NewTLS(auth.TrustPinned(fingerprint))),
		grpc.WithPerRPCCredentials(auth.Token(joinToken)),
		// Without pings, a control plane that vanished would look merely quiet.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 5 * time.Second}),
		grpc.WithConnectParams(redial),
	)
	if err != nil {
		return nil, err
	}
	return &Remote{
		client:  nodev1.NewNodesClient(conn),
		conn:    conn,
		servers: map[types.NamespacedName]*agonesv1.GameServer{},
		changed: make(chan struct{}, 1),
	}, nil
}

// Close ends the link.
func (r *Remote) Close() error {
	return r.conn.Close()
}

// Run keeps the Watch call for node open until ctx is done, dialling again
// whenever it breaks.
func (r *Remote) Run(ctx context.Context, node string) {
	refused := false // whether the last call ended over the join token
	for ctx.Err() == nil {
		err := r.watch(ctx, node) // only returns when the call ended
		if r.setCurrent(false) {
			refused = false // the call had been working, so a refusal is news again
		}
		if ctx.Err() == nil {
			refused = explain(err, refused)
		}
		select {
		case <-ctx.Done():
		case <-time.After(reconnectDelay):
		}
	}
}

// explain logs why a Watch call ended, and reports whether the control
// plane refused the join token. Trying again does not mend that, so it is
// said once, and not with every attempt that follows.
func explain(err error, refusedBefore bool) (refused bool) {
	if status.Code(err) != codes.Unauthenticated {
		slog.Warn("lost the control plane, reconnecting", "error", err)
		return false
	}
	if !refusedBefore {
		slog.Error("the control plane refuses this agent's join token, which was probably rotated; " +
			"start the agent again with the new one, which `agones-lite token show join` prints on the control plane")
	}
	return true
}

// watch runs one Watch call. What arrives before "synced" is collected
// aside, so that a half-received list never passes for the whole.
func (r *Remote) watch(ctx context.Context, node string) error {
	stream, err := r.client.Watch(ctx, &nodev1.WatchRequest{Node: node})
	if err != nil {
		return err
	}
	fresh := map[types.NamespacedName]*agonesv1.GameServer{}
	for {
		update, err := stream.Recv()
		if err != nil {
			return err
		}
		if request := update.GetLogRequest(); request != nil && r.logs != nil {
			go r.sendLog(ctx, request)
			continue
		}
		if err := r.take(update, fresh); err != nil {
			return err
		}
	}
}

// take applies one message of the Watch call.
func (r *Remote) take(update *nodev1.WatchResponse, fresh map[types.NamespacedName]*agonesv1.GameServer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.servers
	if !r.current {
		target = fresh
	}
	switch change := update.GetChange().(type) {
	case *nodev1.WatchResponse_GameServer:
		gs := &agonesv1.GameServer{}
		if err := json.Unmarshal(change.GameServer, gs); err != nil {
			return err
		}
		target[types.NamespacedName{Namespace: gs.Namespace, Name: gs.Name}] = gs
	case *nodev1.WatchResponse_Removed:
		delete(target, types.NamespacedName{Namespace: change.Removed.GetNamespace(), Name: change.Removed.GetName()})
	case *nodev1.WatchResponse_Synced:
		r.servers, r.current = fresh, true
	}
	r.signal()
	return nil
}

// setCurrent notes whether the copy is fed by a live call, and returns
// whether it was before.
func (r *Remote) setCurrent(current bool) (before bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before, r.current = r.current, current
	return before
}

// signal tells the agent to look again. Called with the lock held.
func (r *Remote) signal() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// Assigned returns the copy of the GameServers placed on the node, and
// whether it is complete and fed by a live connection.
func (r *Remote) Assigned(string) ([]*agonesv1.GameServer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*agonesv1.GameServer, 0, len(r.servers))
	for _, gs := range r.servers {
		out = append(out, gs.DeepCopy())
	}
	return out, r.current
}

// Changes signals whenever the copy changed.
func (r *Remote) Changes() (<-chan struct{}, func()) {
	return r.changed, func() {}
}

// Register announces the node.
func (r *Remote) Register(node *litev1.Node) error {
	data, err := json.Marshal(node)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	_, err = r.client.Register(ctx, &nodev1.RegisterRequest{Node: data})
	return err
}

// Report passes on something that happened to a GameServer.
func (r *Remote) Report(report gameservers.Report) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	_, err := r.client.Report(ctx, &nodev1.ReportRequest{
		Namespace:      report.Namespace,
		Name:           report.Name,
		Event:          string(report.Event),
		ReserveSeconds: int64(report.Reserve / time.Second),
		Detail:         report.Detail,
		Uid:            string(report.UID),
	})
	return err
}

// UpdateCounts changes Counters and Lists as a game server asked through the SDK.
func (r *Remote) UpdateCounts(namespace, name, change string, counters []*sidecarv1.CounterUpdate, lists []*sidecarv1.ListUpdate) (*agonesv1.GameServer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	resp, err := r.client.UpdateCounts(ctx, &nodev1.UpdateCountsRequest{
		Namespace: namespace, Name: name, ChangeId: change, Counters: counters, Lists: lists,
	})
	if err != nil || len(resp.GetGameServer()) == 0 {
		return nil, err
	}
	gs := &agonesv1.GameServer{}
	return gs, json.Unmarshal(resp.GetGameServer(), gs)
}

// SetMetadata adds labels and annotations set through the SDK.
func (r *Remote) SetMetadata(namespace, name string, labels, annotations map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	_, err := r.client.SetMetadata(ctx, &nodev1.SetMetadataRequest{
		Namespace: namespace, Name: name, Labels: labels, Annotations: annotations,
	})
	return err
}
