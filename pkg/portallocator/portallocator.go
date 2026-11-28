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
	"cmp"
	"slices"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"

	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
)

// Used returns the host ports held by servers, which must all be on one node.
func Used(servers []*agonesv1.GameServer) map[int32]bool {
	used := map[int32]bool{}
	for _, gs := range servers {
		Take(used, gs)
	}
	return used
}

// Take adds the host ports of gs to used.
func Take(used map[int32]bool, gs *agonesv1.GameServer) {
	for _, p := range gs.Spec.Ports {
		if p.PortPolicy != agonesv1.None && p.HostPort != 0 {
			used[p.HostPort] = true
		}
	}
}

// Allocate gives every Dynamic and Passthrough port of gs a free host port
// from the range it names among ranges, then splits TCPUDP ports in two as
// upstream does.
//
// It reports false, leaving gs untouched, when a Static port is taken, a
// port names a range that is not among ranges, or a range has too few free
// ports.
//
// The ranges are picked from one after the other, and what was picked from
// one counts as taken in the next, as the GameServer's own Static ports do.
// The ranges of a node do not overlap, so that never stops a pick that
// could have been made in another order.
func Allocate(gs *agonesv1.GameServer, ranges map[string]scrimv1.PortRange, used map[int32]bool) bool {
	if !staticPortsFree(gs, used) {
		return false
	}
	taken := ownStatic(gs)
	picked := map[string][]int32{}
	for _, name := range Named(gs) {
		r, known := ranges[name]
		if !known {
			return false
		}
		free, ok := pickFree(countIn(gs, name), r, used, taken)
		if !ok {
			return false
		}
		picked[name] = free
		taken = append(taken, free...)
	}
	assign(gs, picked)
	splitTCPUDP(gs)
	return true
}

// Named returns the names of the ranges the Dynamic and Passthrough ports
// of gs take their host ports from, sorted.
func Named(gs *agonesv1.GameServer) []string {
	var names []string
	for _, p := range gs.Spec.Ports {
		if automatic(p) && !slices.Contains(names, rangeOf(p)) {
			names = append(names, rangeOf(p))
		}
	}
	slices.Sort(names)
	return names
}

// Highest returns the highest host port Allocate has given gs from each
// range. A range it gave none from is not there.
func Highest(gs *agonesv1.GameServer) map[string]int32 {
	highest := map[string]int32{}
	for _, p := range gs.Spec.Ports {
		if automatic(p) {
			highest[rangeOf(p)] = max(highest[rangeOf(p)], p.HostPort)
		}
	}
	return highest
}

// rangeOf is the name of the range a port takes its host port from.
// Admission fills in the default name; an object that skipped admission
// means the same by leaving it out.
func rangeOf(p agonesv1.GameServerPort) string {
	return cmp.Or(p.Range, agonesv1.DefaultPortRange)
}

// ownStatic returns the Static host ports of gs itself. They may lie in the
// node's range, and are then as taken as a neighbour's.
func ownStatic(gs *agonesv1.GameServer) []int32 {
	var ports []int32
	for _, p := range gs.Spec.Ports {
		if p.PortPolicy == agonesv1.Static {
			ports = append(ports, p.HostPort)
		}
	}
	return ports
}

func automatic(p agonesv1.GameServerPort) bool {
	return p.PortPolicy == agonesv1.Dynamic || p.PortPolicy == agonesv1.Passthrough
}

// countIn counts the Dynamic and Passthrough ports of gs that take their
// host port from the range named name.
func countIn(gs *agonesv1.GameServer, name string) int {
	n := 0
	for _, p := range gs.Spec.Ports {
		if automatic(p) && rangeOf(p) == name {
			n++
		}
	}
	return n
}

func staticPortsFree(gs *agonesv1.GameServer, used map[int32]bool) bool {
	for _, p := range gs.Spec.Ports {
		if p.PortPolicy == agonesv1.Static && used[p.HostPort] {
			return false
		}
	}
	return true
}

// pickFree returns the n lowest ports in r that are neither used nor
// among own.
func pickFree(n int, r scrimv1.PortRange, used map[int32]bool, own []int32) ([]int32, bool) {
	free := make([]int32, 0, n)
	// Counted as int64: an int32 that has reached a range's end at the
	// largest int32 starts again at the smallest, far outside the range.
	for port := int64(r.Min); port <= int64(r.Max) && len(free) < n; port++ {
		if p := int32(port); !used[p] && !slices.Contains(own, p) { //nolint:gosec // between r.Min and r.Max, which are int32
			free = append(free, p)
		}
	}
	return free, len(free) == n
}

// assign hands out what was picked from each range, in order, to the
// ports that name it.
func assign(gs *agonesv1.GameServer, picked map[string][]int32) {
	for i := range gs.Spec.Ports {
		p := &gs.Spec.Ports[i]
		if !automatic(*p) {
			continue
		}
		name := rangeOf(*p)
		p.HostPort, picked[name] = picked[name][0], picked[name][1:]
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
