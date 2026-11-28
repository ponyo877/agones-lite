package gameservers

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
)

// A development GameServer stands for a game that runs on a developer's
// machine, outside scrim. Upstream registers one when a GameServer has the
// annotation agones.dev/dev-address, so that it can be allocated like any
// other while its game is changed and run again by hand. Nothing is run
// for it, and it is on no node. It is not one waiting for a node either
// (waitingForANode leaves it out), and once deleted, decide gives it up at
// once, as it does any GameServer without a node.
//
// Upstream: syncDevelopmentGameServer in pkg/gameservers/controller.go.

// Development returns the address of a development GameServer.
func Development(gs *agonesv1.GameServer) (address string, ok bool) {
	return gs.GetDevAddress()
}

// Register gives a development GameServer the status upstream gives it:
// Ready, at the address of the annotation, on the host ports of its
// Static ports. Upstream also writes the address where the node's name
// goes. Here that stays empty: the name of a node is what agents and the
// node controller go by, and no node runs this GameServer.
func Register(gs *agonesv1.GameServer, address string) {
	gs.Status.State = agonesv1.GameServerStateReady
	gs.Status.Address = address
	gs.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}
	gs.Status.Ports = nil
	for _, p := range gs.Spec.Ports {
		gs.Status.Ports = append(gs.Status.Ports, p.Status())
	}
}
