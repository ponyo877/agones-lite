// Package v1 holds the resources that exist only in agones-lite.
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
	// APIVersion is the apiVersion of every agones-lite resource.
	APIVersion = "agones-lite/v1"
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

// NodeSpec is what an operator asks of a node.
type NodeSpec struct {
	// Unschedulable stops new game servers and allocations from landing here.
	Unschedulable bool `json:"unschedulable,omitempty"`
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

// Schedulable reports whether new game servers may be placed on the node.
func (n *Node) Schedulable() bool {
	return n.Status.Phase == NodeReady && !n.Spec.Unschedulable
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
