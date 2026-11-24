package gameservers

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
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
