package auth

import (
	"context"
	"net/http"
	"strings"

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
func RequireHTTP(check Checker, role Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), bearer)
		if !check.Allows(role, token) && !hasVerifiedCertificate(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, `{"error":"a valid token is required"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasVerifiedCertificate(r *http.Request) bool {
	return r.TLS != nil && len(r.TLS.VerifiedChains) > 0
}

// RequireGRPC returns the server options that do for gRPC what RequireHTTP
// does for HTTP, on single calls and on streams alike.
func RequireGRPC(check Checker, role Role) []grpc.ServerOption {
	allowed := func(ctx context.Context) error {
		md, _ := metadata.FromIncomingContext(ctx)
		for _, header := range md.Get("authorization") {
			if token, ok := strings.CutPrefix(header, bearer); ok && check.Allows(role, token) {
				return nil
			}
		}
		return status.Error(codes.Unauthenticated, "a valid token is required")
	}
	unary := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := allowed(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := allowed(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
	return []grpc.ServerOption{grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(stream)}
}

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
