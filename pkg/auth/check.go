package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const bearer = "Bearer "

// Checker says whether a token grants a role. *Secrets is one.
type Checker interface {
	Allows(role Role, token string) bool
}

// RequireHTTP lets a request through to next only when it carries a token
// for role in its Authorization header, or came with a client certificate
// the listener verified (see ServerTLSWithClients). A gRPC call served over
// net/http is covered too: its "authorization" metadata is this same header.
//
// An answer that goes on, such as a watch or a log that is being followed,
// is cut off once the token stops being valid, as a gRPC stream is by
// guardStream. It is cut off rather than ended, so that the client sees a
// broken connection and not what looks like the natural end of a log.
func RequireHTTP(check Checker, role Role, next http.Handler) http.Handler {
	return requireHTTP(check, role, next, recheckEvery)
}

// requireHTTP is RequireHTTP with the time between two looks at the token
// of an answer that goes on. A test does not have five seconds.
func requireHTTP(check Checker, role Role, next http.Handler, every time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := func() error { return allowedHTTP(r, check, role) }
		if allowed() != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, `{"error":"a valid token is required"}`, http.StatusUnauthorized)
			return
		}
		ctx, end := context.WithCancelCause(r.Context())
		defer end(nil)
		go endWhenRefused(ctx, end, allowed, every)
		next.ServeHTTP(w, r.WithContext(ctx))
		if errors.Is(context.Cause(ctx), errRefused) {
			panic(http.ErrAbortHandler) // net/http's way to close the connection in mid-answer
		}
	})
}

// errRefused says that a request has no token for what it asks.
var errRefused = errors.New("a valid token is required")

func allowedHTTP(r *http.Request, check Checker, role Role) error {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), bearer)
	if !check.Allows(role, token) && !hasVerifiedCertificate(r) {
		return errRefused
	}
	return nil
}

func hasVerifiedCertificate(r *http.Request) bool {
	return r.TLS != nil && len(r.TLS.VerifiedChains) > 0
}

// recheckEvery is how often the token of an open stream, or of an answer
// that goes on, is looked at again.
const recheckEvery = 5 * time.Second

// RequireGRPC returns the server options that do for gRPC what RequireHTTP
// does for HTTP, on single calls and on streams alike.
func RequireGRPC(check Checker, role Role) []grpc.ServerOption {
	allowed := func(ctx context.Context) error { return allowedGRPC(ctx, check, role) }
	unary := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := allowed(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	return []grpc.ServerOption{grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(guardStream(allowed, recheckEvery))}
}

func allowedGRPC(ctx context.Context, check Checker, role Role) error {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, header := range md.Get("authorization") {
		if token, ok := strings.CutPrefix(header, bearer); ok && check.Allows(role, token) {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "a valid token is required")
}

// guardStream lets a stream start only with a valid token, and ends it once
// the token stops being valid, for example because it was rotated. Without
// the second half, a caller that could no longer make a single call would
// keep its open stream, and look connected while nothing it says is heard.
func guardStream(allowed func(context.Context) error, every time.Duration) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := allowed(ss.Context()); err != nil {
			return err
		}
		ctx, end := context.WithCancelCause(ss.Context())
		defer end(nil)
		go endWhenRefused(ctx, end, func() error { return allowed(ctx) }, every)
		err := handler(srv, &guarded{ServerStream: ss, ctx: ctx})
		if refused := context.Cause(ctx); status.Code(refused) == codes.Unauthenticated {
			return refused
		}
		return err
	}
}

// endWhenRefused checks again at every tick and ends the stream with
// the refusal as its cause.
func endWhenRefused(ctx context.Context, end context.CancelCauseFunc, allowed func() error, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := allowed(); err != nil {
				end(err)
				return
			}
		}
	}
}

// guarded is a stream whose context ends when its token is refused.
type guarded struct {
	grpc.ServerStream
	ctx context.Context
}

func (g *guarded) Context() context.Context { return g.ctx }

// Token attaches a token to every gRPC call of a client.
// It implements credentials.PerRPCCredentials.
type Token string

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (t Token) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": bearer + string(t)}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials: a token
// is only ever sent over TLS.
func (Token) RequireTransportSecurity() bool {
	return true
}
