package agent

import (
	"agones.dev/agones/pkg/sdk"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

// sdkView returns gs in the shape the SDK hands to game servers.
// Ported from convert in upstream's pkg/sdkserver/sdk.go, minus the
// PlayerTracking part, which is an alpha feature we do not have.
func sdkView(gs *agonesv1.GameServer) *sdk.GameServer {
	health := gs.Spec.Health
	view := &sdk.GameServer{
		ObjectMeta: &sdk.GameServer_ObjectMeta{
			Name:              gs.Name,
			Namespace:         gs.Namespace,
			Uid:               string(gs.UID),
			ResourceVersion:   gs.ResourceVersion,
			Generation:        gs.Generation,
			CreationTimestamp: gs.CreationTimestamp.Unix(),
			Annotations:       gs.Annotations,
			Labels:            gs.Labels,
		},
		Spec: &sdk.GameServer_Spec{
			Health: &sdk.GameServer_Spec_Health{
				Disabled:            health.Disabled,
				PeriodSeconds:       health.PeriodSeconds,
				FailureThreshold:    health.FailureThreshold,
				InitialDelaySeconds: health.InitialDelaySeconds,
			},
		},
		Status: sdkStatus(gs.Status),
	}
	if gs.DeletionTimestamp != nil {
		view.ObjectMeta.DeletionTimestamp = gs.DeletionTimestamp.Unix()
	}
	return view
}

func sdkStatus(status agonesv1.GameServerStatus) *sdk.GameServer_Status {
	out := &sdk.GameServer_Status{State: string(status.State), Address: status.Address}
	for _, a := range status.Addresses {
		out.Addresses = append(out.Addresses, &sdk.GameServer_Status_Address{Type: string(a.Type), Address: a.Address})
	}
	for _, p := range status.Ports {
		out.Ports = append(out.Ports, &sdk.GameServer_Status_Port{Name: p.Name, Port: p.Port})
	}
	for name, c := range status.Counters {
		if out.Counters == nil {
			out.Counters = map[string]*sdk.GameServer_Status_CounterStatus{}
		}
		out.Counters[name] = &sdk.GameServer_Status_CounterStatus{Count: c.Count, Capacity: c.Capacity}
	}
	for name, l := range status.Lists {
		if out.Lists == nil {
			out.Lists = map[string]*sdk.GameServer_Status_ListStatus{}
		}
		out.Lists[name] = &sdk.GameServer_Status_ListStatus{Capacity: l.Capacity, Values: append([]string(nil), l.Values...)}
	}
	return out
}
