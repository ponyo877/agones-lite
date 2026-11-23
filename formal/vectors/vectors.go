// Package vectors holds what the Lean models in ../Formal answer for many
// small inputs, so that Go tests can replay them against the real functions.
// A proof about a model says nothing about the Go code unless the two behave
// alike; these files are how that is checked.
//
// `lake exe vectors` in the parent directory writes the files.
package vectors

import (
	"bufio"
	"embed"
	"encoding/json"
	"testing"
)

//go:embed *.jsonl
var files embed.FS

// Read returns the vectors of one file, each line decoded into a T.
func Read[T any](t testing.TB, name string) []T {
	t.Helper()
	file, err := files.Open(name)
	if err != nil {
		t.Fatalf("vectors: %v", err)
	}
	defer func() { _ = file.Close() }()
	var out []T
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		var vector T
		if err := json.Unmarshal(lines.Bytes(), &vector); err != nil {
			t.Fatalf("vectors: %s: %v", name, err)
		}
		out = append(out, vector)
	}
	if err := lines.Err(); err != nil {
		t.Fatalf("vectors: %s: %v", name, err)
	}
	return out
}
