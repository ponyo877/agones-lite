// Package scheduling decides which node a GameServer runs on.
//
// It is one pure function, Place, and a Room for placing many GameServers
// in a row. It does the part of kube-scheduler's job that Agones depends
// on: skip nodes that cannot take the GameServer, then prefer full nodes
// (Packed) or empty ones (Distributed).
//
// Upstream counterpart: none. Agones asks kube-scheduler for Packed by adding
// a pod affinity in GameServer.Pod (pkg/apis/agones/v1/gameserver.go).
package scheduling

import (
	"cmp"
	"slices"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/portallocator"
)

// Place picks a node for gs and gives it host ports there. It returns a
// changed copy of gs, or false when no node can take it right now.
//
// servers is every GameServer in the system; the ones already on a node are
// what make that node full.
func Place(gs *agonesv1.GameServer, nodes []*litev1.Node, servers []*agonesv1.GameServer) (*agonesv1.GameServer, bool) {
	return RoomOn(nodes, servers).Place(gs)
}

// Room is what the nodes have left to give, kept current while GameServers
// are placed one after the other. Finding out what every node holds takes a
// look at every GameServer there is. A Room takes that look once, for as
// many placements as follow: a Fleet of ten thousand is placed with one
// look instead of ten thousand.
type Room struct {
	lots []*lot
}

// lot is one node and what is on it.
type lot struct {
	node        *litev1.Node
	residents   int
	cpu, memory resource.Quantity // what the residents ask for, together
	ports       map[int32]bool    // the host ports they have
	// free is the range host ports are still handed out from. It begins as
	// the node's range and loses its low end as ports are given: they are
	// given lowest first, so what lies below the last one given is taken.
	// Without it, every placement would walk past all the ports before it.
	free litev1.PortRange
}

// RoomOn counts what servers leave free on nodes.
func RoomOn(nodes []*litev1.Node, servers []*agonesv1.GameServer) *Room {
	room := &Room{}
	byName := map[string]*lot{}
	for _, node := range nodes {
		l := &lot{node: node, ports: map[int32]bool{}, free: node.Status.PortRange}
		byName[node.Name] = l
		room.lots = append(room.lots, l)
	}
	for _, gs := range servers {
		if l, known := byName[gs.Status.NodeName]; known {
			l.take(gs)
		}
	}
	return room
}

// Place picks a node for gs and gives it host ports there, as the function
// Place does, and counts the result in: the next GameServer finds the node
// that much fuller.
func (r *Room) Place(gs *agonesv1.GameServer) (*agonesv1.GameServer, bool) {
	for _, l := range r.ranked(gs.Spec.Scheduling) {
		if !l.fits(gs) {
			continue
		}
		l.passTaken()
		placed := gs.DeepCopy()
		if !portallocator.Allocate(placed, l.free, l.ports) {
			continue
		}
		placed.Status.NodeName = l.node.Name
		l.take(placed)
		l.free.Min = max(l.free.Min, portallocator.Highest(placed)+1)
		return placed, true
	}
	return nil, false
}

// ranked orders the nodes from most to least preferred: fullest first for
// Packed, emptiest first for Distributed. Ties go by name, so the result is
// stable.
func (r *Room) ranked(strategy apis.SchedulingStrategy) []*lot {
	out := slices.Clone(r.lots)
	slices.SortFunc(out, func(a, b *lot) int {
		byFill := cmp.Compare(b.residents, a.residents)
		if strategy == apis.Distributed {
			byFill = -byFill
		}
		return cmp.Or(byFill, cmp.Compare(a.node.Name, b.node.Name))
	})
	return out
}

// passTaken moves the low end of the free range past the ports that are
// taken. A node whose ports have all gone is then seen to be full at a
// glance: Packed tries the fullest nodes first, and would otherwise walk
// the whole range of each of them for every GameServer it places.
func (l *lot) passTaken() {
	for l.free.Min < l.free.Max && l.ports[l.free.Min] {
		l.free.Min++
	}
}

// take counts gs among the residents.
func (l *lot) take(gs *agonesv1.GameServer) {
	l.residents++
	cpu, memory := Requests(gs)
	l.cpu.Add(cpu)
	l.memory.Add(memory)
	portallocator.Take(l.ports, gs)
}

// fits reports whether the node has room for gs, ports aside.
func (l *lot) fits(gs *agonesv1.GameServer) bool {
	// A node that is being drained shuts down every GameServer without a
	// session, so one placed there would be told to go at once, and its
	// set would make another. Draining a node cordons it as well, but the
	// two are separate fields, and Schedulable only reads the one.
	if !l.node.Schedulable() {
		return false
	}
	if limit := int(l.node.Status.MaxGameServers); limit > 0 && l.residents >= limit {
		return false
	}
	cpu, memory := Requests(gs)
	cpu.Add(l.cpu)
	memory.Add(l.memory)
	return within(cpu, l.node.Status.Capacity.CPU) && within(memory, l.node.Status.Capacity.Memory)
}

// within treats a zero capacity as "no limit".
func within(wanted, capacity resource.Quantity) bool {
	return capacity.IsZero() || wanted.Cmp(capacity) <= 0
}

// Requests sums the CPU and memory requests of every container of gs.
// Containers without requests count as zero.
func Requests(gs *agonesv1.GameServer) (cpu, memory resource.Quantity) {
	for _, c := range gs.Spec.Template.Spec.Containers {
		cpu.Add(c.Resources.Requests[corev1.ResourceCPU])
		memory.Add(c.Resources.Requests[corev1.ResourceMemory])
	}
	return cpu, memory
}
