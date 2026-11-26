#!/bin/sh
# Checks the Quint models in formal/quint: every state each of them can
# reach, with the TLC model checker.
#
# Usage: build/ci/models.sh [check|vectors]
#
#   check    (the default) types every model, runs its scenarios, and has
#            TLC hold its invariants and what must come about in the end
#            against every reachable state. Two models are of what the code
#            did before a fix, and must fail: a model that cannot show the
#            fault it was written for shows nothing.
#   vectors  writes down what the models' tables answer, into
#            formal/vectors/quint, for the Go tests to replay.
#
# Needs node and npm, jq, and a Java of version 21 or later (JAVA_HOME may
# say which). Quint itself is the version formal/quint/package-lock.json
# names; on its first run it fetches the model checker into ~/.quint.
#
# The scenarios and the tables are run by the evaluator that comes with
# Quint (--backend typescript). The other one is a program Quint would
# fetch on its first run, by a way that GitHub refuses to the shared
# machines of CI more often than not.
set -eu
mode=${1:-check}
cd "$(dirname "$0")/../../formal/quint"

# GitHub's runners have several Javas, and say where the one we need is.
if [ -n "${JAVA_HOME_21_X64:-}" ]; then JAVA_HOME=$JAVA_HOME_21_X64; fi
if [ -n "${JAVA_HOME:-}" ]; then PATH="$JAVA_HOME/bin:$PATH"; export JAVA_HOME PATH; fi
java=$(java -version 2>&1 | sed -n '1s/[^"]*"\([0-9]*\).*/\1/p')
[ "${java:-0}" -ge 21 ] || { echo "the model checker needs Java 21 or later, and found ${java:-none}: set JAVA_HOME"; exit 1; }

[ -x node_modules/.bin/quint ] || npm ci --no-audit --no-fund >/dev/null
quint=node_modules/.bin/quint
out=$(mktemp)
trap 'rm -f "$out"' EXIT

# tlc file module invariant [temporal properties]: prints how many states
# the model has, or what TLC said when a check did not hold.
tlc() {
	set -- "$1" "$2" "$3" "${4:-}"
	if [ -n "$4" ]; then
		"$quint" verify "$1" --main "$2" --backend tlc --invariant "$3" --temporal "$4" >"$out" 2>&1 || return 1
		grep -q 'Finished checking temporal properties' "$out" || return 1
	else
		"$quint" verify "$1" --main "$2" --backend tlc --invariant "$3" >"$out" 2>&1 || return 1
	fi
	grep -q 'No violation found' "$out" || return 1
	states=$(sed -n 's/.* \([0-9]*\) distinct states found, 0 states left on queue.*/\1/p' "$out" | tail -1)
	[ -n "$states" ] || return 1
	echo "$2: $states states, every one of them holds $3${4:+ and $4}"
}

holds() {
	tlc "$@" || { grep -v -e '^Parsing file' -e '^Semantic processing' -e '^Linting' "$out"; echo "$2: does not hold"; exit 1; }
}

# fails file module invariant: the model must have a state that breaks it.
fails() {
	if tlc "$@" >/dev/null; then
		echo "$2: holds $3, though it is the model of a fault"
		exit 1
	fi
	grep -q 'violation' "$out" || { cat "$out"; echo "$2: the check did not run"; exit 1; }
	echo "$2: breaks $3, as the code did before it was mended"
}

check() {
	for model in *.qnt; do
		"$quint" typecheck "$model" >/dev/null
	done
	# The scenarios: histories that are written out, run step by step.
	for main in gameserver requestsNamed requestsUnnamed linkTogether linkApart agent node queue store cascade health view; do
		file=$(grep -l "^module $main " ./*.qnt)
		"$quint" test "$file" --main "$main" --backend typescript --max-samples=100 >"$out" 2>&1 || { cat "$out"; exit 1; }
		echo "$main: $(grep -c ' ok ' "$out") scenarios"
	done

	holds gameserver.qnt gameserver safe deletionsEnd,shutdownsAreCleanedUp
	holds requests.qnt requestsNamed safe
	holds link.qnt linkTogether safe theCopyCatchesUp
	holds agent.qnt agent safe theAgentComesToRest
	holds node.qnt node safe
	holds queue.qnt queue safe everyKeyGetsItsTurn
	holds store.qnt store safe everySyncReturns
	holds cascade.qnt cascade safe aDeletedFleetGoes
	holds health.qnt health safe
	holds view.qnt view safe

	fails requests.qnt requestsUnnamed aRepeatChangesNothing
	fails link.qnt linkApart safe
}

# One file for each variable of vectors.qnt. A value of an enumerated type
# is written as its name, a set as a list, and the lines are sorted, so that
# the same tables always give the same files.
vectors() {
	"$quint" run vectors.qnt --backend typescript --max-steps=0 --max-samples=1 --out-itf="$out" >/dev/null 2>&1
	mkdir -p ../vectors/quint
	for name in $(jq -r '.vars[]' "$out"); do
		jq -cS --arg name "$name" '
			def plain:
				if type == "object" then
					if has("#set") then .["#set"] | map(plain)
					elif has("#bigint") then .["#bigint"] | tonumber
					elif has("tag") then .tag
					else with_entries(.value |= plain) end
				else . end;
			.states[0][$name]["#set"][] | plain' "$out" | LC_ALL=C sort >"../vectors/quint/$name.jsonl"
		echo "formal/vectors/quint/$name.jsonl: $(wc -l <"../vectors/quint/$name.jsonl" | tr -d ' ') vectors"
	done
}

case "$mode" in
check) check ;;
vectors) vectors ;;
*) echo "usage: $0 [check|vectors]"; exit 2 ;;
esac
