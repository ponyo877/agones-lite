// Package gameserversets keeps a GameServerSet at its desired size.
//
// Start with plan.go: Plan compares spec.replicas with the GameServers a set
// owns and says how many to create and which ones to remove. sort.go decides
// which GameServers go first when there are too many. status.go counts them
// for the set's status.
//
// Every function here only looks at its arguments. Reading the store and
// carrying out what Plan decided is the caller's job.
//
// Upstream counterparts, both in pkg/gameserversets: controller.go
// (computeReconciliationAction, computeStatus) and gameserversets.go
// (SortGameServersByStrategy).
package gameserversets
