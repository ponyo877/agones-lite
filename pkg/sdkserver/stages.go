package sdkserver

import (
	"context"
	"errors"

	sdkalpha "agones.dev/agones/pkg/sdk/alpha"
)

// errPlayerTracking is what upstream returns while the PlayerTracking
// feature gate is off, which is its default.
var errPlayerTracking = errors.New("PlayerTracking not enabled")

// alpha is the alpha SDK service: player tracking. Upstream ships it turned
// off and has since removed it; scrim behaves like the default.
type alpha struct {
	sdkalpha.UnimplementedSDKServer
}

func (alpha) PlayerConnect(context.Context, *sdkalpha.PlayerID) (*sdkalpha.Bool, error) {
	return &sdkalpha.Bool{}, errPlayerTracking
}

func (alpha) PlayerDisconnect(context.Context, *sdkalpha.PlayerID) (*sdkalpha.Bool, error) {
	return &sdkalpha.Bool{}, errPlayerTracking
}

func (alpha) SetPlayerCapacity(context.Context, *sdkalpha.Count) (*sdkalpha.Empty, error) {
	return &sdkalpha.Empty{}, errPlayerTracking
}

func (alpha) GetPlayerCapacity(context.Context, *sdkalpha.Empty) (*sdkalpha.Count, error) {
	return &sdkalpha.Count{}, errPlayerTracking
}

func (alpha) GetPlayerCount(context.Context, *sdkalpha.Empty) (*sdkalpha.Count, error) {
	return &sdkalpha.Count{}, errPlayerTracking
}

func (alpha) IsPlayerConnected(context.Context, *sdkalpha.PlayerID) (*sdkalpha.Bool, error) {
	return &sdkalpha.Bool{}, errPlayerTracking
}

func (alpha) GetConnectedPlayers(context.Context, *sdkalpha.Empty) (*sdkalpha.PlayerIDList, error) {
	return &sdkalpha.PlayerIDList{}, errPlayerTracking
}
