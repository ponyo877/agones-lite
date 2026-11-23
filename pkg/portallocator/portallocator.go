// Package portallocator gives GameServers host ports on a node.
//
// It is a set of pure functions. Which ports are taken is never stored: it is
// worked out from the GameServers already on the node, so a port can neither
// leak nor be counted twice.
//
// Upstream counterpart: pkg/portallocator. Upstream keeps a table of ports per
// node "slot" and leaves the choice of the real node to kube-scheduler. Here
// the node is already chosen, so ports are picked on that node directly.
package portallocator

import (
	"maps"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
)

// Used returns the host ports held by servers, which must all be on one node.
func Used(servers []*agonesv1.GameServer) map[int32]bool {
	used := map[int32]bool{}
	for _, gs := range servers {
		for _, p := range gs.Spec.Ports {
			if p.PortPolicy != agonesv1.None && p.HostPort != 0 {
				used[p.HostPort] = true
			}
		}
	}
	return used
}

// Allocate gives every Dynamic and Passthrough port of gs a free host port
// from r, then splits TCPUDP ports in two as upstream does.
//
// It reports false, leaving gs untouched, when a Static port is taken or the
// range has too few free ports.
func Allocate(gs *agonesv1.GameServer, r litev1.PortRange, used map[int32]bool) bool {
	if !staticPortsFree(gs, used) {
		return false
	}
	free, ok := pickFree(countAutomatic(gs), r, withStatic(used, gs))
	if !ok {
		return false
	}
	assign(gs, free)
	splitTCPUDP(gs)
	return true
}

// withStatic returns used plus the Static host ports of gs itself. They may
// lie in the node's range, and are then as taken as a neighbour's.
func withStatic(used map[int32]bool, gs *agonesv1.GameServer) map[int32]bool {
	taken := map[int32]bool{}
	maps.Copy(taken, used)
	for _, p := range gs.Spec.Ports {
		if p.PortPolicy == agonesv1.Static {
			taken[p.HostPort] = true
		}
	}
	return taken
}

func automatic(p agonesv1.GameServerPort) bool {
	return p.PortPolicy == agonesv1.Dynamic || p.PortPolicy == agonesv1.Passthrough
}

func countAutomatic(gs *agonesv1.GameServer) int {
	return gs.CountPorts(func(policy agonesv1.PortPolicy) bool {
		return policy == agonesv1.Dynamic || policy == agonesv1.Passthrough
	})
}

func staticPortsFree(gs *agonesv1.GameServer, used map[int32]bool) bool {
	for _, p := range gs.Spec.Ports {
		if p.PortPolicy == agonesv1.Static && used[p.HostPort] {
			return false
		}
	}
	return true
}

// pickFree returns the n lowest free ports in r.
func pickFree(n int, r litev1.PortRange, used map[int32]bool) ([]int32, bool) {
	free := make([]int32, 0, n)
	// Counted as int64: an int32 that has reached a range's end at the
	// largest int32 starts again at the smallest, far outside the range.
	for port := int64(r.Min); port <= int64(r.Max) && len(free) < n; port++ {
		if !used[int32(port)] {
			free = append(free, int32(port))
		}
	}
	return free, len(free) == n
}

func assign(gs *agonesv1.GameServer, free []int32) {
	for i := range gs.Spec.Ports {
		p := &gs.Spec.Ports[i]
		if !automatic(*p) {
			continue
		}
		p.HostPort, free = free[0], free[1:]
		if p.PortPolicy == agonesv1.Passthrough {
			p.ContainerPort = p.HostPort
		}
	}
}

// splitTCPUDP turns each TCPUDP port into a "-tcp" and a "-udp" port that
// share one host port. The TCP half stays in place; the UDP half goes last.
func splitTCPUDP(gs *agonesv1.GameServer) {
	var udp []agonesv1.GameServerPort
	for i := range gs.Spec.Ports {
		p := &gs.Spec.Ports[i]
		if p.Protocol != agonesv1.ProtocolTCPUDP {
			continue
		}
		half := *p
		half.Name += "-udp"
		half.Protocol = corev1.ProtocolUDP
		udp = append(udp, half)

		p.Name += "-tcp"
		p.Protocol = corev1.ProtocolTCP
	}
	gs.Spec.Ports = append(gs.Spec.Ports, udp...)
}
