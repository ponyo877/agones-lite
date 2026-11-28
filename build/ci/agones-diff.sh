#!/usr/bin/env bash
# Plays the same game servers, allocations and Fleet changes on Agones and
# on scrim, and fails if the two answer differently. Before they are
# compared, what cannot be the same is taken out: addresses, ports, node
# names, times, and the names that Fleets make up.
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

# The names a Fleet makes up end in random letters; so do those of its
# GameServerSets.
anonymous='walk(if type == "string" then sub("^diff-fleet-[a-z0-9]{5}(-[a-z0-9]{5})?$"; "diff-fleet-*") else . end)'

# answer prints what the allocator said to REQUEST.
answer() {
	local out
	out=$("${side}_allocate" "$1")
	jq -S --argjson code "${out##*$'\n'}" "{code: \$code, answer: (
		del(.address, .addresses, .nodeName, .source)
		| if .ports then .ports |= map(del(.port)) else . end)} | $anonymous" <<<"${out%$'\n'*}"
}

servers() {
	"${side}_list" gameservers | jq -S ".items | map({
		name: .metadata.name, labels: .metadata.labels,
		annotations: .metadata.annotations, finalizers: .metadata.finalizers,
		spec: (.spec | .ports |= map(del(.hostPort))),
		status: (.status | del(.address, .addresses, .nodeName) | .ports |= (. // [] | map(del(.port))))
	}) | $anonymous | sort_by(.name, .status.state, .labels.version)"
}

fleet() {
	jq -S -n --argjson fleets "$("${side}_list" fleets)" \
		--argjson sets "$("${side}_list" gameserversets)" \
		--argjson servers "$(servers)" "{
		fleet: \$fleets.items | map({annotations: .metadata.annotations, spec, status}),
		sets: \$sets.items | sort_by(.metadata.creationTimestamp) | map({
			labels: .spec.template.metadata.labels, replicas: .spec.replicas, status}),
		servers: \$servers | map({labels, state: .status.state, counters: .status.counters})
	} | $anonymous"
}

# settle prints what WHAT shows once it stops changing, and no game server
# is on its way from one state to another.
settle() {
	local before='' now
	for _ in $(seq 100); do
		now=$("$1")
		if [ "$now" = "$before" ] && ! grep -qE '"(PortAllocation|Creating|Starting|Scheduled|RequestReady|Shutdown)"' <<<"$now"; then
			printf '%s\n' "$now"
			return
		fi
		before=$now
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
	settle servers
	allocations
	echo "# the game servers after the allocations"
	settle servers
	"${side}_delete" gameservers diff-a diff-b diff-c diff-d
	empty gameservers

	echo "# a Fleet of three"
	"${side}_apply" "$cases/fleet.yaml"
	settle fleet
	echo "# two allocations from it"
	for _ in 1 2; do
		answer '{"namespace": "default", "gameServerSelectors": [{"matchLabels": {"agones.dev/fleet": "diff-fleet"}}], "counters": {"rooms": {"action": "Increment", "amount": "1"}}}'
		sleep 1
	done
	settle fleet
	echo "# scaled down below what is allocated"
	"${side}_scale" diff-fleet 1
	settle fleet
	echo "# scaled up"
	"${side}_scale" diff-fleet 4
	settle fleet
	echo "# a new template"
	sed 's/version: one/version: two/' "$cases/fleet.yaml" >fleet-two.yaml
	"${side}_apply" fleet-two.yaml
	settle fleet
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
	exit 1
fi
echo "scrim answered as Agones did, $(grep -c '^#' agones.txt) times"
