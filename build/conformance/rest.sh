#!/bin/sh
# Runs the WebSocket client of Agones, then its REST client, against the
# sidecar of this game server.
#
# The WebSocket client checks the first GameServer it is sent, which is
# the GameServer as it is when the client connects: it must be Reserved or
# Shutdown. Agones runs the REST client first, against a local SDK server
# that goes on after Shutdown, so the WebSocket client is sent a GameServer
# in Shutdown. Here Shutdown ends the game server, so the WebSocket client
# goes first, and the GameServer is Reserved before it connects.
set -eu
sdk=http://localhost:$AGONES_SDK_HTTP_PORT
wget -q -O /dev/null --post-data '{}' "$sdk/reserve"
until wget -q -O - "$sdk/gameserver" | grep -q Reserved; do
	sleep 1
done
/websocket
exec /rest
