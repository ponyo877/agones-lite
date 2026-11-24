// Package gameserverallocations chooses a GameServer for an allocation
// request and marks it Allocated.
//
// Start with find.go: Find narrows the GameServers down to the candidates
// and returns the one a selector accepts that comes first in the order the
// request's scheduling strategy wants. FindPacked finds the same one for
// Packed without a look at every GameServer. order.go has the two
// orderings, Packed and Distributed. apply.go has Apply, which is
// everything an allocation changes on the chosen GameServer. status.go
// writes the answer into the request.
//
// Every function here only looks at its arguments. Reading the store,
// writing the result of Apply back and trying again after a lost race is the
// caller's job. So is preparing the request, in upstream's order:
// ApplyDefaults, then Validate, then Converter.
//
// Upstream counterparts, all in pkg/gameserverallocations: find.go
// (findGameServerForAllocation), allocation_cache.go (which GameServers are
// candidates, and the two orderings) and allocator.go
// (applyAllocationToGameServer, allocateFromLocalCluster). Upstream answers
// Contention when its cache and its batch of requests disagree. Nothing is
// cached or batched here, so that answer has no counterpart.
package gameserverallocations
