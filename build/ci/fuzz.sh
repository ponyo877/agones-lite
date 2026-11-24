#!/bin/sh
# Runs every fuzz target for a while, one after the other.
# Usage: build/ci/fuzz.sh [seconds for each target]
# With FUZZ_SKIP set to a regular expression, the targets it matches are
# left out.
#
# "go test" only replays the inputs that were kept. This looks for new
# ones: not for long, but on every change. An input that fails is written
# to the package's testdata/fuzz, and CI keeps it for whoever looks into it.
set -eu
seconds=${1:-30}
skip=${FUZZ_SKIP:-^$}
ran=0

for pkg in $(go list ./...); do
	for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' | grep -Ev "$skip" || true); do
		echo "== $target in $pkg, for ${seconds}s"
		go test "$pkg" -run '^$' -fuzz "^${target}\$" -fuzztime "${seconds}s" -fuzzminimizetime 10s
		ran=$((ran + 1))
	done
done
echo "$ran fuzz targets ran"
[ "$ran" -gt 0 ]
