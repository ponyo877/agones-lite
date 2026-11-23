// Package scheduling decides which node a GameServer runs on.
//
// It is one pure function, Place, plus helpers. It does the part of
// kube-scheduler's job that Agones depends on: skip nodes that cannot take
// the GameServer, then prefer full nodes (Packed) or empty ones (Distributed).
//
// Upstream counterpart: none. Agones asks kube-scheduler for Packed by adding
// a pod affinity in GameServer.Pod (pkg/apis/agones/v1/gameserver.go).
package scheduling

import (
	"sort"

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
	onNode := ByNode(servers)
	for _, node := range ranked(gs.Spec.Scheduling, nodes, onNode) {
		residents := onNode[node.Name]
		if !fits(gs, node, residents) {
			continue
		}
		placed := gs.DeepCopy()
		if !portallocator.Allocate(placed, node.Status.PortRange, portallocator.Used(residents)) {
			continue
		}
		placed.Status.NodeName = node.Name
		return placed, true
	}
	return nil, false
}

// ByNode groups the GameServers that have a node by that node's name.
func ByNode(servers []*agonesv1.GameServer) map[string][]*agonesv1.GameServer {
	out := map[string][]*agonesv1.GameServer{}
	for _, gs := range servers {
		if gs.Status.NodeName != "" {
			out[gs.Status.NodeName] = append(out[gs.Status.NodeName], gs)
		}
	}
	return out
}

// ranked orders nodes from most to least preferred: fullest first for Packed,
// emptiest first for Distributed. Ties go by name, so the result is stable.
func ranked(strategy apis.SchedulingStrategy, nodes []*litev1.Node, onNode map[string][]*agonesv1.GameServer) []*litev1.Node {
	out := append([]*litev1.Node(nil), nodes...)
	sort.Slice(out, func(i, j int) bool {
		a, b := len(onNode[out[i].Name]), len(onNode[out[j].Name])
		if a == b {
			return out[i].Name < out[j].Name
		}
		if strategy == apis.Distributed {
			return a < b
		}
		return a > b
	})
	return out
}

// fits reports whether node has room for gs, ports aside.
func fits(gs *agonesv1.GameServer, node *litev1.Node, residents []*agonesv1.GameServer) bool {
	if !node.Schedulable() {
		return false
	}
	if limit := int(node.Status.MaxGameServers); limit > 0 && len(residents) >= limit {
		return false
	}
	cpu, memory := Requests(gs)
	for _, r := range residents {
		c, m := Requests(r)
		cpu.Add(c)
		memory.Add(m)
	}
	return within(cpu, node.Status.Capacity.CPU) && within(memory, node.Status.Capacity.Memory)
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
