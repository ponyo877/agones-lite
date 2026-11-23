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

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// Ready asks for the game server to be marked Ready.
func (s *Server) Ready(context.Context, *sdk.Empty) (*sdk.Empty, error) {
	s.stopReserveTimer()
	return s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_READY})
}

// Allocate lets a game server allocate itself.
func (s *Server) Allocate(context.Context, *sdk.Empty) (*sdk.Empty, error) {
	s.stopReserveTimer()
	return s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_ALLOCATE})
}

// Shutdown asks for the game server to be shut down and removed.
func (s *Server) Shutdown(context.Context, *sdk.Empty) (*sdk.Empty, error) {
	s.stopReserveTimer()
	return s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_SHUTDOWN})
}

// Reserve marks the game server Reserved. After d it asks for Ready again;
// zero seconds means Reserved until the game server says otherwise.
func (s *Server) Reserve(_ context.Context, d *sdk.Duration) (*sdk.Empty, error) {
	s.stopReserveTimer()
	if seconds := d.GetSeconds(); seconds > 0 {
		s.startReserveTimer(time.Duration(seconds) * time.Second)
	}
	return s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_RESERVE, ReserveSeconds: d.GetSeconds()})
}

func (s *Server) startReserveTimer(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserve = time.AfterFunc(d, func() {
		_, _ = s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_READY})
	})
}

func (s *Server) stopReserveTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserve != nil {
		s.reserve.Stop()
		s.reserve = nil
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
