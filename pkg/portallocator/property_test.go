package portallocator

import (
	"fmt"
	"math"
	"slices"
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
// Static ports, used ports and the ranges happen all the time.
func aPort() *rapid.Generator[int32] {
	return rapid.Int32Range(7000, 7025)
}

// aGameServer draws up to five ports, each Dynamic, Passthrough or Static.
// Its Static ports are distinct, as admission guarantees. A Dynamic or
// Passthrough port names the default range, or "game", or leaves the name
// out.
func aGameServer(t *rapid.T) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{}
	taken := map[int32]bool{}
	for i := range rapid.IntRange(0, 5).Draw(t, "ports") {
		port := agonesv1.GameServerPort{Name: fmt.Sprint("port", i), ContainerPort: 7654, Protocol: corev1.ProtocolUDP}
		port.PortPolicy = rapid.SampledFrom([]agonesv1.PortPolicy{agonesv1.Dynamic, agonesv1.Passthrough, agonesv1.Static}).Draw(t, "policy")
		if port.PortPolicy == agonesv1.Static {
			port.HostPort = aPort().Filter(func(p int32) bool { return !taken[p] }).Draw(t, "hostPort")
			taken[port.HostPort] = true
		} else {
			port.Range = rapid.SampledFrom([]string{"", agonesv1.DefaultPortRange, "game"}).Draw(t, "range")
		}
		gs.Spec.Ports = append(gs.Spec.Ports, port)
	}
	return gs
}

// someRanges draws the ranges of a node: the default one from the lower
// half of the band, or now and then one that ends where int32 does, and
// most of the time a range named "game" from the upper half. They do not
// overlap, as the agent makes sure.
func someRanges(t *rapid.T) map[string]scrimv1.PortRange {
	ranges := map[string]scrimv1.PortRange{agonesv1.DefaultPortRange: aRange(t, "default", 7000, 7012)}
	if rapid.IntRange(0, 9).Draw(t, "atTheEnd") == 0 {
		ranges[agonesv1.DefaultPortRange] = scrimv1.PortRange{Min: math.MaxInt32 - rapid.Int32Range(0, 3).Draw(t, "below"), Max: math.MaxInt32}
	}
	if rapid.IntRange(0, 3).Draw(t, "noGame") != 0 {
		ranges["game"] = aRange(t, "game", 7013, 7025)
	}
	return ranges
}

func aRange(t *rapid.T, name string, lo, hi int32) scrimv1.PortRange {
	return scrimv1.PortRange{Min: rapid.Int32Range(lo, hi).Draw(t, name+"Min"), Max: rapid.Int32Range(lo, hi).Draw(t, name+"Max")}
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
// port, none is held by anybody else, and the picked ones are in the range
// they name. The theorem is about one range; with more, what was picked
// from one counts as taken in the next, as the GameServer's own Static
// ports do.
func TestAllocatedPortsAreFreeDistinctAndInRange(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gs, ranges, used := aGameServer(t), someRanges(t), someUsedPorts(t)

		if !Allocate(gs, ranges, used) {
			return
		}

		seen := map[int32]bool{}
		for _, p := range gs.Spec.Ports {
			require.False(t, used[p.HostPort], "port %d is held by another GameServer", p.HostPort)
			require.False(t, seen[p.HostPort], "port %d was given out twice", p.HostPort)
			seen[p.HostPort] = true
			if r := ranges[rangeOf(p)]; p.PortPolicy != agonesv1.Static {
				assert.True(t, r.Min <= p.HostPort && p.HostPort <= r.Max, "port %d is outside %v", p.HostPort, r)
			}
		}
	})
}

// A refusal has one of three reasons, and leaves the GameServer as it was.
func TestAllocateOnlyRefusesForAReasonAndThenChangesNothing(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gs, ranges, used := aGameServer(t), someRanges(t), someUsedPorts(t)
		before := gs.DeepCopy()

		if Allocate(gs, ranges, used) {
			return
		}

		assert.Equal(t, before, gs, "a refusal must not touch the GameServer")
		assert.True(t, !staticPortsFree(before, used) || slices.ContainsFunc(Named(before), func(name string) bool {
			r, known := ranges[name]
			return !known || tooFewFree(before, name, r, used)
		}), "refused although its Static ports were free and every range it names had room")
	})
}

// tooFewFree counts the free ports of a range the slow, obvious way.
func tooFewFree(gs *agonesv1.GameServer, name string, r scrimv1.PortRange, used map[int32]bool) bool {
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
	return free < countIn(gs, name)
}
