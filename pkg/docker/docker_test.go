package docker

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitTag(t *testing.T) {
	tests := map[string][2]string{
		"alpine":                         {"alpine", "latest"},
		"alpine:3.20":                    {"alpine", "3.20"},
		"alpine:":                        {"alpine", "latest"},
		"localhost:5000/game":            {"localhost:5000/game", "latest"},
		"localhost:5000/game:v1":         {"localhost:5000/game", "v1"},
		"example.com/game@sha256:abc123": {"example.com/game@sha256:abc123", ""},
		"":                               {"", "latest"},
	}
	for image, want := range tests {
		name, tag := SplitTag(image)
		assert.Equal(t, want, [2]string{name, tag}, image)
	}
}

func TestValidImage(t *testing.T) {
	sha := "sha256:" + strings.Repeat("ab", 32)
	valid := []string{
		"alpine", "alpine:3.20", "library/alpine", "example/game:1",
		"us-docker.pkg.dev/agones-images/examples/simple-game-server:0.44",
		"localhost:5000/game", "localhost:5000/game:v1", "registry.example.com:443/a/b/c:tag_1.2-3",
		"192.168.1.10:5000/game", "[::1]:5000/game:v1",
		"example.com/game@" + sha, "example.com/game:v1@" + sha,
		"a__b/c-d.e", "Registry/game",
	}
	for _, image := range valid {
		assert.True(t, ValidImage(image), image)
	}
	invalid := []string{
		"", "alpine:", ":latest", "Alpine", "example/Game", "alpine:la test", "alpine:.tag",
		"alpine@sha256:abc", "alpine@" + sha + "x", "a//b", "/alpine", "alpine/", "-alpine", "a..b",
		"example.com/game?tag=1", "example.com/game#1", "alpine:" + strings.Repeat("t", 129),
		"example.com/" + strings.Repeat("a", 250),
	}
	for _, image := range invalid {
		assert.False(t, ValidImage(image), image)
	}
}

func TestPullErrorFindsFailuresInTheStream(t *testing.T) {
	ok := `{"status":"Pulling"}` + "\n" + `{"status":"Done"}`
	assert.NoError(t, pullError(strings.NewReader(ok)))

	failed := `{"status":"Pulling"}` + "\n" + `{"error":"manifest unknown"}`
	assert.EqualError(t, pullError(strings.NewReader(failed)), "manifest unknown")

	// "error" is deprecated since API 1.48, to be left out one day.
	detailed := `{"status":"Pulling"}` + "\n" + `{"errorDetail":{"message":"manifest unknown"}}`
	assert.EqualError(t, pullError(strings.NewReader(detailed)), "manifest unknown")
}

func TestCreateRequest(t *testing.T) {
	owner := newCreateRequest(Spec{
		Image: "sidecar",
		Ports: []Port{{Container: 7654, Host: 7000, Protocol: "udp"}},
	})
	body, err := json.Marshal(owner)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"Image": "sidecar",
		"ExposedPorts": {"7654/udp": {}},
		"HostConfig": {"PortBindings": {"7654/udp": [{"HostPort": "7000"}]}}
	}`, string(body))

	guest := newCreateRequest(Spec{Image: "game", ShareNetworkWith: "owner", Command: []string{"/game"}})
	body, err = json.Marshal(guest)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"Image": "game",
		"Entrypoint": ["/game"],
		"HostConfig": {"NetworkMode": "container:owner"}
	}`, string(body), "a container that joins a network publishes nothing itself")
}

// Two game server ports may lead to one port of the container.
func TestTwoHostPortsCanLeadToOneContainerPort(t *testing.T) {
	req := newCreateRequest(Spec{Image: "sidecar", Ports: []Port{
		{Container: 7654, Host: 7000, Protocol: "udp"},
		{Container: 7654, Host: 7001, Protocol: "udp"},
		{Container: 7654, Host: 7000, Protocol: "tcp"},
	}})

	assert.Equal(t, map[string][]portBinding{
		"7654/udp": {{HostPort: "7000"}, {HostPort: "7001"}},
		"7654/tcp": {{HostPort: "7000"}},
	}, req.HostConfig.PortBindings)
	assert.Len(t, req.ExposedPorts, 2)
}

func TestFramesAreUnwrappedIntoOneStream(t *testing.T) {
	frame := func(stream byte, text string) string {
		return string([]byte{stream, 0, 0, 0, 0, 0, 0, byte(len(text))}) + text
	}
	wire := frame(1, "ready\n") + frame(2, "warning: low memory\n") + frame(1, "") + frame(1, "bye\n")

	out, err := io.ReadAll(&frames{body: io.NopCloser(strings.NewReader(wire))})

	require.NoError(t, err)
	assert.Equal(t, "ready\nwarning: low memory\nbye\n", string(out))
}

// Docker sends a frame of nothing for every empty write of the container.
// A reader that answered each with "nothing, and no error" would be given
// up on by its caller after a hundred of them in a row.
func TestFramesOfNothingAreSkippedHoweverManyThereAre(t *testing.T) {
	empty := string([]byte{1, 0, 0, 0, 0, 0, 0, 0})
	wire := strings.Repeat(empty, 500) + string([]byte{1, 0, 0, 0, 0, 0, 0, 3}) + "bye"
	log := &frames{body: io.NopCloser(strings.NewReader(wire))}
	buffer := make([]byte, 16)

	n, err := log.Read(buffer)

	require.NoError(t, err)
	assert.Equal(t, "bye", string(buffer[:n]), "the first read already has something to give")
}

// A log that stops in the middle of a frame was cut, by a daemon or a
// connection that went away. That is not the end of the log.
func TestAStreamThatEndsInsideAFrameIsNotTakenForAWholeLog(t *testing.T) {
	header := string([]byte{1, 0, 0, 0, 0, 0, 0, 10})
	tests := map[string]struct{ wire, read string }{
		"inside the output": {header + "only", "only"},
		"inside a header":   {header + "ten bytes!" + header[:5], "ten bytes!"},
	}
	for name, tc := range tests {
		out, err := io.ReadAll(&frames{body: io.NopCloser(strings.NewReader(tc.wire))})

		assert.ErrorIs(t, err, io.ErrUnexpectedEOF, name)
		assert.Equal(t, tc.read, string(out), "%s: what came before the cut is still read", name)
	}
}

// TestAgainstTheDaemon runs a real container. It is skipped when no Docker
// daemon answers at DefaultHost.
func TestAgainstTheDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := New(DefaultHost())
	require.NoError(t, err)
	if err := c.Ping(ctx); err != nil {
		t.Skipf("no Docker daemon at %s: %v", DefaultHost(), err)
	}

	const image, label = "alpine:3.20", "agones-lite.test"
	require.NoError(t, c.Pull(ctx, image))
	has, err := c.HasImage(ctx, image)
	require.NoError(t, err)
	assert.True(t, has)

	id, err := c.Create(ctx, Spec{
		Name:    "agones-lite-docker-test",
		Image:   image,
		Command: []string{"sh", "-c", "echo to stdout; echo to stderr >&2; exit 3"},
		Labels:  map[string]string{label: "yes"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Remove(context.Background(), id) })
	require.NoError(t, c.Start(ctx, id))

	require.Eventually(t, func() bool {
		containers, err := c.List(ctx, label)
		return err == nil && len(containers) == 1 && containers[0].Exited() && containers[0].ExitCode == 3
	}, 30*time.Second, 200*time.Millisecond, "the container should exit with code 3")

	logs, err := c.Logs(ctx, id, false, 0)
	require.NoError(t, err)
	text, err := io.ReadAll(logs)
	require.NoError(t, err)
	_ = logs.Close()
	assert.ElementsMatch(t, []string{"to stdout", "to stderr"}, strings.Split(strings.TrimSpace(string(text)), "\n"))

	require.NoError(t, c.Remove(ctx, id))
	require.NoError(t, c.Remove(ctx, id), "removing twice is fine")
	containers, err := c.List(ctx, label)
	require.NoError(t, err)
	assert.Empty(t, containers)

	_, err = c.HasImage(ctx, "agones-lite/no-such-image:none")
	assert.NoError(t, err)
}
