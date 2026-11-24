// Package testenv is for the tests that need something of the machine they
// run on: a Docker daemon, the Agones submodule.
//
// On a developer's machine such a test is skipped when the thing is not
// there. In CI nothing is supposed to be missing, and a test that quietly
// does not run checks nothing: there it fails instead.
package testenv

import (
	"os"
	"testing"
)

// Complete names the environment variable that says nothing is missing
// here. CI sets it.
const Complete = "AGONES_LITE_COMPLETE"

// Missing ends a test that cannot run for want of something on this
// machine: with a skip, or with a failure where Complete is set.
func Missing(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv(Complete) != "" {
		t.Fatalf("this machine is said to be complete ("+Complete+" is set), and yet: "+format, args...)
	}
	t.Skipf(format, args...)
}
