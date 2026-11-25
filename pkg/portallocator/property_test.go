package portallocator

import (
	"fmt"
	"math"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"pgregory.net/rapid"

	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
)

// These tests state what must hold for any input and let rapid look for an
// input that breaks it. A failure is printed as the smallest such input.

// aPort draws a port number from a narrow band, so that collisions between
// Static ports, used ports and the range happen all the time.
func aPort() *rapid.Generator[int32] {
	return rapid.Int32Range(7000, 7012)
}

// aGameServer draws up to five ports, each Dynamic, Passthrough or Static.
// Its Static ports are distinct, as admission guarantees.
func aGameServer(t *rapid.T) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{}
	taken := map[int32]bool{}
	for i := range rapid.IntRange(0, 5).Draw(t, "ports") {
		port := agonesv1.GameServerPort{Name: fmt.Sprint("port", i), ContainerPort: 7654, Protocol: corev1.ProtocolUDP}
		port.PortPolicy = rapid.SampledFrom([]agonesv1.PortPolicy{agonesv1.Dynamic, agonesv1.Passthrough, agonesv1.Static}).Draw(t, "policy")
		if port.PortPolicy == agonesv1.Static {
			port.HostPort = aPort().Filter(func(p int32) bool { return !taken[p] }).Draw(t, "hostPort")
			taken[port.HostPort] = true
		}
		gs.Spec.Ports = append(gs.Spec.Ports, port)
	}
	return gs
}

// aRange draws a range from the narrow band, or now and then one that ends
// where int32 does.
func aRange(t *rapid.T) scrimv1.PortRange {
	if rapid.IntRange(0, 9).Draw(t, "atTheEnd") == 0 {
		return scrimv1.PortRange{Min: math.MaxInt32 - rapid.Int32Range(0, 3).Draw(t, "below"), Max: math.MaxInt32}
	}
	return scrimv1.PortRange{Min: aPort().Draw(t, "min"), Max: aPort().Draw(t, "max")}
}

func someUsedPorts(t *rapid.T) map[int32]bool {
	used := map[int32]bool{}
	for _, port := range rapid.SliceOfN(aPort(), 0, 8).Draw(t, "used") {
		used[port] = true
	}
	return used
}

// The theorem allocate_distinct in formal/Formal/Ports.lean, asked of the
// real function: after Allocate no two ports of the GameServer share a host
// port, none is held by anybody else, and the picked ones are in the range.
func TestAllocatedPortsAreFreeDistinctAndInRange(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gs, r, used := aGameServer(t), aRange(t), someUsedPorts(t)

		if !Allocate(gs, r, used) {
			return
		}

		seen := map[int32]bool{}
		for _, p := range gs.Spec.Ports {
			require.False(t, used[p.HostPort], "port %d is held by another GameServer", p.HostPort)
			require.False(t, seen[p.HostPort], "port %d was given out twice", p.HostPort)
			seen[p.HostPort] = true
			if p.PortPolicy != agonesv1.Static {
				assert.True(t, r.Min <= p.HostPort && p.HostPort <= r.Max, "port %d is outside %v", p.HostPort, r)
			}
		}
	})
}

// A refusal has one of two reasons, and leaves the GameServer as it was.
func TestAllocateOnlyRefusesForAReasonAndThenChangesNothing(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gs, r, used := aGameServer(t), aRange(t), someUsedPorts(t)
		before := gs.DeepCopy()

		if Allocate(gs, r, used) {
			return
		}

		assert.Equal(t, before, gs, "a refusal must not touch the GameServer")
		assert.True(t, !staticPortsFree(before, used) || tooFewFree(before, r, used),
			"refused although its Static ports were free and the range had room")
	})
}

// tooFewFree counts the free ports of the range the slow, obvious way.
func tooFewFree(gs *agonesv1.GameServer, r scrimv1.PortRange, used map[int32]bool) bool {
	static := map[int32]bool{}
	for _, p := range gs.Spec.Ports {
		if p.PortPolicy == agonesv1.Static {
			static[p.HostPort] = true
		}
	}
	free := 0
	for port := int64(r.Min); port <= int64(r.Max); port++ {
		if !used[int32(port)] && !static[int32(port)] {
			free++
		}
	}
	return free < countAutomatic(gs)
}
