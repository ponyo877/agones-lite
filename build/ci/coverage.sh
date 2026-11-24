#!/bin/sh
# Fails when the tests reach less of the code than the floor.
# Usage: build/ci/coverage.sh PERCENT
#
# The number says little about how good the tests are. A drop says that
# code came in without any.
set -eu
floor=$1
profile=$(mktemp)
trap 'rm -f "$profile"' EXIT

go test ./... -count=1 -coverpkg=./... -coverprofile="$profile"
total=$(go tool cover -func="$profile" | awk '/^total:/ { sub("%", "", $3); print $3 }')
echo "the tests reach ${total}% of the statements; the floor is ${floor}%"
awk -v total="$total" -v floor="$floor" 'BEGIN { exit !(total >= floor) }'
