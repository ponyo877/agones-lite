package cli

import (
	"context"
	"flag"
	"fmt"
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
	require.NoError(t, os.WriteFile(file, []byte("data-dir: /var/lib/agones-lite\nmax-gameservers: 50\nno-agent: true\nlisten: 0.0.0.0:9443\n"), 0o600))

	o, err := serverFlags([]string{"--config", file, "--listen", "127.0.0.1:1234"})

	require.NoError(t, err)
	assert.Equal(t, "/var/lib/agones-lite", o.dataDir)
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

func TestFollowPrintsTheListAndThenEveryChange(t *testing.T) {
	// What the management API sends for "?watch=true": two fleets that
	// exist, the sign that this is all, and one change.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "true", r.URL.Query().Get("watch"))
		for _, event := range []string{
			`Added {"metadata": {"name": "a"}}`,
			`Added {"metadata": {"name": "a-much-longer-name"}}`,
			`Synced {}`,
			`Deleted {"metadata": {"name": "a"}}`,
		} {
			name, data, _ := strings.Cut(event, " ")
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		}
	}))
	defer api.Close()
	type named struct {
		Metadata struct{ Name string }
	}
	row := func(n named) string { return n.Metadata.Name + "\tyes" }
	out := &strings.Builder{}

	err := follow(context.Background(), client{base: api.URL, http: api.Client()}, "/v1/fleets", "NAME\tOK", row, out)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"EVENT      NAME                 OK",
		"Added      a                    yes",
		"Added      a-much-longer-name   yes",
		"Deleted    a                    yes",
		"",
	}, strings.Split(out.String(), "\n"))
}
