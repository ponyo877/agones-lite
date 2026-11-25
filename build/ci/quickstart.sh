#!/bin/sh
# Runs the quick start of the README with a scrim binary, as a newcomer
# would: the control plane is started, the sample Fleet of Agones is
# applied, two game servers become Ready, one is allocated, and it answers
# a player's packet. Everything this starts is removed again.
# Usage: build/ci/quickstart.sh PATH_TO_SCRIM
#
# It needs Docker, and it needs Linux for the last step: where Docker runs
# in a virtual machine, as on macOS, the host cannot be counted on to reach
# a game server's UDP port.
set -eu
version=$(awk '$1 == "agones.dev/agones" { print $2 }' go.mod)
examples="https://raw.githubusercontent.com/agones-dev/agones/$version/examples/simple-game-server"
scrim=$(realpath "$1")
cd "$(mktemp -d)"

# containers lists what scrim has started, running or not.
containers() {
	docker ps --all --quiet --filter label=scrim.node
}

# holds WHAT: whether one of the things this waits for is so.
holds() {
	case $1 in
	up) "$scrim" get fleets ;;
	ready) [ "$("$scrim" get gameservers | grep -c ' Ready ')" -eq 2 ] ;;
	answering) [ "$(echo hello | nc -u -w 2 "$address" "$port")" = "ACK: hello" ] ;;
	gone) [ -z "$(containers)" ] ;;
	esac > /dev/null 2>&1
}

# wait_for SECONDS WHAT: returns when it holds, and fails when it never did.
wait_for() {
	left=$1
	until holds "$2"; do
		left=$((left - 1))
		if [ "$left" -le 0 ]; then
			echo "gave up waiting for: $2" >&2
			return 1
		fi
		sleep 1
	done
}

# tidy removes what was started. When something went wrong, it first shows
# what the control plane saw of it.
tidy() {
	status=$?
	if [ "$status" -ne 0 ]; then
		echo "--- the control plane's log"
		cat server.log
		echo "--- the game servers, and their containers"
		"$scrim" get gameservers || true
		docker ps --all
	fi >&2
	"$scrim" delete fleet simple-game-server > /dev/null 2>&1 || true
	wait_for 60 gone || true
	kill "$server" 2> /dev/null || true
	containers | xargs -r docker rm --force > /dev/null 2>&1 || true
	exit "$status"
}

"$scrim" server > server.log 2>&1 &
server=$!
trap tidy EXIT
wait_for 30 up

curl -fsSL "$examples/fleet.yaml" | "$scrim" apply -f -
wait_for 180 ready
"$scrim" get gameservers

# The answer is the game server's name, its address and its port.
# shellcheck disable=SC2046 # three words, to be taken apart
set -- $("$scrim" allocate --fleet simple-game-server)
name=$1 address=$2 port=$3
echo "allocated $name at $address:$port"
"$scrim" get gameservers | grep "^$name " | grep -q ' Allocated '

wait_for 5 answering
echo "it answers a player's packet"
