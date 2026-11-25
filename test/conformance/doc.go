// Package conformance checks the promise scrim makes to game servers:
// one that was written for Agones runs unchanged.
//
// The tests talk to the sidecar the way a game server does, through the
// official Go SDK and through REST, and everything behind the sidecar is
// the real thing: agent, control plane and store. Only Docker is missing.
// In its place bench_test.go has a stand-in that keeps containers as
// records, except that "starting" a sidecar container really runs the SDK
// server, with the environment the agent gave that container.
//
// Start with sdk_test.go, which walks a game server through its whole life.
package conformance
