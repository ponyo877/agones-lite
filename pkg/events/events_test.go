package events

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func open(t *testing.T) *Recorder {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestEventsComeBackNewestFirstAndPerResource(t *testing.T) {
	r := open(t)
	r.Record("GameServer", "default", "a", Normal, "Placed", "on node %s", "node-1")
	r.Record("GameServer", "default", "a", Warning, "Unhealthy", "health check failed")
	r.Record("GameServer", "default", "b", Normal, "Placed", "on node node-2")

	got, err := r.For("GameServer", "default", "a")

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "Unhealthy", got[0].Reason)
	assert.Equal(t, Warning, got[0].Type)
	assert.Equal(t, "on node node-1", got[1].Message)
}

func TestPruneDropsOldEvents(t *testing.T) {
	r := open(t)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now.Add(-25 * time.Hour) }
	r.Record("Fleet", "default", "f", Normal, "Old", "a day ago")
	r.now = func() time.Time { return now }
	r.Record("Fleet", "default", "f", Normal, "New", "just now")

	require.NoError(t, r.Prune(24*time.Hour))

	got, err := r.For("Fleet", "default", "f")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "New", got[0].Reason)
}

func TestANilRecorderDropsEverything(t *testing.T) {
	var r *Recorder
	r.Record("GameServer", "default", "a", Normal, "Placed", "ignored")
	got, err := r.For("GameServer", "default", "a")
	assert.NoError(t, err)
	assert.Empty(t, got)
	assert.NoError(t, r.Prune(time.Hour))
	assert.NoError(t, r.Close())
}
