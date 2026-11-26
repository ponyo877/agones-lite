package sdkserver

import (
	"context"
	"slices"
	"strconv"

	"agones.dev/agones/pkg/sdk"
	"google.golang.org/protobuf/proto"

	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
)

// The view is the GameServer as the game server sees it: the last one from
// the agent with every pending change made on top. A change to Counters or
// Lists is pending from the moment the SDK call accepts it until the agent
// answers it, so a read right after a write already shows the new value.

// judgement looks at the view and returns the change a request asks for, or
// the reason to refuse the request. It must be a pure function.
type judgement func(view *sdk.GameServer) (*sidecarv1.TellRequest, error)

// change judges a request against the view, makes the change there at once
// and queues it for the agent. It returns the view with the change made.
func (s *Server) change(ctx context.Context, judge judgement) (*sdk.GameServer, error) {
	if err := s.waitForFirst(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	change, err := judge(s.view)
	if err != nil {
		return nil, err
	}
	// Queued and noted under one lock: the agent must get the changes in the
	// order they were made to the view.
	if _, err := s.tell(change); err != nil {
		return nil, err
	}
	s.pending = append(s.pending, change)
	s.show()
	return s.view, nil
}

// settle takes in the agent's answer to req. For a change to Counters or
// Lists the answer is the GameServer with that change made, so the change
// stops being pending and the answer becomes the base.
func (s *Server) settle(req *sidecarv1.TellRequest, answer *sdk.GameServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = slices.DeleteFunc(s.pending, func(change *sidecarv1.TellRequest) bool { return change == req })
	s.adopt(answer)
	// Requests go out one at a time, so right now the agent has seen none of
	// the changes still pending, and neither has the GameServer held back.
	s.adopt(s.held)
	s.held = nil
	s.show()
}

// set takes in a GameServer pushed by the agent.
func (s *Server) set(gs *sdk.GameServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		// The agent may have made the change it is working on before it
		// sent gs, or after: only its answer tells. Making that change on
		// top of gs could count it twice, so gs waits for the answer.
		if !older(gs, s.held) {
			s.held = gs
		}
		return
	}
	s.adopt(gs)
	s.show()
}

// adopt makes gs the base, unless the base is newer: a push that was slow
// to arrive must not undo the answer that overtook it.
func (s *Server) adopt(gs *sdk.GameServer) {
	if gs == nil || older(gs, s.current) {
		return
	}
	if s.current == nil {
		close(s.arrived)
	}
	s.current = gs
}

// older reports whether a is an earlier version of the GameServer than b.
// A resourceVersion in scrim is a decimal number that only ever grows.
// Without two such numbers to compare, a is not older.
func older(a, b *sdk.GameServer) bool {
	versionA, errA := strconv.ParseUint(a.GetObjectMeta().GetResourceVersion(), 10, 64)
	versionB, errB := strconv.ParseUint(b.GetObjectMeta().GetResourceVersion(), 10, 64)
	return errA == nil && errB == nil && versionA < versionB
}

// show works out the view again and hands it to every watcher if it changed.
// Upstream's watchers hear of a Counter or List once it is stored; here they
// hear of it as soon as it is asked for. The caller holds the lock.
func (s *Server) show() {
	view := WithPending(s.current, s.pending)
	if proto.Equal(view, s.view) {
		return
	}
	s.view = view
	for watcher := range s.watchers {
		offer(watcher, view)
	}
}

// offer replaces an update the watcher has not read yet: only the latest
// state matters.
func offer(watcher chan *sdk.GameServer, gs *sdk.GameServer) {
	select {
	case <-watcher:
	default:
	}
	watcher <- gs
}

// get waits for the first GameServer to arrive, then returns the view.
func (s *Server) get(ctx context.Context) (*sdk.GameServer, error) {
	if err := s.waitForFirst(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view, nil
}

func (s *Server) waitForFirst(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.arrived:
		return nil
	}
}

// subscribe returns a channel that carries the view now and after every
// change to it.
func (s *Server) subscribe() (<-chan *sdk.GameServer, func()) {
	watcher := make(chan *sdk.GameServer, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.view != nil {
		watcher <- s.view
	}
	s.watchers[watcher] = struct{}{}
	return watcher, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.watchers, watcher)
	}
}
