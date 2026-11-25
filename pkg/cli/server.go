package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/ponyo877/scrim/pkg/agent"
	"github.com/ponyo877/scrim/pkg/agones"
	"github.com/ponyo877/scrim/pkg/auth"
	"github.com/ponyo877/scrim/pkg/controlplane"
	"github.com/ponyo877/scrim/pkg/events"
	"github.com/ponyo877/scrim/pkg/metrics"
	"github.com/ponyo877/scrim/pkg/nodes"
	"github.com/ponyo877/scrim/pkg/store"
	nodev1 "github.com/ponyo877/scrim/proto/node/v1"
)

const (
	// eventRetention is how long the event history is kept.
	eventRetention = 24 * time.Hour
	// tokenReload is how often rotated tokens are picked up.
	tokenReload = 5 * time.Second
)

// part is one long-running piece of a process. It returns when ctx is done.
type part func(ctx context.Context) error

type serverOptions struct {
	dataDir          string
	listen           string
	allocationListen string
	nodeListen       string
	metricsListen    string
	tlsNames         string
	clientCADir      string
	noAgent          bool
	graces           nodes.Graces
	node             nodeOptions
}

func serverFlags(args []string) (*serverOptions, error) {
	o := &serverOptions{}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.StringVar(&o.dataDir, "data-dir", env("SCRIM_DATA_DIR", defaultDataDir), "directory for the state database, certificates and tokens")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:9443", "address of the management API")
	fs.StringVar(&o.allocationListen, "allocation-listen", "127.0.0.1:9444", "address of the allocation API")
	fs.StringVar(&o.nodeListen, "node-listen", "", "address where agents on other machines connect, for example :9445 (default: none)")
	fs.StringVar(&o.metricsListen, "metrics-listen", "127.0.0.1:9446", "address where Prometheus scrapes metrics, without TLS or tokens")
	fs.StringVar(&o.tlsNames, "tls-san", "", "extra host names and addresses for the server certificate, comma separated")
	fs.StringVar(&o.clientCADir, "allocation-client-ca-dir", "", "accept allocation clients with a certificate from a CA in this directory")
	fs.BoolVar(&o.noAgent, "no-agent", false, "do not run game servers on this machine")
	fs.DurationVar(&o.graces.Unallocated, "node-lost-grace", nodes.DefaultGraces.Unallocated, "how long a disconnected node's idle game servers are waited for")
	fs.DurationVar(&o.graces.Allocated, "node-lost-allocated-grace", nodes.DefaultGraces.Allocated, "how long a disconnected node's allocated game servers are waited for")
	o.node.register(fs)
	if err := parseWithConfig(fs, args); err != nil {
		return nil, err
	}
	o.node.finish()
	return o, nil
}

// runServer starts the control plane and, unless told otherwise, an agent
// in the same process.
func runServer(ctx context.Context, args []string, out io.Writer) error {
	o, err := serverFlags(args)
	if err != nil {
		return err
	}
	if err := agones.Setup(); err != nil {
		return err
	}
	if err := os.MkdirAll(o.dataDir, 0o700); err != nil {
		return err
	}
	database := filepath.Join(o.dataDir, "state.db")
	s, err := store.Open(database, store.Kinds())
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	recorder, err := events.Open(database)
	if err != nil {
		return err
	}
	defer func() { _ = recorder.Close() }()
	secrets, err := auth.Load(o.dataDir, strings.Split(o.tlsNames, ","))
	if err != nil {
		return err
	}

	parts, err := o.parts(ctx, s, recorder, secrets)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "scrim: management API on https://%s, allocation API on https://%s\n", o.listen, o.allocationListen)
	_, _ = fmt.Fprintf(out, "scrim: tokens and the CA certificate are in %s\n", o.dataDir)
	return runAll(ctx, parts...)
}

// parts lists everything the server runs side by side.
func (o *serverOptions) parts(ctx context.Context, s *store.Store, recorder *events.Recorder, secrets *auth.Secrets) ([]part, error) {
	measure := metrics.New(s)
	cp, err := controlplane.New(s, recorder, measure, o.graces)
	if err != nil {
		return nil, err
	}
	scrape := &http.Server{Addr: o.metricsListen, Handler: measure.Handler(), ReadHeaderTimeout: 10 * time.Second}
	allocation, err := allocationServer(ctx, o, cp, secrets)
	if err != nil {
		return nil, err
	}
	api := &http.Server{
		Addr:              o.listen,
		Handler:           exceptHealth(cp.API.Handler(), auth.RequireHTTP(secrets, auth.Admin, cp.API.Handler())),
		TLSConfig:         secrets.ServerTLS(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	parts := []part{
		func(ctx context.Context) error { s.Run(ctx); return nil },
		func(ctx context.Context) error { recorder.Run(ctx); return nil },
		func(ctx context.Context) error { cp.Run(ctx); return nil },
		func(ctx context.Context) error { return serveTLS(ctx, api) },
		func(ctx context.Context) error { return serveTLS(ctx, allocation) },
		func(ctx context.Context) error { return servePlain(ctx, scrape) },
		func(ctx context.Context) error {
			return every(ctx, time.Hour, func() error { return recorder.Prune(eventRetention) })
		},
		func(ctx context.Context) error { return every(ctx, tokenReload, secrets.Reload) },
	}
	if o.nodeListen != "" {
		parts = append(parts, func(ctx context.Context) error {
			return serveAgents(ctx, o.nodeListen, nodes.NewServer(s, cp.Agents), secrets)
		})
	}
	if o.noAgent {
		return parts, nil
	}
	daemon, err := o.node.connect(ctx)
	if err != nil {
		return nil, err
	}
	local := agent.New(o.node.agent, cp.Agents, daemon)
	cp.Agents.Attach(o.node.agent.NodeName, local) // its logs are read directly
	return append(parts, local.Run), nil
}

// exceptHealth serves /healthz without asking for a token, so that load
// balancers and service managers can check on the process.
func exceptHealth(open, protected http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			open.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func allocationServer(ctx context.Context, o *serverOptions, cp *controlplane.ControlPlane, secrets *auth.Secrets) (*http.Server, error) {
	handler, err := cp.Allocation.Handler(ctx)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := secrets.ServerTLSWithClients(o.clientCADir)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              o.allocationListen,
		Handler:           auth.RequireHTTP(secrets, auth.Allocator, handler),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

// serveTLS runs an HTTPS server until ctx is done.
func serveTLS(ctx context.Context, server *http.Server) error {
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// servePlain runs an HTTP server without TLS until ctx is done.
func servePlain(ctx context.Context, server *http.Server) error {
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// serveAgents runs the gRPC service that agents on other machines dial.
func serveAgents(ctx context.Context, address string, service nodev1.NodesServer, secrets *auth.Secrets) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	//nolint:contextcheck // what guards a call goes by the context of that call, not by ours
	options := append(auth.RequireGRPC(secrets, auth.Node),
		grpc.Creds(credentials.NewTLS(secrets.ServerTLS())),
		// Agents ping to notice a lost control plane; allow them to.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 10 * time.Second, Timeout: 5 * time.Second}),
	)
	server := grpc.NewServer(options...)
	nodev1.RegisterNodesServer(server, service)
	go func() {
		<-ctx.Done()
		server.Stop()
	}()
	return server.Serve(listener)
}

// every calls fn now and then again each period, until ctx is done or fn fails.
func every(ctx context.Context, period time.Duration, fn func() error) error {
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		if err := fn(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// runAll runs the parts of a process side by side. When one fails or ctx
// ends, the others are told to stop and runAll returns the first error.
func runAll(ctx context.Context, parts ...part) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(parts))
	for _, run := range parts {
		go func() { results <- run(ctx) }()
	}
	var first error
	for range parts {
		if err := <-results; err != nil && first == nil {
			first = err
		}
		cancel()
	}
	return first
}
