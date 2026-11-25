#!/bin/sh
# Fails when a test was skipped.
# Usage: build/ci/skipped.sh
#
# "go test" names the tests it skipped only when asked, and a test that
# did not run looks like one that passed. On a machine where nothing is
# missing, as in CI, none has a reason to be skipped. The load test and the
# test that tries its judgement are the exception: they wait to be asked
# for, and CI asks for them in a job of its own, which sees that both ran.
set -eu
asked_for='^--- SKIP: (TestLoad|TestLoadSetsAsideWhatADiskThatStoodStillHeldUp) '
skipped=$(go test ./... -count=1 -v | grep '^--- SKIP' | grep -Ev "$asked_for" || true)
[ -z "$skipped" ] || { echo "these tests did not run:"; echo "$skipped"; exit 1; }
echo "no test was skipped"
