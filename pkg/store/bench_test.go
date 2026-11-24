package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
)

// BenchmarkChange measures one change to a GameServer that waits until the
// file has it, as everything that is answered to the outside does.
//
// A write that saves itself pays for a sync of the disk each time. Written
// behind, one writer still waits for every sync, but 64 of them wait for
// the same one. The numbers depend on the disk: on a laptop a sync may
// cost next to nothing, on a server it is a millisecond or two.
//
//	go test ./pkg/store/ -run '^$' -bench Change
func BenchmarkChange(b *testing.B) {
	b.Run("each-write-saves-itself", func(b *testing.B) {
		servers := GameServersOf(openWith(b, 1000))
		for i := 0; b.Loop(); i++ {
			require.NoError(b, relabel(servers, i%1000))
		}
	})
	for _, writers := range []int{1, 64} {
		b.Run(fmt.Sprintf("written-behind/%d-writers", writers), func(b *testing.B) {
			s := openWith(b, 1000)
			defer runBehind(s)()
			b.ResetTimer()
			changeAndSync(b, GameServersOf(s), writers)
		})
	}
}

// changeAndSync makes b.N changes, shared out among that many writers.
// Each waits for the file after every change it makes.
func changeAndSync(b *testing.B, servers Typed[*agonesv1.GameServer], writers int) {
	var next atomic.Int64
	var writing sync.WaitGroup
	for range writers {
		writing.Go(func() {
			for i := int(next.Add(1)); i <= b.N; i = int(next.Add(1)) {
				if err := errors.Join(relabel(servers, i%1000), servers.Sync(context.Background())); err != nil {
					b.Error(err)
				}
			}
		})
	}
	writing.Wait()
}

// BenchmarkRead measures the three ways of reading the GameServers of one
// node out of 10,000 on a hundred nodes: a copy of everything, a look at
// everything, and the group the store keeps.
func BenchmarkRead(b *testing.B) {
	servers := GameServersOf(openWith(b, 10000))
	ways := map[string]func() int{
		"List":   func() int { return countOn("node-42", servers.List("")) },
		"View":   func() int { return countOn("node-42", servers.View("")) },
		"OnNode": func() int { return len(OnNode(servers, "node-42")) },
	}
	for name, read := range ways {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				require.Equal(b, 100, read())
			}
		})
	}
}

func countOn(node string, servers []*agonesv1.GameServer) (n int) {
	for _, gs := range servers {
		if gs.Status.NodeName == node {
			n++
		}
	}
	return n
}

// openWith returns a store that holds n GameServers, a hundred to a node.
func openWith(b *testing.B, n int) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "state.db"), Kinds())
	require.NoError(b, err)
	b.Cleanup(func() { _ = s.Close() })
	stop := runBehind(s) // to fill it quickly
	for i := range n {
		gs := gameServer(fmt.Sprint("gs-", i))
		gs.Status.NodeName = fmt.Sprint("node-", i/100)
		_, err := GameServersOf(s).Create(gs)
		require.NoError(b, err)
	}
	stop()
	return s
}

// runBehind has the store written behind until the function it returns is
// called.
func runBehind(s *Store) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { s.Run(ctx) })
	return func() {
		cancel()
		running.Wait()
	}
}

func relabel(servers Typed[*agonesv1.GameServer], i int) error {
	return servers.Change("default", fmt.Sprint("gs-", i), func(gs *agonesv1.GameServer) bool {
		gs.Labels = map[string]string{"turn": fmt.Sprint(i)}
		return true
	})
}
