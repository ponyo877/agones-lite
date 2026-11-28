package gameserverallocations

import (
	"context"
	"testing"

	pb "agones.dev/agones/pkg/allocation/go"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A development GameServer is on no node: its game runs on a developer's
// machine. It is allocated like any other, with either strategy, while a
// GameServer on a node that is gone is not.
func TestADevelopmentGameServerIsAllocated(t *testing.T) {
	for _, strategy := range []pb.AllocationRequest_SchedulingStrategy{pb.AllocationRequest_Packed, pb.AllocationRequest_Distributed} {
		t.Run(strategy.String(), func(t *testing.T) {
			service, servers := aServiceWith(t, 0)
			_, err := servers.Create(server("on-a-node-that-is-gone", agonesv1.GameServerStateReady, on("gone")))
			require.NoError(t, err)
			_, err = servers.Create(server("dev", agonesv1.GameServerStateReady, func(gs *agonesv1.GameServer) {
				gs.Annotations = map[string]string{agonesv1.DevAddressAnnotation: "192.0.2.1"}
				gs.Status.Address = "192.0.2.1"
			}))
			require.NoError(t, err)

			answer, err := service.Allocate(context.Background(), &pb.AllocationRequest{Namespace: namespace, Scheduling: strategy})

			require.NoError(t, err)
			assert.Equal(t, "dev", answer.GetGameServerName())
			assert.Equal(t, "192.0.2.1", answer.GetAddress())
			gs, _ := servers.Get(namespace, "dev")
			assert.Equal(t, agonesv1.GameServerStateAllocated, gs.Status.State)
		})
	}
}
