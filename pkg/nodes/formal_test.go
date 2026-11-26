package nodes

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/formal/vectors"
	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
)

// These tests tie the Quint model in formal/quint/node.qnt to the Go code:
// what an operator asks of a node, as the model has it, is what Node.Duty
// reads, what Node.Ask writes, and what each operation leaves behind.

func TestDutyAgreesWithTheQuintModel(t *testing.T) {
	type vector struct {
		Unschedulable, Drain bool
		Duty                 string
	}
	all := vectors.Read[vector](t, "quint/duties.jsonl")
	for _, v := range all {
		n := &scrimv1.Node{Spec: scrimv1.NodeSpec{Unschedulable: v.Unschedulable, Drain: v.Drain}}
		assert.Equal(t, scrimv1.Duty(v.Duty), n.Duty(), "%+v", v)
	}
	assert.Len(t, all, 4, "the two switches, every way")
}

func TestTheOperationsAgreeWithTheQuintModel(t *testing.T) {
	type vector struct {
		Operation            string
		Unschedulable, Drain bool
		After                scrimv1.NodeSpec
		Duty                 string
	}
	all := vectors.Read[vector](t, "quint/operations.jsonl")
	seen := map[string]bool{}
	for _, v := range all {
		name := strings.ToLower(v.Operation) // the model's names begin with a capital
		seen[name] = true
		duty, known := operations[name]
		require.True(t, known, "the model has an operation %q", name)
		n := &scrimv1.Node{Spec: scrimv1.NodeSpec{Unschedulable: v.Unschedulable, Drain: v.Drain}}

		n.Ask(duty)

		assert.Equal(t, scrimv1.Duty(v.Duty), duty, "%+v", v)
		assert.Equal(t, v.After, n.Spec, "%+v", v)
		assert.Equal(t, duty, n.Duty(), "what was asked is what is read: %+v", v)
	}
	for name := range operations {
		assert.True(t, seen[name], "the model has no operation %q", name)
	}
	assert.Len(t, all, len(operations)*4, "every operation, on a node with its switches every way")
}
