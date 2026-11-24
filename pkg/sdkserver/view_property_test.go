package sdkserver

import (
	"context"
	"strconv"
	"testing"

	"agones.dev/agones/pkg/sdk"
	sdkbeta "agones.dev/agones/pkg/sdk/beta"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// A read must show every write from the moment the write is accepted. The
// agent's answers and the GameServers it pushes arrive on their own, in no
// fixed order, and a push may be as old as it likes. Here rapid plays the
// game server, the agent and the network, and looks for an order of events
// after which a write does not show.
func TestAReadShowsEveryWriteInWhateverOrderAnswersAndPushesArrive(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		w := &viewWorld{
			sidecar: New(&fakeAgent{}), // no deliver loop: the agent below takes the requests
			stored:  []*sdk.GameServer{withCounter("1", counter{0, 1000})},
		}
		w.sidecar.set(w.stored[0])

		t.Repeat(map[string]func(*rapid.T){
			"the game server adds a player":           w.addPlayer,
			"the game server asks for something else": w.askForReady,
			"the agent takes the next request":        w.agentTakes,
			"the agent's answer arrives":              w.answerArrives,
			"a push arrives, however old":             w.pushArrives,
			"":                                        w.everyWriteShows,
		})
	})
}

// viewWorld is a sidecar with everything around it played by hand.
type viewWorld struct {
	sidecar *Server
	stored  []*sdk.GameServer      // every version the control plane had, oldest first
	taken   *sidecarv1.TellRequest // what the agent is working on: one request at a time
	written int64                  // the players the game server added
}

func (w *viewWorld) addPlayer(*rapid.T) {
	_, err := beta{server: w.sidecar}.UpdateCounter(context.Background(), &sdkbeta.UpdateCounterRequest{
		CounterUpdateRequest: &sdkbeta.CounterUpdateRequest{Name: "players", CountDiff: 1},
	})
	if err == nil { // refused when too many requests are waiting
		w.written++
	}
}

func (w *viewWorld) askForReady(*rapid.T) {
	_, _ = w.sidecar.Ready(context.Background(), &sdk.Empty{})
}

// agentTakes hands the agent the next request. A change to the Counter is
// stored at once, as a new version; its answer is still to come.
func (w *viewWorld) agentTakes(*rapid.T) {
	if w.taken != nil || len(w.sidecar.outbox) == 0 {
		return
	}
	w.taken = <-w.sidecar.outbox
	if w.taken.GetEvent() != sidecarv1.Event_EVENT_UPDATE_COUNTS {
		return
	}
	next := WithPending(w.latest(), []*sidecarv1.TellRequest{w.taken})
	next.ObjectMeta.ResourceVersion = strconv.Itoa(len(w.stored) + 1)
	w.stored = append(w.stored, next)
}

func (w *viewWorld) answerArrives(*rapid.T) {
	if w.taken == nil {
		return
	}
	var answer *sdk.GameServer
	if w.taken.GetEvent() == sidecarv1.Event_EVENT_UPDATE_COUNTS {
		answer = w.latest()
	}
	w.sidecar.settle(w.taken, answer)
	w.taken = nil
}

func (w *viewWorld) pushArrives(t *rapid.T) {
	w.sidecar.set(rapid.SampledFrom(w.stored).Draw(t, "pushed"))
}

func (w *viewWorld) everyWriteShows(t *rapid.T) {
	view, err := w.sidecar.GetGameServer(context.Background(), &sdk.Empty{})
	require.NoError(t, err)
	require.Equal(t, w.written, playersIn(view).count, "the view on top of version %s", view.GetObjectMeta().GetResourceVersion())
}

func (w *viewWorld) latest() *sdk.GameServer {
	return w.stored[len(w.stored)-1]
}
