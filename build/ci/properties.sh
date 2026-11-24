#!/bin/sh
# Runs the property tests and the model tests with more tries than
# "go test" gives them by default, which is a hundred.
# Usage: build/ci/properties.sh [tries for each property]
set -eu
checks=${1:-1000}

# Only the packages that use rapid know its flags.
packages=$(grep -rl --include='*_test.go' 'pgregory.net/rapid' cmd pkg test | xargs -n1 dirname | sort -u | sed 's|^|./|')
[ -n "$packages" ] || { echo "no package uses rapid"; exit 1; }
# shellcheck disable=SC2086 # one argument for each package
go test $packages -count=1 -rapid.checks="$checks" -rapid.nofailfile
