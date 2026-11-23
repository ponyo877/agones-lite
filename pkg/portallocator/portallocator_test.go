package portallocator

import (
	"math"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
)

var ports7000to7002 = litev1.PortRange{Min: 7000, Max: 7002}

func server(ports ...agonesv1.GameServerPort) *agonesv1.GameServer {
	return &agonesv1.GameServer{Spec: agonesv1.GameServerSpec{Ports: ports}}
}

func port(policy agonesv1.PortPolicy, protocol corev1.Protocol) agonesv1.GameServerPort {
	return agonesv1.GameServerPort{Name: "game", PortPolicy: policy, Protocol: protocol, ContainerPort: 7654}
}

func TestAllocate(t *testing.T) {
	tests := map[string]struct {
		gs      *agonesv1.GameServer
		used    map[int32]bool
		ok      bool
		host    []int32
		contain []int32
	}{
		"dynamic takes the lowest free port": {
			gs:   server(port(agonesv1.Dynamic, corev1.ProtocolUDP)),
			used: map[int32]bool{7000: true},
			ok:   true, host: []int32{7001}, contain: []int32{7654},
		},
		"passthrough uses the host port inside the container too": {
			gs: server(port(agonesv1.Passthrough, corev1.ProtocolUDP)),
			ok: true, host: []int32{7000}, contain: []int32{7000},
		},
		"none gets no host port": {
			gs: server(port(agonesv1.None, corev1.ProtocolUDP)),
			ok: true, host: []int32{0}, contain: []int32{7654},
		},
		"a full range fails": {
			gs:   server(port(agonesv1.Dynamic, corev1.ProtocolUDP)),
			used: map[int32]bool{7000: true, 7001: true, 7002: true},
			ok:   false, host: []int32{0}, contain: []int32{7654},
		},
		"dynamic keeps off the game server's own static port": {
			gs: server(
				agonesv1.GameServerPort{Name: "admin", PortPolicy: agonesv1.Static, HostPort: 7000, ContainerPort: 8080},
				port(agonesv1.Dynamic, corev1.ProtocolUDP),
				port(agonesv1.Passthrough, corev1.ProtocolUDP),
			),
			ok: true, host: []int32{7000, 7001, 7002}, contain: []int32{8080, 7654, 7002},
		},
		"a static port next to a dynamic one, in a range of two": {
			gs: server(
				agonesv1.GameServerPort{PortPolicy: agonesv1.Static, HostPort: 7000},
				port(agonesv1.Dynamic, corev1.ProtocolUDP),
			),
			ok: true, host: []int32{7000, 7001}, contain: []int32{0, 7654},
		},
		"its own static ports count against the range": {
			gs: server(
				agonesv1.GameServerPort{PortPolicy: agonesv1.Static, HostPort: 7000},
				agonesv1.GameServerPort{PortPolicy: agonesv1.Static, HostPort: 7001},
				agonesv1.GameServerPort{PortPolicy: agonesv1.Static, HostPort: 7002},
				port(agonesv1.Dynamic, corev1.ProtocolUDP),
			),
			ok: false, host: []int32{7000, 7001, 7002, 0}, contain: []int32{0, 0, 0, 7654},
		},
		"a taken static port fails": {
			gs:   server(agonesv1.GameServerPort{PortPolicy: agonesv1.Static, HostPort: 9000}),
			used: map[int32]bool{9000: true},
			ok:   false, host: []int32{9000}, contain: []int32{0},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.ok, Allocate(tc.gs, ports7000to7002, tc.used))
			var host, contain []int32
			for _, p := range tc.gs.Spec.Ports {
				host = append(host, p.HostPort)
				contain = append(contain, p.ContainerPort)
			}
			assert.Equal(t, tc.host, host)
			assert.Equal(t, tc.contain, contain)
		})
	}
}

func TestTCPUDPBecomesTwoPortsOnOneHostPort(t *testing.T) {
	gs := server(port(agonesv1.Dynamic, agonesv1.ProtocolTCPUDP))

	assert.True(t, Allocate(gs, ports7000to7002, nil))

	assert.Len(t, gs.Spec.Ports, 2)
	tcp, udp := gs.Spec.Ports[0], gs.Spec.Ports[1]
	assert.Equal(t, "game-tcp", tcp.Name)
	assert.Equal(t, corev1.ProtocolTCP, tcp.Protocol)
	assert.Equal(t, "game-udp", udp.Name)
	assert.Equal(t, corev1.ProtocolUDP, udp.Protocol)
	assert.Equal(t, tcp.HostPort, udp.HostPort)
}

func TestUsedCountsATCPUDPPortOnce(t *testing.T) {
	a := server(port(agonesv1.Dynamic, agonesv1.ProtocolTCPUDP))
	b := server(port(agonesv1.Dynamic, corev1.ProtocolUDP), port(agonesv1.None, corev1.ProtocolUDP))
	assert.True(t, Allocate(a, ports7000to7002, nil))
	assert.True(t, Allocate(b, ports7000to7002, Used([]*agonesv1.GameServer{a})))

	assert.Equal(t, map[int32]bool{7000: true, 7001: true}, Used([]*agonesv1.GameServer{a, b}))
	// Removing a frees exactly its own port.
	assert.Equal(t, map[int32]bool{7001: true}, Used([]*agonesv1.GameServer{b}))
}

// Found by TestAllocatedPortsAreFreeDistinctAndInRange once its ranges
// reached the end of int32: the loop over the range counted on past it, to
// the smallest int32, and handed that out as the third port.
func TestARangeThatEndsWhereInt32DoesHasNoPortsBeyondIt(t *testing.T) {
	lastTwo := litev1.PortRange{Min: math.MaxInt32 - 1, Max: math.MaxInt32}
	dynamic := port(agonesv1.Dynamic, corev1.ProtocolUDP)

	two := server(dynamic, dynamic)
	assert.True(t, Allocate(two, lastTwo, nil))
	assert.Equal(t, []int32{math.MaxInt32 - 1, math.MaxInt32}, []int32{two.Spec.Ports[0].HostPort, two.Spec.Ports[1].HostPort})

	three := server(dynamic, dynamic, dynamic)
	assert.False(t, Allocate(three, lastTwo, nil), "the range has two ports")
}
