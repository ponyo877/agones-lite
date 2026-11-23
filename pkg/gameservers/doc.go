// Package gameservers moves a GameServer through its life.
//
// Start with state.go: Next is the whole state machine in one table. Things
// that happen to a GameServer are Events; Next says which state an Event
// leads to. Only the control plane changes a state. Agents and sidecars
// report Events, and the Controller turns them into state changes.
//
// controller.go has the two entry points. Report applies an Event. The sync
// loop does what nobody reports: it places new GameServers on a node, turns
// RequestReady into Ready, and cleans up after Shutdown.
//
// Upstream counterparts, all in pkg/gameservers: controller.go (the sync
// loop), health.go, missing.go and succeeded.go (container exits, here the
// Exited and ContainersMissing events).
package gameservers
