package portallocator

import (
	"encoding/json"
	"fmt"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/ponyo877/agones-lite/formal/vectors"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
)

// modelPort is a port as the Lean model sees it: "automatic", or Static
// with its host port.
type modelPort struct {
	static *int32
}

func (p *modelPort) UnmarshalJSON(raw []byte) error {
	if string(raw) == `"automatic"` {
		return nil
	}
	var port struct {
		Static int32 `json:"static"`
	}
	p.static = &port.Static
	return json.Unmarshal(raw, &port)
}

// portVector is one answer of the model in formal/Formal/Ports.lean.
type portVector struct {
	Ports     []modelPort `json:"ports"`
	Lo        int32       `json:"lo"`
	Hi        int32       `json:"hi"`
	Used      []int32     `json:"used"`
	HostPorts *[]int32    `json:"hostPorts"` // nil when the model allocates nothing
}

func (v portVector) gameServer() *agonesv1.GameServer {
	gs := &agonesv1.GameServer{}
	for i, p := range v.Ports {
		port := agonesv1.GameServerPort{Name: fmt.Sprint("port", i), ContainerPort: 7654, Protocol: corev1.ProtocolUDP}
		port.PortPolicy = agonesv1.Dynamic
		if p.static != nil {
			port.PortPolicy, port.HostPort = agonesv1.Static, *p.static
		}
		gs.Spec.Ports = append(gs.Spec.Ports, port)
	}
	return gs
}

func (v portVector) used() map[int32]bool {
	used := map[int32]bool{}
	for _, port := range v.Used {
		used[port] = true
	}
	return used
}

func hostPorts(gs *agonesv1.GameServer) []int32 {
	ports := []int32{}
	for _, p := range gs.Spec.Ports {
		ports = append(ports, p.HostPort)
	}
	return ports
}

// The theorems in formal/Formal/Ports.lean are about a model. This test is
// what ties them to Allocate: on every input the model was run on, the two
// must answer alike.
func TestAllocateAgreesWithTheLeanModel(t *testing.T) {
	all := vectors.Read[portVector](t, "ports.jsonl")
	require.NotEmpty(t, all)
	for i, v := range all {
		gs := v.gameServer()

		ok := Allocate(gs, litev1.PortRange{Min: v.Lo, Max: v.Hi}, v.used())

		if v.HostPorts == nil {
			require.False(t, ok, "vector %d: the model allocates nothing: %+v", i, v)
			continue
		}
		require.True(t, ok, "vector %d: the model allocates %v: %+v", i, *v.HostPorts, v)
		require.Equal(t, *v.HostPorts, hostPorts(gs), "vector %d: %+v", i, v)
	}
}
