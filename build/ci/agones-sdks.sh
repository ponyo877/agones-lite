#!/bin/sh
# Runs one of the SDK conformance clients of Agones, unchanged, as a game
# server on scrim.
#
# The clients are the ones Agones runs for each of its SDKs (test/sdk in
# third_party/agones). Each checks what the SDK answers by itself, and
# ends with Shutdown only when every check passed. Agones runs them
# against a local SDK server that stands in for the rest, and checks which
# requests arrived. Here nothing stands in: the requests go through the
# sidecar and the agent to the control plane. So this checks what came of
# them, in the GameServer that the control plane keeps:
#
#   - it was Ready, then Allocated, then Shutdown, and never Unhealthy;
#   - it has a label and an annotation from the client, with the values
#     Agones checks for: the time it was created, and its UID.
#
# Usage: build/ci/agones-sdks.sh PATH_TO_SCRIM CLIENT
# CLIENT is go, rest, python, csharp, node, rust, rust-async or cpp. Its
# image is built from build/conformance when it is not there already.
# It needs Docker and jq.
set -eu
scrim=$(realpath "$1")
client=$2
version=$(awk '$1 == "agones.dev/agones" { print $2 }' go.mod)
image=scrim-agones-sdk-${client%-async}:$version
name=sdk-$client

# What Agones' own run gives a client (build/includes/sdk.mk): the feature
# gates of Beta to those that try Counters and Lists only when they are
# told to, and RUN_ASYNC to the second run of the Rust client.
#
# The REST client is not told to. It wants UpdateList to answer with the
# List as updated, which the local SDK server of Agones does. The SDK
# server that Agones runs beside a game server answers with an empty List,
# and so does scrim's.
beta="CountsAndLists=true&GKEAutopilotExtendedDurationPods=true&ScheduledAutoscaler=true&SidecarContainers=true&FleetAutoscaleRequestMetaData=true"
case $client in
python | csharp) env="[{name: FEATURE_GATES, value: \"$beta\"}]" ;;
rust-async) env='[{name: RUN_ASYNC, value: "true"}]' ;;
go | rest | node | rust | cpp) env='[]' ;;
*)
	echo "unknown client: $client" >&2
	exit 2
	;;
esac

if ! docker image inspect "$image" > /dev/null 2>&1; then
	docker build --tag "$image" --file "build/conformance/${client%-async}.Dockerfile" .
fi
cd "$(mktemp -d)"

# manifest is the GameServer. The Counter and the List start as the local
# SDK server of Agones has them, which is what the clients expect. The
# clients send a health ping or a few, since the local SDK server does
# not check them: checking starts after they are done.
manifest() {
	cat <<-EOF
		apiVersion: agones.dev/v1
		kind: GameServer
		metadata:
		  name: $name
		spec:
		  ports:
		    - name: default
		      containerPort: 7654
		  health:
		    initialDelaySeconds: 300
		  counters:
		    rooms: {count: 1, capacity: 10}
		  lists:
		    players: {values: [test0, test1, test2], capacity: 100}
		  template:
		    spec:
		      containers:
		        - name: client
		          image: $image
		          imagePullPolicy: Never
		          env: $env
	EOF
}

# seen FILTER: whether a change of the GameServer matches the jq FILTER.
seen() {
	jq -ne --arg name "$name" "any(inputs; .object.metadata.name == \$name and ($1))" changes.jsonl
}

# holds WHAT: whether one of the things this waits for is so.
holds() {
	case $1 in
	up) "$scrim" get fleets ;;
	watching) grep -q '"type":"Synced"' changes.jsonl ;;
	started) seen '.object.status.state | IN("Scheduled", "RequestReady", "Ready", "Reserved", "Allocated", "Shutdown")' ;;
	over) seen '.type == "Deleted" or (.object.status.state | IN("Shutdown", "Unhealthy", "Error"))' ;;
	gone) [ -z "$(docker ps --all --quiet --filter "label=scrim.gameserver=$name")" ] ;;
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

# follow keeps what the client prints in client.log. Its container may not
# be there yet, or not started, when Docker has nothing to follow and ends
# at once; or it may be gone already, as a quick client is done in a few
# seconds.
follow() {
	until [ -s client.log ] || holds over; do
		"$scrim" logs "$name" -f > client.log 2> /dev/null || true
		sleep 0.2
	done
}

# tidy removes what was started. When something went wrong, it first shows
# what the control plane and the client said.
tidy() {
	status=$?
	if [ "$status" -ne 0 ]; then
		echo "--- the control plane's log"
		cat server.log
		echo "--- what the GameServer went through"
		cat changes.jsonl watch.log 2> /dev/null || true
	fi >&2
	echo "--- what the client printed"
	cat client.log 2> /dev/null || true
	"$scrim" delete gameserver "$name" > /dev/null 2>&1 || true
	wait_for 60 gone || true
	for pid in $watcher $logs $server; do
		kill "$pid" 2> /dev/null || true
	done
	docker ps --all --quiet --filter "label=scrim.gameserver=$name" | xargs -r docker rm --force > /dev/null 2>&1 || true
	exit "$status"
}

watcher='' logs=''
"$scrim" server > server.log 2>&1 &
server=$!
trap tidy EXIT
wait_for 30 up

"$scrim" get gameservers -w -o json > changes.jsonl 2> watch.log &
watcher=$!
wait_for 10 watching
manifest | "$scrim" apply -f -
follow &
logs=$!
wait_for 120 started
wait_for 300 over

# The GameServer as every change left it, in order. Each state in $states
# has a space after it, and only Ready needs one before it as well, to tell
# it from RequestReady.
jq -c --arg name "$name" 'select(.object.metadata.name == $name) | .object' changes.jsonl > gameserver.jsonl
states=$(jq -r '.status.state' gameserver.jsonl | uniq | tr '\n' ' ')
echo "$name went through: $states"
case " $states" in
*" Unhealthy "* | *" Error "*) echo "the GameServer failed" >&2 && exit 1 ;;
*" Ready "*"Allocated "*"Shutdown "*) ;;
*) echo "the GameServer was not Ready, then Allocated, then Shutdown" >&2 && exit 1 ;;
esac

# What the client set, with the values that Agones checks for. The SDK
# puts "agones.dev/sdk-" before the keys the client names.
jq -se '
	map(select(.status.state == "Shutdown")) | last
	| (.metadata.creationTimestamp | fromdateiso8601 | tostring) as $created
	| .metadata.uid as $uid
	| [(.metadata.labels // {} | to_entries[] | select(.value == $created)),
	   (.metadata.annotations // {} | to_entries[] | select(.value == $uid))]
	| map(select(.key | startswith("agones.dev/sdk-")))
	| if (map(.value) | index($created)) and (map(.value) | index($uid)) then .
	  else error("no label with the creation time, or no annotation with the UID") end
' gameserver.jsonl
echo "the $client client of Agones passed on scrim"
