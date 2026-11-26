// Package v1 holds the resources that exist only in scrim.
//
// Agones leans on the Kubernetes Node object. Without Kubernetes we keep our
// own, much smaller, record of each machine that runs game servers.
package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// APIVersion is the apiVersion of every scrim resource.
	APIVersion = "scrim/v1"
	// NodeKind is the kind of a Node.
	NodeKind = "Node"
)

// NodePhase says whether the control plane can currently reach a node.
type NodePhase string

const (
	// NodeReady means the node's agent is connected.
	NodeReady NodePhase = "Ready"
	// NodeDisconnected means the agent's connection was lost.
	NodeDisconnected NodePhase = "Disconnected"
)

// Node is one machine that runs game servers. Nodes have no namespace.
type Node struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeSpec   `json:"spec"`
	Status NodeStatus `json:"status"`
}

// NodeSpec is what an operator asks of a node. Its two switches spell one
// of three Duties: see Duty.
type NodeSpec struct {
	// Unschedulable stops new game servers and allocations from landing here.
	Unschedulable bool `json:"unschedulable,omitempty"`
	// Drain also shuts down the game servers here that have no session, so
	// the node empties as its sessions end.
	Drain bool `json:"drain,omitempty"`
}

// Duty is what an operator asks of a node, as one of three things.
type Duty string

const (
	// Open takes new game servers, and new sessions on the ones it has.
	Open Duty = "Open"
	// Cordoned takes neither. What runs there stays.
	Cordoned Duty = "Cordoned"
	// Draining takes neither, and sheds the game servers that have no
	// session, so the node empties as its sessions end.
	Draining Duty = "Draining"
)

// Duty reads the two switches of the spec. Drain decides by itself: a node
// that is being drained takes nothing new, whether or not it is cordoned as
// well.
func (n *Node) Duty() Duty {
	switch {
	case n.Spec.Drain:
		return Draining
	case n.Spec.Unschedulable:
		return Cordoned
	}
	return Open
}

// Ask sets the two switches of the spec the one way that spells duty.
func (n *Node) Ask(duty Duty) {
	n.Spec.Unschedulable, n.Spec.Drain = duty != Open, duty == Draining
}

// NodeStatus is what the node's agent reports about itself.
type NodeStatus struct {
	Phase NodePhase `json:"phase"`
	// Address is where players connect to reach game servers on this node.
	Address string `json:"address"`
	// Capacity limits the sum of resource requests. A zero value means no limit.
	Capacity Resources `json:"capacity,omitempty"`
	// MaxGameServers limits the number of game servers. Zero means no limit.
	MaxGameServers int32 `json:"maxGameServers,omitempty"`
	// PortRange is where Dynamic and Passthrough host ports come from.
	PortRange         PortRange   `json:"portRange"`
	LastHeartbeatTime metav1.Time `json:"lastHeartbeatTime,omitempty"`
	Version           string      `json:"version,omitempty"`
}

// Resources is an amount of CPU and memory.
type Resources struct {
	CPU    resource.Quantity `json:"cpu,omitempty"`
	Memory resource.Quantity `json:"memory,omitempty"`
}

// PortRange is an inclusive range of host ports.
type PortRange struct {
	Min int32 `json:"min"`
	Max int32 `json:"max"`
}

// Schedulable reports whether new game servers may be placed on the node,
// and new sessions on its game servers: its agent is connected, and no
// operator has asked otherwise.
func (n *Node) Schedulable() bool {
	return n.Status.Phase == NodeReady && n.Duty() == Open
}

// DeepCopy returns a copy that shares nothing with n.
func (n *Node) DeepCopy() *Node {
	out := *n
	out.ObjectMeta = *n.ObjectMeta.DeepCopy()
	out.Status.Capacity.CPU = n.Status.Capacity.CPU.DeepCopy()
	out.Status.Capacity.Memory = n.Status.Capacity.Memory.DeepCopy()
	out.Status.LastHeartbeatTime = *n.Status.LastHeartbeatTime.DeepCopy()
	return &out
}

// DeepCopyObject makes Node a runtime.Object, like the Agones types.
func (n *Node) DeepCopyObject() runtime.Object {
	return n.DeepCopy()
}
