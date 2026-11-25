package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlagsMayComeBeforeOrAfterPositionalArguments(t *testing.T) {
	for _, args := range [][]string{
		{"-n", "prod", "gameservers", "one"},
		{"gameservers", "-n", "prod", "one"},
		{"gameservers", "one", "-n", "prod"},
	} {
		fs := flag.NewFlagSet("get", flag.ContinueOnError)
		namespace := fs.String("n", "default", "")
		positional, err := parse(fs, args)
		require.NoError(t, err)
		assert.Equal(t, []string{"gameservers", "one"}, positional, "%v", args)
		assert.Equal(t, "prod", *namespace, "%v", args)
	}
}

func TestAConfigFileFillsInWhatTheCommandLineLeftOut(t *testing.T) {
	file := filepath.Join(t.TempDir(), "server.yaml")
	require.NoError(t, os.WriteFile(file, []byte("data-dir: /var/lib/scrim\nmax-gameservers: 50\nno-agent: true\nlisten: 0.0.0.0:9443\n"), 0o600))

	o, err := serverFlags([]string{"--config", file, "--listen", "127.0.0.1:1234"})

	require.NoError(t, err)
	assert.Equal(t, "/var/lib/scrim", o.dataDir)
	assert.Equal(t, int32(50), o.node.agent.MaxGameServers)
	assert.True(t, o.noAgent)
	assert.Equal(t, "127.0.0.1:1234", o.listen, "the command line wins over the file")
}

func TestAConfigFileWithAnUnknownKeyIsAnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "server.yaml")
	require.NoError(t, os.WriteFile(file, []byte("data-directory: /oops\n"), 0o600))

	_, err := serverFlags([]string{"--config", file})

	assert.ErrorContains(t, err, "data-directory")
}

// serverFlagsFrom reads the server's flags from a config file with this
// content and nothing else.
func serverFlagsFrom(t *testing.T, content string) (*serverOptions, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "server.yaml")
	require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
	return serverFlags([]string{"--config", file})
}

func TestAConfigFileValueIsReadTheWayItWouldBeTyped(t *testing.T) {
	tests := map[string]struct {
		content string
		check   func(t *testing.T, o *serverOptions)
	}{
		"a key without a value leaves the flag as it was": {
			content: "listen:\n",
			check:   func(t *testing.T, o *serverOptions) { assert.Equal(t, "127.0.0.1:9443", o.listen) },
		},
		"a list is joined with commas": {
			content: "tls-san: [cp.example.com, 192.0.2.1]\n",
			check:   func(t *testing.T, o *serverOptions) { assert.Equal(t, "cp.example.com,192.0.2.1", o.tlsNames) },
		},
		"a list may be written one item to a line": {
			content: "tls-san:\n- cp.example.com\n- 192.0.2.1\n",
			check:   func(t *testing.T, o *serverOptions) { assert.Equal(t, "cp.example.com,192.0.2.1", o.tlsNames) },
		},
		"a large number stays a whole number": {
			content: "max-gameservers: 1000000\n",
			check:   func(t *testing.T, o *serverOptions) { assert.Equal(t, int32(1000000), o.node.agent.MaxGameServers) },
		},
		"a number with an exponent is written out": {
			content: "max-gameservers: 1e3\n",
			check:   func(t *testing.T, o *serverOptions) { assert.Equal(t, int32(1000), o.node.agent.MaxGameServers) },
		},
		"a number of more digits than a float holds keeps them all": {
			content: "node-name: 12345678901234567\n",
			check:   func(t *testing.T, o *serverOptions) { assert.Equal(t, "12345678901234567", o.node.agent.NodeName) },
		},
		"an empty text is a value like any other": {
			content: "listen: \"\"\n",
			check:   func(t *testing.T, o *serverOptions) { assert.Empty(t, o.listen) },
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			o, err := serverFlagsFrom(t, tc.content)

			require.NoError(t, err)
			tc.check(t, o)
		})
	}
}

func TestAConfigFileValueThatNoFlagCanTakeIsAnError(t *testing.T) {
	tests := map[string]struct {
		content string
		want    string
	}{
		"a map": {
			content: "tls-san:\n  name: cp.example.com\n",
			want:    "tls-san: a flag takes one value or a list of them, not a map",
		},
		"a list within a list": {
			content: "tls-san: [[cp.example.com]]\n",
			want:    "tls-san: every item of a list must be a single value",
		},
		"a list with an empty item": {
			content: "tls-san:\n- cp.example.com\n-\n",
			want:    "tls-san: every item of a list must be a single value",
		},
		"a number with a fraction for a flag that counts": {
			content: "max-gameservers: 1.5\n",
			want:    `max-gameservers: parse error`,
		},
		"an unknown key, even without a value": {
			content: "data-directory:\n",
			want:    "data-directory: there is no such flag",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := serverFlagsFrom(t, tc.content)

			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestKindsAreFoundByAnyOfTheirNames(t *testing.T) {
	for _, word := range []string{"Fleet", "fleet", "fleets", "flt"} {
		k, err := findKind(word)
		require.NoError(t, err, word)
		assert.Equal(t, "fleets", k.plural)
	}
	_, err := findKind("pods")
	assert.Error(t, err)
	assert.Equal(t, "/v1/namespaces/prod/gameservers/one", kinds[0].path("prod", "one"))
}

func TestARefusalIsReadFromEitherAPI(t *testing.T) {
	for body, want := range map[string]string{
		`{"error": "fleet \"simple\" not found"}`:                                `fleet "simple" not found (HTTP 404)`,
		`{"code": 8, "message": "there is no available GameServer to allocate"}`: "there is no available GameServer to allocate (HTTP 404)",
		`<html>proxy says no</html>`:                                             "Not Found (HTTP 404)",
	} {
		assert.EqualError(t, failure(404, []byte(body)), want)
	}
}

// watched is what the management API sends for "?watch=true": two fleets
// that exist, the sign that this is all, and one change.
func watched(w http.ResponseWriter) {
	for _, event := range []string{
		`Added {"metadata": {"name": "a"}}`,
		`Added {"metadata": {"name": "a-much-longer-name"}}`,
		`Synced {}`,
		`Deleted {"metadata": {"name": "a"}}`,
	} {
		name, data, _ := strings.Cut(event, " ")
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	}
	_ = http.NewResponseController(w).Flush()
}

type named struct {
	Metadata struct{ Name string }
}

func nameAndYes(n named) string { return n.Metadata.Name + "\tyes" }

// printed is a table that tells when its last row is there.
type printed struct {
	strings.Builder
	last     string
	complete func()
}

func (p *printed) Write(row []byte) (int, error) {
	n, err := p.Builder.Write(row)
	if strings.HasPrefix(string(row), p.last) {
		p.complete()
	}
	return n, err
}

func TestFollowPrintsTheListAndThenEveryChange(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "true", r.URL.Query().Get("watch"))
		watched(w)
		<-r.Context().Done() // a watch goes on until the client leaves
	}))
	defer api.Close()
	ctx, stop := context.WithCancel(context.Background())
	out := &printed{last: "Deleted", complete: stop} // the user has seen enough

	err := follow(ctx, client{base: api.URL, http: api.Client()}, "/v1/fleets", "NAME\tOK", nameAndYes, out)

	require.NoError(t, err, "stopping a watch is how it is meant to end")
	assert.Equal(t, []string{
		"EVENT      NAME                 OK",
		"Added      a                    yes",
		"Added      a-much-longer-name   yes",
		"Deleted    a                    yes",
		"",
	}, strings.Split(out.String(), "\n"))
}

// The server closes a watch when it restarts, and when the store gives up
// on a watcher that fell behind. The table would then stay as it is while
// the world moves on, so "get -w" must not end as if all were well.
func TestFollowFailsWhenTheServerEndsTheWatch(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { watched(w) }))
	defer api.Close()
	out := &strings.Builder{}

	err := follow(context.Background(), client{base: api.URL, http: api.Client()}, "/v1/fleets", "NAME\tOK", nameAndYes, out)

	assert.ErrorIs(t, err, errWatchClosed)
	assert.Contains(t, out.String(), "Deleted    a", "what came before the end is still shown")
}

// A connection that breaks in the middle of the stream is told apart from
// one the server closed: the user learns what broke.
func TestFollowFailsWhenTheConnectionBreaks(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		watched(w)
		panic(http.ErrAbortHandler) // what auth.RequireHTTP does when the token is rotated
	}))
	defer api.Close()

	err := follow(context.Background(), client{base: api.URL, http: api.Client()}, "/v1/fleets", "NAME\tOK", nameAndYes, &strings.Builder{})

	assert.ErrorContains(t, err, "the watch broke off")
	assert.NotErrorIs(t, err, errWatchClosed)
}

func TestBufferSaysHowManyToKeepReadyAndWhy(t *testing.T) {
	var out strings.Builder

	err := runBuffer(context.Background(), []string{"--rate", "2", "--startup", "5s"}, &out)

	require.NoError(t, err)
	assert.Equal(t, `bufferSize: 90

At 2 allocations a second, 70 are asked for in the 30s between two looks of
the autoscaler and the 5s a game server takes to be Ready. What is Ready or
on its way at one look has to do for that long. With 90, a request finds
nothing Ready after at most 1 of every 100 looks.
`, out.String())
}

func TestBufferAsksForARateAndAStartupTimeThatMakeSense(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--rate", "0", "--startup", "5s"},
		{"--rate", "2", "--startup", "-1s"},
		{"--rate", "2", "--startup", "5s", "--sync", "0s"},
		{"--rate", "2", "--startup", "5s", "--miss", "0"},
		{"--rate", "2", "--startup", "5s", "--miss", "1"},
	} {
		assert.ErrorContains(t, runBuffer(context.Background(), args, io.Discard), "usage: scrim buffer", "%v", args)
	}
}
