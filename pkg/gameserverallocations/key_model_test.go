package gameserverallocations

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	pb "agones.dev/agones/pkg/allocation/go"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// What KeyAnnotation promises, held against a model of it: matchmakers ask,
// with a key or without, again and again, and sessions end in between.
//
// The model says of every answer which GameServer it must name, or that it
// must name one that is in no session: what every matchmaker was told is
// checked, and not only where things stood at the end.
func TestAKeyIsAnsweredWithTheSameGameServerForAsLongAsItsSessionLasts(t *testing.T) {
	rapid.Check(t, func(r *rapid.T) {
		const size = 6
		service, servers := aServiceWith(t, size)
		m := &sessions{service: service, servers: servers, ready: size, playing: map[string]bool{}, heldBy: map[string]string{}}
		r.Repeat(map[string]func(*rapid.T){
			"a matchmaker asks": m.asks,
			"a session ends":    m.ends,
		})
	})
}

// sessions is the model, beside the Service it is a model of.
type sessions struct {
	service *Service
	servers store.Typed[*agonesv1.GameServer]
	ready   int               // nobody starts new ones here: each is given once, and gone after
	playing map[string]bool   // the GameServers in a session, by name
	heldBy  map[string]string // the GameServer a key holds, while its session lasts
}

func (m *sessions) asks(r *rapid.T) {
	key := rapid.SampledFrom([]string{"", "a", "b", "c"}).Draw(r, "key")
	name, code := askOnce(m.service, key)
	held, again := m.heldBy[key]
	switch {
	case again:
		require.Equal(r, held, name, "the key holds a GameServer, and that is the answer")
	case m.ready == 0:
		require.Equal(r, codes.ResourceExhausted, code, "there is none left to give")
	default:
		require.Equal(r, codes.OK, code)
		require.False(r, m.playing[name], "%s is in a session already, and not this key's", name)
		m.begin(key, name)
	}
}

func (m *sessions) begin(key, name string) {
	m.ready--
	m.playing[name] = true
	if key != "" {
		m.heldBy[key] = name
	}
}

func (m *sessions) ends(r *rapid.T) {
	if len(m.playing) == 0 {
		r.Skip("nobody is playing")
	}
	name := rapid.SampledFrom(slices.Sorted(maps.Keys(m.playing))).Draw(r, "name")
	require.NoError(r, m.servers.Delete(namespace, name))
	delete(m.playing, name)
	maps.DeleteFunc(m.heldBy, func(_, held string) bool { return held == name })
}

// aServiceWith returns a Service over a store of its own, with that many
// Ready GameServers on one node. The store saves every write itself.
func aServiceWith(t *testing.T, ready int) (*Service, store.Typed[*agonesv1.GameServer]) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = store.NodesOf(s).Create(&litev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: litev1.NodeStatus{Phase: litev1.NodeReady}})
	require.NoError(t, err)
	servers := store.GameServersOf(s)
	for i := range ready {
		_, err := servers.Create(server(fmt.Sprint("gs-", i), agonesv1.GameServerStateReady, on("node")))
		require.NoError(t, err)
	}
	return NewService(s, nil), servers
}

// askOnce is one request, named by key unless that is empty.
func askOnce(s *Service, key string) (name string, code codes.Code) {
	request := &pb.AllocationRequest{Namespace: namespace}
	if key != "" {
		request.Metadata = &pb.MetaPatch{Annotations: map[string]string{KeyAnnotation: key}}
	}
	answer, err := s.Allocate(context.Background(), request)
	return answer.GetGameServerName(), status.Code(err)
}
