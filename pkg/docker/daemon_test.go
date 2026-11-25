package docker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI is the API version the fake daemon and the client settle on.
const fakeAPI = "1.47"

// fakeDaemon stands in for the Docker daemon on a unix socket, so that what
// the client really sends can be looked at. It returns a client for it.
// The daemon tells its API versions by itself; handler gets the rest.
func fakeDaemon(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	return listen(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"ApiVersion": "` + fakeAPI + `", "MinAPIVersion": "1.24"}`))
			return
		}
		handler(w, r)
	})
}

// listen serves handler on a unix socket and returns a client for it.
func listen(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	// Not t.TempDir(): its path is too long for a unix socket on macOS.
	dir, err := os.MkdirTemp("", "docker")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)

	daemon := httptest.NewUnstartedServer(handler)
	_ = daemon.Listener.Close()
	daemon.Listener = listener
	daemon.Start()
	t.Cleanup(daemon.Close)

	client, err := New("unix://" + socket)
	require.NoError(t, err)
	return client
}

// An empty tag is how the daemon is asked for every tag of an image.
func TestPullAlwaysNamesOneTagOrDigest(t *testing.T) {
	tests := map[string][2]string{
		"alpine":                         {"alpine", "latest"},
		"alpine:":                        {"alpine", "latest"},
		"localhost:5000/game:v1":         {"localhost:5000/game", "v1"},
		"example.com/game@sha256:abc123": {"example.com/game@sha256:abc123", ""},
	}
	for image, want := range tests {
		var asked [2]string
		c := fakeDaemon(t, func(_ http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/v"+fakeAPI+"/images/create", r.URL.Path)
			asked = [2]string{r.URL.Query().Get("fromImage"), r.URL.Query().Get("tag")}
		})

		require.NoError(t, c.Pull(context.Background(), image))

		assert.Equal(t, want, asked, image)
	}
}

func TestAnImageNameStaysInThePath(t *testing.T) {
	var path, query string
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusNotFound)
	})

	has, err := c.HasImage(context.Background(), "example.com/game?tag=1#latest")

	require.NoError(t, err)
	assert.False(t, has)
	assert.Equal(t, "/v"+fakeAPI+"/images/example.com/game?tag=1#latest/json", path)
	assert.Empty(t, query)
}

// The daemon's name filter finds a name anywhere in a volume's name.
func TestRemoveVolumesTakesOnlyThoseThatStartWithThePrefix(t *testing.T) {
	var removed []string
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			removed = append(removed, r.URL.Path)
			return
		}
		assert.JSONEq(t, `{"name": ["scrim_default_gs_"]}`, r.URL.Query().Get("filters"))
		_, _ = w.Write([]byte(`{"Volumes": [
			{"Name": "scrim_default_gs_tmp"},
			{"Name": "scrim_default_gs_cache"},
			{"Name": "backup-of-scrim_default_gs_tmp"}
		]}`))
	})

	require.NoError(t, c.RemoveVolumes(context.Background(), "scrim_default_gs_"))

	assert.Equal(t, []string{
		"/v" + fakeAPI + "/volumes/scrim_default_gs_tmp",
		"/v" + fakeAPI + "/volumes/scrim_default_gs_cache",
	}, removed)
}
