// Package fleetautoscalers works out how many GameServers a Fleet should have.
//
// Start with desired.go: Desired applies the policy of a FleetAutoscaler to
// a Fleet and returns the number of replicas the policy wants. Each policy
// has a file of its own: buffer.go, capacity.go (Counter and List),
// webhook.go, schedule.go and chain.go. status.go fills in the
// FleetAutoscaler's status after a pass.
//
// Every function here only looks at its arguments. Reading the store,
// calling the webhook, writing the answer to the Fleet's spec.replicas and
// recording events is the caller's job.
//
// Upstream counterparts, both in pkg/fleetautoscalers: fleetautoscalers.go
// (computeDesiredFleetSize and the apply...Policy functions) and
// controller.go (updateStatus, updateStatusUnableToScale). The feature gates
// are fixed at their defaults, so Schedule and Chain always work and the
// Wasm policy does not exist.
package fleetautoscalers
