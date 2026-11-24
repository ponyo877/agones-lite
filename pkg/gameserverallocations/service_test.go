package gameserverallocations

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	pb "agones.dev/agones/pkg/allocation/go"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// The tests here are about when a request is given up. They run in a
// bubble of testing/synctest: its time moves only when everyone in it
// waits, so a disk can stall for ten seconds and the test take none.

// site is a Service over a store that saves behind, with Ready GameServers
// on one node, and a disk that the test can stall.
type site struct {
	*Service
	store *store.Store
	stall func() // the disk takes nothing from now on
	mend  func() // and takes everything again
}

func newSite(t *testing.T, ready int) *site {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	recorder, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	require.NoError(t, err)
	_, err = store.NodesOf(s).Create(&litev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: litev1.NodeStatus{Phase: litev1.NodeReady}})
	require.NoError(t, err)
	for i := range ready {
		_, err := store.GameServersOf(s).Create(server(fmt.Sprint("gs-", i), agonesv1.GameServerStateReady, on("node")))
		require.NoError(t, err)
	}

	var gate sync.Mutex
	stalled := sync.NewCond(&gate)
	open := true
	s.Disk = func() error {
		gate.Lock()
		defer gate.Unlock()
		for !open {
			stalled.Wait()
		}
		return nil
	}
	set := func(to bool) func() {
		return func() { gate.Lock(); open = to; gate.Unlock(); stalled.Broadcast() }
	}

	ctx, stop := context.WithCancel(context.Background())
	var run sync.WaitGroup
	run.Go(func() { s.Run(ctx) })
	synctest.Wait() // Run has taken over the saving
	t.Cleanup(func() {
		set(true)()
		stop()
		run.Wait()
		_ = recorder.Close()
		_ = s.Close()
	})
	return &site{Service: NewService(s, recorder), store: s, stall: set(false), mend: set(true)}
}

// ask is one request for any GameServer, by a caller that waits for patience.
func (s *site) ask(patience time.Duration) (name string, code codes.Code, took time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	asked := time.Now()
	answer, err := s.Allocate(ctx, &pb.AllocationRequest{Namespace: namespace})
	return answer.GetGameServerName(), status.Code(err), time.Since(asked)
}

// allocated counts the GameServers that are taken, answered for or not.
func (s *site) allocated() (taken int) {
	for _, gs := range store.GameServersOf(s.store).View(namespace) {
		if gs.Status.State == agonesv1.GameServerStateAllocated {
			taken++
		}
	}
	return taken
}

func TestARequestWhoseCallerHasGoneTakesNoGameServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSite(t, 3)
		gone, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := s.Allocate(gone, &pb.AllocationRequest{Namespace: namespace})

		assert.Equal(t, codes.Canceled, status.Code(err))
		assert.Zero(t, s.allocated())
	})
}

func TestARequestThatWaitsForItsTurnLongerThanItsCallerTakesNoGameServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSite(t, 3)
		s.turn <- struct{}{} // someone else is choosing, and takes their time

		_, code, took := s.ask(time.Second)

		assert.Equal(t, codes.DeadlineExceeded, code)
		assert.Equal(t, time.Second, took)
		<-s.turn
		assert.Zero(t, s.allocated())
	})
}

// A disk stalls for nine and a half seconds. Eight matchmakers each want
// one GameServer, give up on a request after a second, and ask again.
//
// Asked for without a limit, every request takes a GameServer before it
// waits for the disk, and its caller is gone before the disk is back: each
// matchmaker takes nine that nobody plays on. With the limit, a matchmaker
// loses the one request that was on its way when the disk stalled, and is
// told at once from then on.
func TestAStalledDiskWastesNoMoreThanWhatWasAskedForWithinPatience(t *testing.T) {
	const matchmakers, stall, timeout = 8, 9500 * time.Millisecond, time.Second
	tests := map[string]struct {
		patience time.Duration
		wasted   int
	}{
		"with the limit":    {patience: timeout / 2, wasted: matchmakers},
		"without the limit": {patience: time.Hour, wasted: matchmakers * int(stall/timeout)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newSite(t, 1000)
				s.Patience = test.patience
				s.stall()
				time.AfterFunc(stall, s.mend)

				var asking sync.WaitGroup
				answered := make([]string, matchmakers)
				for i := range matchmakers {
					asking.Go(func() { answered[i] = s.untilAnswered(timeout) })
				}
				asking.Wait()
				require.NoError(t, s.store.Sync(context.Background()))

				assert.NotContains(t, answered, "", "every matchmaker has its GameServer in the end")
				assert.Equal(t, test.wasted, s.allocated()-matchmakers, "GameServers taken for a caller that had gone")
			})
		})
	}
}

// untilAnswered asks until a request is answered with a GameServer, and
// returns its name. A request that is refused is asked again after a
// tenth of the timeout, as a client with a retry policy would.
func (s *site) untilAnswered(timeout time.Duration) string {
	for {
		name, code, _ := s.ask(timeout)
		if code == codes.OK {
			return name
		}
		if code == codes.Unavailable {
			time.Sleep(timeout / 10)
		}
	}
}

func TestWhileTheDiskIsStalledARequestIsRefusedAtOnceAndOnceItIsBackTheNextIsAnswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSite(t, 10)
		s.stall()
		_, code, took := s.ask(3 * time.Second)
		require.Equal(t, codes.DeadlineExceeded, code, "the first one waits for the disk, as long as its caller does")
		require.Equal(t, 3*time.Second, took)

		_, code, took = s.ask(3 * time.Second)

		assert.Equal(t, codes.Unavailable, code)
		assert.Zero(t, took, "the caller learns it without waiting")

		s.mend()
		require.NoError(t, s.store.Sync(context.Background()))
		name, code, _ := s.ask(3 * time.Second)
		assert.Equal(t, codes.OK, code)
		assert.NotEmpty(t, name)
	})
}
