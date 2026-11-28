package main

import (
	"log/slog"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
)

// Every level a GameServer may name means something, and the order is
// upstream's: Error says the least, Trace the most.
func TestEveryLogLevelOfTheSpecIsALevel(t *testing.T) {
	assert.Equal(t, slog.LevelError, level(string(agonesv1.SdkServerLogLevelError)))
	assert.Equal(t, slog.LevelInfo, level(string(agonesv1.SdkServerLogLevelInfo)))
	assert.Equal(t, slog.LevelDebug, level(string(agonesv1.SdkServerLogLevelDebug)))
	assert.Less(t, level(string(agonesv1.SdkServerLogLevelTrace)), slog.LevelDebug)
	assert.Equal(t, slog.LevelInfo, level(""), "a sidecar started by an older agent")
}
