#!/usr/bin/env bash
# Plays the same game servers, allocations and Fleet changes on Agones and
# on scrim, and fails if the two answer differently. Before they are
# compared, what cannot be the same is taken out: addresses, ports, node
# names, times, and the names that Fleets make up. So is what one of the
# two has and the other cannot: scrim's own annotations, and the
# immutableReplicas that Agones' CRD adds for a Kubernetes Scale.
#
# Usage: build/ci/agones-diff.sh PATH_TO_SCRIM
#
# kubectl must point at a cluster that runs Agones, with its allocator
# answering without TLS (build/ci/agones-kind.sh makes one).
set -euo pipefail

scrim=$(realpath "$1")
cases=$(realpath test/agones-diff)
cd "$(mktemp -d)"

# What each side is asked to do, in its own words.
agones_apply() { kubectl apply --server-side --force-conflicts -f "$1" >/dev/null; }
scrim_apply() { "$scrim" apply -f "$1" >/dev/null; }
agones_list() { kubectl get "$1" -o json; }
scrim_list() { "$scrim" get "$1" -o json; }
agones_scale() { kubectl scale fleet "$1" --replicas "$2" >/dev/null; }
scrim_scale() { "$scrim" scale fleet "$1" --replicas "$2" >/dev/null; }
agones_delete() { kubectl delete "$@" --wait >/dev/null; }
scrim_delete() { for name in "${@:2}"; do "$scrim" delete "$1" "$name" >/dev/null; done; }
agones_allocate() {
	curl -sS -w '\n%{http_code}' -d "$1" http://127.0.0.1:8443/gameserverallocation
}
scrim_allocate() {
	curl -sS -w '\n%{http_code}' -d "$1" --cacert scrim-data/tls/ca.crt \
		-H "authorization: Bearer $(cat scrim-data/tokens/allocator)" \
		https://127.0.0.1:9444/gameserverallocation
}

# alike takes out what is bound to differ. The names a Fleet makes up end
# in random letters; so do those of its GameServerSets.
alike='walk(
	if type == "string" then sub("^diff-fleet-[a-z0-9]{5}(-[a-z0-9]{5})?$"; "diff-fleet-*")
	elif type == "object" then
		del(.immutableReplicas)
		| with_entries(select(.key | startswith("scrim/") | not))
		| if has("agones.dev/last-allocated") then .["agones.dev/last-allocated"] = "a time" else . end
	else . end)'

# answer prints what the allocator said to REQUEST.
answer() {
	local out
	out=$("${side}_allocate" "$1")
	jq -S --argjson code "${out##*$'\n'}" "{code: \$code, answer: (
		del(.address, .addresses, .nodeName, .source)
		| if .ports then .ports |= map(del(.port)) else . end)} | $alike" <<<"${out%$'\n'*}"
}

servers() {
	"${side}_list" gameservers | jq -S ".items | map({
		name: .metadata.name, labels: .metadata.labels,
		annotations: .metadata.annotations, finalizers: .metadata.finalizers,
		spec: (.spec | .ports |= map(del(.hostPort))),
		status: (.status | del(.address, .addresses, .nodeName) | .ports |= (. // [] | map(del(.port))))
	}) | $alike | sort_by(.name, .status.state, .labels.version)"
}

# done_on_purpose is the one place where scrim answers differently from
# Agones v1.62 on purpose, applied to what Agones answers. An old
# GameServerSet that holds as many sessions as its size, or more, is done:
# scrim scales it to 0 at once. Agones lowers it a step, finds it short of
# its size, and waits there until the sessions end (docs/design.md says why
# scrim does not).
done_on_purpose='.sets |= ((.[:-1] | map(
	if .replicas > 0 and .status.allocatedReplicas + .status.reservedReplicas >= .replicas
	then .replicas = 0 else . end)) + .[-1:])'

fleet() {
	jq -S -n --argjson fleets "$("${side}_list" fleets)" \
		--argjson sets "$("${side}_list" gameserversets)" \
		--argjson servers "$(servers)" "{
		fleet: \$fleets.items | map({annotations: .metadata.annotations, spec, status}),
		sets: \$sets.items | sort_by(.metadata.creationTimestamp) | map({
			labels: .spec.template.metadata.labels, replicas: .spec.replicas, status}),
		servers: \$servers | map({labels, state: .status.state, counters: .status.counters})
	} | $alike | if \"$side\" == \"agones\" then $done_on_purpose else . end"
}

# settle prints what WHAT shows once it has not changed for QUIET seconds,
# with no game server on its way from one state to another.
#
# A Fleet needs a long QUIET. Agones' Fleet controller goes on to the next
# step of a change when it hears of the last one, and when it misses that,
# at the informers' resync, every 30 seconds.
settle() {
	local what=$1 quiet=$2 before='' now since=$SECONDS
	for _ in $(seq 200); do
		now=$("$what")
		if [ "$now" != "$before" ] || grep -qE '"(PortAllocation|Creating|Starting|Scheduled|RequestReady|Shutdown)"' <<<"$now"; then
			before=$now since=$SECONDS
		elif ((SECONDS - since >= quiet)); then
			printf '%s\n' "$now"
			return
		fi
		sleep 3
	done
	echo "$side never settled: $now" >&2
	return 1
}

# empty waits until there is no object of KIND left, so that what was
# deleted is not seen by what comes next.
empty() {
	for _ in $(seq 100); do
		"${side}_list" "$1" | jq -e '.items == []' >/dev/null && return
		sleep 3
	done
	echo "$side kept some $1" >&2
	return 1
}

allocations() {
	local line
	while IFS= read -r line; do
		jq -r '"# " + .why' <<<"$line"
		answer "$(jq -c .request <<<"$line")"
		sleep 1 # Agones' allocator sees changes through a cache
	done <"$cases/requests.jsonl"
}

play() {
	side=$1
	echo "# four game servers"
	"${side}_apply" "$cases/gameservers.yaml"
	settle servers 6
	allocations
	echo "# the game servers after the allocations"
	settle servers 6
	"${side}_delete" gameservers diff-a diff-b diff-c diff-d
	empty gameservers

	echo "# a Fleet of three"
	"${side}_apply" "$cases/fleet.yaml"
	settle fleet 40
	echo "# two allocations from it"
	for _ in 1 2; do
		answer '{"namespace": "default", "gameServerSelectors": [{"matchLabels": {"agones.dev/fleet": "diff-fleet"}}], "counters": {"rooms": {"action": "Increment", "amount": "1"}}}'
		sleep 1
	done
	settle fleet 40
	echo "# scaled down below what is allocated"
	"${side}_scale" diff-fleet 1
	settle fleet 40
	echo "# scaled up"
	"${side}_scale" diff-fleet 4
	settle fleet 40
	echo "# a new template"
	sed 's/version: one/version: two/' "$cases/fleet.yaml" >fleet-two.yaml
	"${side}_apply" fleet-two.yaml
	settle fleet 40
	"${side}_delete" fleets diff-fleet
	empty gameservers
}

kubectl -n agones-system port-forward svc/agones-allocator 8443:443 >/dev/null &
forward=$!
"$scrim" server >server.log 2>&1 &
server=$!
trap 'kill $forward $server 2>/dev/null; docker rm -f $(docker ps -aq --filter label=scrim.gameserver) >/dev/null 2>&1 || true' EXIT
for _ in $(seq 60); do
	"$scrim" get fleets >/dev/null 2>&1 && curl -s -o /dev/null http://127.0.0.1:8443/ && break
	sleep 1
done

play agones >agones.txt
play scrim >scrim.txt
if ! diff -u agones.txt scrim.txt; then
	echo "scrim answered differently from Agones (- Agones, + scrim)" >&2
	for side in agones scrim; do
		echo "::group::what $side answered"
		cat $side.txt
		echo "::endgroup::"
	done
	exit 1
fi
echo "scrim answered as Agones did, $(grep -c '^#' agones.txt) times"
