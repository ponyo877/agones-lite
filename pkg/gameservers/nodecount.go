package gameservers

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

	"github.com/ponyo877/scrim/pkg/store"
)

// NodeCount is how many Ready and Allocated GameServers a node holds.
// Packed allocation and scale-down both rank nodes by these two numbers.
type NodeCount struct {
	Ready     int64
	Allocated int64
}

// CountByNode counts the Ready and Allocated GameServers on each node.
//
// Upstream: PerNodeCounter in pkg/gameservers/pernodecounter.go keeps the
// same numbers up to date from Kubernetes events. Here they are simply
// counted when needed.
func CountByNode(servers []*agonesv1.GameServer) map[string]NodeCount {
	counts := map[string]NodeCount{}
	for _, gs := range servers {
		node := gs.Status.NodeName
		if node == "" {
			continue
		}
		// Like upstream, a node only has an entry once it holds a Ready or
		// Allocated GameServer.
		switch count := counts[node]; gs.Status.State {
		case agonesv1.GameServerStateReady:
			count.Ready++
			counts[node] = count
		case agonesv1.GameServerStateAllocated:
			count.Allocated++
			counts[node] = count
		}
	}
	return counts
}

// spot is a node and a state. The store keeps the GameServers sorted by
// the two, for those who ask how full every node is, or for the Ready ones
// of a single node.
type spot struct {
	node  string
	state agonesv1.GameServerState
}

var bySpot = store.By[*agonesv1.GameServer, spot]{
	Name: "node and state",
	Of:   func(gs *agonesv1.GameServer) spot { return spot{gs.Status.NodeName, gs.Status.State} },
}

// CountOnNodes is CountByNode of every GameServer in the store, without a
// look at each of them.
func CountOnNodes(servers store.Typed[*agonesv1.GameServer]) map[string]NodeCount {
	counts := map[string]NodeCount{}
	for at, n := range store.GroupSizes(servers, bySpot) {
		if at.node == "" {
			continue
		}
		switch count := counts[at.node]; at.state {
		case agonesv1.GameServerStateReady:
			count.Ready = int64(n)
			counts[at.node] = count
		case agonesv1.GameServerStateAllocated:
			count.Allocated = int64(n)
			counts[at.node] = count
		}
	}
	return counts
}

// OnNodeIn returns the GameServers on a node that are in a state. Like
// View it does not copy: read them, never change them.
func OnNodeIn(servers store.Typed[*agonesv1.GameServer], node string, state agonesv1.GameServerState) []*agonesv1.GameServer {
	return store.Group(servers, bySpot, spot{node, state})
}

// OneOnNodeIn returns one of the GameServers OnNodeIn returns, by its place
// among them: for choosing one by chance without a look at the others. See
// store.Member.
func OneOnNodeIn(servers store.Typed[*agonesv1.GameServer], node string, state agonesv1.GameServerState, place int) (*agonesv1.GameServer, bool) {
	return store.Member(servers, bySpot, spot{node, state}, place)
}
