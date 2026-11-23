package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestANodeTakesNewGameServersOnlyWhileItIsConnectedAndOpen(t *testing.T) {
	tests := map[string]struct {
		phase NodePhase
		spec  NodeSpec
		want  bool
	}{
		"connected and open":            {phase: NodeReady, want: true},
		"not connected":                 {phase: NodeDisconnected},
		"cordoned":                      {phase: NodeReady, spec: NodeSpec{Unschedulable: true}},
		"cordoned and draining":         {phase: NodeReady, spec: NodeSpec{Unschedulable: true, Drain: true}},
		"draining, though not cordoned": {phase: NodeReady, spec: NodeSpec{Drain: true}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			node := &Node{Spec: tc.spec, Status: NodeStatus{Phase: tc.phase}}

			assert.Equal(t, tc.want, node.Schedulable())
		})
	}
}
