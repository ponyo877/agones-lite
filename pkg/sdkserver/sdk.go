package sdkserver

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"agones.dev/agones/pkg/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
)

// Ready asks for the game server to be marked Ready.
func (s *Server) Ready(context.Context, *sdk.Empty) (*sdk.Empty, error) {
	return s.ask(sidecarv1.Event_EVENT_READY, 0)
}

// Allocate lets a game server allocate itself.
func (s *Server) Allocate(context.Context, *sdk.Empty) (*sdk.Empty, error) {
	return s.ask(sidecarv1.Event_EVENT_ALLOCATE, 0)
}

// Shutdown asks for the game server to be shut down and removed.
func (s *Server) Shutdown(context.Context, *sdk.Empty) (*sdk.Empty, error) {
	return s.ask(sidecarv1.Event_EVENT_SHUTDOWN, 0)
}

// Reserve marks the game server Reserved. After d it goes back to Ready;
// zero seconds means Reserved until the game server says otherwise, and so
// does a negative number, as upstream has it.
func (s *Server) Reserve(_ context.Context, d *sdk.Duration) (*sdk.Empty, error) {
	return s.ask(sidecarv1.Event_EVENT_RESERVE, sidecarv1.ReserveFor(d.GetSeconds()))
}

// ask queues one of the four requests that change the game server's state.
// Each ends the reservation that came before, and a Reserve with a time
// begins one.
//
// Ending the old reservation and queueing the request happen under one
// lock, and so does the timer's own message. "The time is up" is therefore
// queued before the request or not at all. Were it queued after a new
// Reserve, it would end the reservation the game server has just made.
func (s *Server) ask(event sidecarv1.Event, reserve time.Duration) (*sdk.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserve != nil {
		s.reserve.Stop()
	}
	s.reserve = nil
	s.asked++
	if reserve > 0 {
		s.reserve = s.expireAfter(reserve, s.asked)
	}
	return s.tell(&sidecarv1.TellRequest{Event: event, ReserveSeconds: int64(reserve / time.Second)})
}

// expireAfter returns a timer that says when the time of a Reserve is up.
// It does not ask for Ready, which is what upstream does
// (resetReserveAfter). A timer that has fired cannot be stopped, so a game
// server that allocates itself at that moment would be taken back to Ready,
// session and all. Whether the time ran out on a Reserved game server is
// for the control plane to say.
//
// asked is the number of the Reserve the timer belongs to. The timer is
// told that number, and not which timer it is: its function may only read
// what was there before the timer was started, and the timer was not.
func (s *Server) expireAfter(d time.Duration, asked int) *time.Timer {
	return time.AfterFunc(d, func() { s.expired(asked) })
}

// expired is what a reservation's timer does when it fires. A timer that
// was stopped too late finds that another request came after its Reserve,
// and says nothing.
func (s *Server) expired(asked int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asked == asked {
		_, _ = s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_RESERVE_EXPIRED})
	}
}

// Health receives pings for as long as the game server keeps the stream open.
func (s *Server) Health(stream sdk.SDK_HealthServer) error {
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&sdk.Empty{})
		}
		if err != nil {
			return err
		}
		s.health.touch(time.Now())
	}
}

// GetGameServer returns the game server's own GameServer.
func (s *Server) GetGameServer(ctx context.Context, _ *sdk.Empty) (*sdk.GameServer, error) {
	return s.get(ctx)
}

// WatchGameServer sends the GameServer now and again after every change.
func (s *Server) WatchGameServer(_ *sdk.Empty, stream sdk.SDK_WatchGameServerServer) error {
	updates, cancel := s.subscribe()
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case gs := <-updates:
			if err := stream.Send(gs); err != nil {
				return err
			}
		}
	}
}

// SetLabel sets a label, which the control plane stores under "agones.dev/sdk-".
func (s *Server) SetLabel(_ context.Context, kv *sdk.KeyValue) (*sdk.Empty, error) {
	if err := validate("label", kv, validation.IsValidLabelValue); err != nil {
		return nil, err
	}
	labels := map[string]string{kv.GetKey(): kv.GetValue()}
	return s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_SET_METADATA, Labels: labels})
}

// SetAnnotation sets an annotation, stored under the same prefix.
func (s *Server) SetAnnotation(_ context.Context, kv *sdk.KeyValue) (*sdk.Empty, error) {
	if err := validate("annotation", kv, nil); err != nil {
		return nil, err
	}
	annotations := map[string]string{kv.GetKey(): kv.GetValue()}
	return s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_SET_METADATA, Annotations: annotations})
}

// validate applies upstream's rules and wording for SetLabel and SetAnnotation.
func validate(what string, kv *sdk.KeyValue, checkValue func(string) []string) error {
	if kv == nil {
		return status.Errorf(codes.InvalidArgument, "%s key/value cannot be nil", what)
	}
	if errs := validation.IsQualifiedName(kv.GetKey()); len(errs) > 0 {
		return status.Errorf(codes.InvalidArgument, "invalid %s key %q: %s", what, kv.GetKey(), strings.Join(errs, ", "))
	}
	if checkValue == nil {
		return nil
	}
	if errs := checkValue(kv.GetValue()); len(errs) > 0 {
		return status.Errorf(codes.InvalidArgument, "invalid %s value %q for key %q: %s", what, kv.GetValue(), kv.GetKey(), strings.Join(errs, ", "))
	}
	return nil
}
