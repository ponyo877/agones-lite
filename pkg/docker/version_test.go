package docker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNegotiate(t *testing.T) {
	tests := map[string]struct {
		daemonNewest, daemonOldest string
		want                       string
		fails                      string // part of the error, when there is no version in common
	}{
		"Docker 28 knows less than we do: its newest":     {"1.51", "1.24", "1.51", ""},
		"Docker 29, which dropped all before 1.44":        {"1.52", "1.44", "1.52", ""},
		"Docker 20.10, the oldest we speak to":            {"1.41", "1.12", "1.41", ""},
		"a daemon that knows more than we do: our newest": {"1.60", "1.44", "1.52", ""},
		"a daemon whose oldest is our newest":             {"1.60", "1.52", "1.52", ""},
		"versions are numbers: 1.100 is after 1.52":       {"1.100", "1.44", "1.52", ""},
		"a next major version that still knows ours":      {"2.3", "1.50", "1.52", ""},
		"a daemon older than Docker 20.10":                {"1.40", "1.12", "", "upgrade Docker"},
		"versions are numbers: 1.9 is before 1.41":        {"1.9", "1.9", "", "upgrade Docker"},
		"a daemon that dropped everything we know":        {"1.60", "1.53", "", "upgrade agones-lite"},
		"a next major version that dropped ours":          {"2.3", "2.0", "", "upgrade agones-lite"},
		"no versions at all":                              {"", "", "", `""`},
		"a newest version that is not one":                {"latest", "1.44", "", `"latest"`},
		"an oldest version that is not one":               {"1.52", "", "", `""`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := negotiate(tc.daemonNewest, tc.daemonOldest)

			assert.Equal(t, tc.want, got)
			if tc.fails == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.fails)
		})
	}
}

// strictDaemon answers like Docker Engine 29: it knows the API from 1.44 to
// 1.52 and refuses a request in any older version.
func strictDaemon(t *testing.T, asked *[]string) *Client {
	t.Helper()
	return listen(t, func(w http.ResponseWriter, r *http.Request) {
		*asked = append(*asked, r.URL.Path)
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"Version": "29.1.3", "ApiVersion": "1.52", "MinAPIVersion": "1.44"}`))
			return
		}
		var minor int
		_, _ = fmt.Sscanf(r.URL.Path, "/v1.%d/", &minor)
		if minor < 44 {
			w.WriteHeader(http.StatusBadRequest)
			message := "client version 1.%d is too old. Minimum supported API version is 1.44, please upgrade your client to a newer version"
			_, _ = fmt.Fprintf(w, `{"message": %q}`, fmt.Sprintf(message, minor))
		}
	})
}

// Docker Engine 29 dropped every API version before 1.44. A client that
// always spoke 1.41 could not even ping it.
func TestTheClientSpeaksAVersionTheDaemonStillKnows(t *testing.T) {
	var asked []string
	c := strictDaemon(t, &asked)

	require.NoError(t, c.Ping(context.Background()))
	_, err := c.HasImage(context.Background(), "alpine:3.20")
	require.NoError(t, err)

	assert.Equal(t, []string{"/version", "/v1.52/_ping", "/v1.52/images/alpine:3.20/json"}, asked,
		"the daemon is asked once, and then spoken to in the newest version both know")
	assert.False(t, strings.Contains(strings.Join(asked, " "), "/v1.41/"))
}

func TestADaemonWithNoVersionInCommonIsRefusedWithAdvice(t *testing.T) {
	var asked []string
	c := listen(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		_, _ = w.Write([]byte(`{"ApiVersion": "1.60", "MinAPIVersion": "1.55"}`))
	})

	err := c.Ping(context.Background())

	assert.ErrorContains(t, err, "upgrade agones-lite")
	assert.Equal(t, []string{"/version"}, asked, "nothing is sent in a version the daemon would refuse")
}

// A daemon that is not up yet must not be given up on for good.
func TestTheDaemonIsAskedAgainAfterAFailure(t *testing.T) {
	up := false
	c := listen(t, func(w http.ResponseWriter, r *http.Request) {
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ApiVersion": "1.52", "MinAPIVersion": "1.44"}`))
	})

	require.Error(t, c.Ping(context.Background()))
	up = true
	assert.NoError(t, c.Ping(context.Background()))
}
