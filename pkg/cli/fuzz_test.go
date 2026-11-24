package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/ponyo877/agones-lite/pkg/apiserver"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// The fuzz tests of this package feed its readers what comes from outside
// the program: a config file an operator wrote, and the stream of a watch.
// Run one with
//
//	go test -fuzz=FuzzConfigFile -fuzztime=2m ./pkg/cli

// someFlags is a flag set with a flag of every kind the commands have, and
// one that was given on the command line.
type someFlags struct {
	*flag.FlagSet
	text, list, given *string
	number            *int
	on                *bool
	wait              *time.Duration
}

func newFlags(t testing.TB) someFlags {
	fs := flag.NewFlagSet("fuzz", flag.ContinueOnError)
	flags := someFlags{
		FlagSet: fs,
		text:    fs.String("text", "unset", ""),
		list:    fs.String("list", "unset", ""),
		given:   fs.String("given", "unset", ""),
		number:  fs.Int("number", -1, ""),
		on:      fs.Bool("on", false, ""),
		wait:    fs.Duration("wait", time.Second, ""),
	}
	require.NoError(t, fs.Parse([]string{"--given", "on the command line"}))
	return flags
}

// FuzzConfigFile: a config file is anything an operator can save. Reading
// it never panics, and it never changes a flag that was given on the
// command line: the file only fills in what was left out.
func FuzzConfigFile(f *testing.F) {
	f.Add([]byte("text: hello\nnumber: 5\non: true\nwait: 90s\nlist: [a, b]\n"))
	f.Add([]byte("given: from the file\n"))
	f.Add([]byte("text:\nnumber: 1e6\nlist:\n- a\n-\n"))
	f.Add([]byte("number: 1.5\ntext: {a: b}\nnothing: here\n"))
	f.Add([]byte("text: 007\nnumber: 0x10\non: yes\nwait: 5\nlist: [[a]]\n"))
	f.Add([]byte("- a list\n"))
	f.Add([]byte("text: &a [*a]\n"))
	f.Fuzz(func(t *testing.T, content []byte) {
		flags := newFlags(t)

		_ = fillFrom(flags.FlagSet, content)

		assert.Equal(t, "on the command line", *flags.given)
	})
}

// FuzzConfigValues: a value written into a config file the way YAML writes
// it is the value the flag gets, as if it had been typed on the command
// line: no digit of a large number lost, no text taken for a number.
func FuzzConfigValues(f *testing.F) {
	f.Add("hello", 5, true, int64(90), "a", "b")
	f.Add("007", 9007199254740993, false, int64(0), "1.10", "yes")
	f.Add("", -1, false, int64(-5), "", " ")
	f.Add("null", 0, true, int64(1), "~", "[x]")
	f.Fuzz(func(t *testing.T, text string, number int, on bool, wait int64, first, second string) {
		valid := utf8.ValidString(text) && utf8.ValidString(first) && utf8.ValidString(second)
		if !valid || strings.Contains(first+second, ",") {
			t.Skip("not text a flag could be given: YAML holds no broken UTF-8, and a list is joined with commas")
		}
		written, err := yaml.Marshal(map[string]any{
			"text": text, "number": number, "on": on, "wait": fmt.Sprint(time.Duration(wait)), "list": []string{first, second},
		})
		if err != nil || !keptByYAML(written, text, first, second) {
			t.Skip("YAML does not hold this text: it cannot write DEL, and reads the line break U+0085 as a space")
		}
		flags := newFlags(t)

		err = fillFrom(flags.FlagSet, written)

		require.NoError(t, err, "%s", written)
		assert.Equal(t, text, *flags.text, "%s", written)
		assert.Equal(t, number, *flags.number, "%s", written)
		assert.Equal(t, on, *flags.on, "%s", written)
		assert.Equal(t, time.Duration(wait), *flags.wait, "%s", written)
		assert.Equal(t, first+","+second, *flags.list, "%s", written)
	})
}

// keptByYAML reports whether YAML, read without us, gives back the texts
// that were written. What it loses, no reader of ours could keep.
func keptByYAML(written []byte, text, first, second string) bool {
	var read struct {
		Text string   `json:"text"`
		List []string `json:"list"`
	}
	return yaml.Unmarshal(written, &read) == nil && read.Text == text && len(read.List) == 2 && read.List[0] == first && read.List[1] == second
}

// FuzzEventStream: the stream of a watch is whatever the other end of the
// connection sends. Reading it never panics and always comes to an end
// that is an error, because a watch has no good end. And the events read
// do not hang on how the connection cut the stream into pieces.
func FuzzEventStream(f *testing.F) {
	f.Add([]byte("event: Added\ndata: {\"metadata\":{\"name\":\"a\"}}\n\nevent: Synced\ndata: {}\n\n"))
	f.Add([]byte("data: no event before it\r\nevent: Deleted\r\ndata: cut off"))
	f.Add([]byte("data\nevent\n: a comment\nid: 7\ndata:no space\n\n\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, stream []byte) {
		whole, end := eventsIn(bytes.NewReader(stream))
		byTheByte, sameEnd := eventsIn(iotest.OneByteReader(bytes.NewReader(stream)))

		assert.ErrorIs(t, end, errWatchClosed, "nothing in a stream this short can break it")
		assert.Equal(t, whole, byTheByte)
		assert.Equal(t, end, sameEnd)
		assert.LessOrEqual(t, len(whole), bytes.Count(stream, []byte("data")))
	})
}

// eventsIn reads a stream to its end and returns what it held, each event
// as its name, a space, and its data.
func eventsIn(stream io.Reader) (events []string, end error) {
	end = readEvents(stream, func(event string, data []byte) error {
		events = append(events, event+" "+string(data))
		return nil
	})
	return events, end
}

// inMemory is a network of one server that never touches the network: a
// fuzz test that opened a real connection for every input would soon have
// used up the ports of the machine.
type inMemory struct {
	arrived chan net.Conn
}

func (m inMemory) Accept() (net.Conn, error) {
	conn, open := <-m.arrived
	if !open {
		return nil, net.ErrClosed
	}
	return conn, nil
}

func (m inMemory) Close() error   { close(m.arrived); return nil }
func (m inMemory) Addr() net.Addr { return &net.UnixAddr{Name: "in memory"} }

func (m inMemory) dial(context.Context, string, string) (net.Conn, error) {
	near, far := net.Pipe()
	m.arrived <- far
	return near, nil
}

// served returns a client of the management API over s, with real HTTP in
// between: what is tested is what goes over the wire.
func served(t testing.TB, s *store.Store) client {
	network := inMemory{arrived: make(chan net.Conn)}
	server := &http.Server{Handler: apiserver.New(s, nil, nil, nil).Handler()}
	go func() { _ = server.Serve(network) }()
	t.Cleanup(func() { _ = server.Close() })
	return client{base: "http://api", http: &http.Client{Transport: &http.Transport{DialContext: network.dial}}}
}

// FuzzWatch: what the server writes into a watch is what the client reads
// out of it, whatever text the object holds. A watch is lines of text, and
// an object may hold line breaks of every kind, and text that looks like
// the next event.
func FuzzWatch(f *testing.F) {
	s, err := store.Open(filepath.Join(f.TempDir(), "state.db"), store.Kinds())
	require.NoError(f, err)
	f.Cleanup(func() { _ = s.Close() })
	api, fleets := served(f, s), store.FleetsOf(s)
	f.Add("note", "two\nlines")
	f.Add("data: {}", "\n\nevent: Deleted\ndata: {}\n\n")
	f.Add("\r", "\u2028\u2029\u0085 \x00 \\n \"")
	f.Add("", strings.Repeat("long ", 20000))
	f.Fuzz(func(t *testing.T, key, value string) {
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			t.Skip("JSON keeps no broken UTF-8, so the store would not either")
		}
		fleet := &agonesv1.Fleet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fleet", Annotations: map[string]string{key: value}}}
		stored, err := fleets.Create(fleet)
		require.NoError(t, err)
		defer func() { require.NoError(t, fleets.Delete("default", "fleet")) }()
		ctx, seenEnough := context.WithCancel(context.Background())
		defer seenEnough()
		var events []string
		read := &agonesv1.Fleet{}

		err = api.events(ctx, "/v1/namespaces/default/fleets?watch=true", func(event string, data []byte) error {
			events = append(events, event)
			if event == apiserver.Synced {
				seenEnough()
				return nil
			}
			return json.Unmarshal(data, read)
		})

		require.NoError(t, err)
		assert.Equal(t, []string{string(store.Added), apiserver.Synced}, events)
		assert.True(t, apiequality.Semantic.DeepEqual(stored, read), "sent %+v\nread %+v", stored.Annotations, read.Annotations)
	})
}

// A watch that is left alone ends the way the server ends it, which the
// same in-memory network shows without a fuzzer.
func TestAWatchOverTheWireEndsWhenTheServerGoesAway(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	network := inMemory{arrived: make(chan net.Conn)}
	server := &http.Server{Handler: apiserver.New(s, nil, nil, nil).Handler()}
	go func() { _ = server.Serve(network) }()
	api := client{base: "http://api", http: &http.Client{Transport: &http.Transport{DialContext: network.dial}}}

	err = api.events(context.Background(), "/v1/namespaces/default/fleets?watch=true", func(event string, _ []byte) error {
		if event == apiserver.Synced {
			go func() { _ = server.Close() }() // the control plane is restarted
		}
		return nil
	})

	require.Error(t, err, "the watch must not end as if nothing had happened")
	assert.False(t, errors.Is(err, context.Canceled))
}
