package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

// The Docker Engine API has versions, and a daemon speaks a range of them:
// Docker 29 dropped every one before 1.44. This client knows a range too.
// Its first request asks the daemon for the daemon's range, and from then on
// it speaks the newest version both know.

// apiVersion is a version of the Docker Engine API: {1, 44} is "1.44". It is
// its numbers that are compared, not its text, so 1.9 comes before 1.44.
type apiVersion [2]int

// The versions this client can speak. What it sends and reads has had the
// same shape from the oldest, which came with Docker 20.10, to the newest.
var (
	oldestAPI = apiVersion{1, 41}
	newestAPI = apiVersion{1, 52}
)

func parseAPIVersion(text string) (apiVersion, error) {
	var v apiVersion
	if _, err := fmt.Sscanf(text, "%d.%d", &v[0], &v[1]); err != nil {
		return v, fmt.Errorf("the daemon gave %q for an API version", text)
	}
	return v, nil
}

func (v apiVersion) String() string {
	return fmt.Sprintf("%d.%d", v[0], v[1])
}

func (v apiVersion) before(w apiVersion) bool {
	return slices.Compare(v[:], w[:]) < 0
}

// negotiate picks the API version to speak with a daemon that knows the
// versions from daemonOldest to daemonNewest: the newest one this client
// knows as well. When there is none, the error says which side to upgrade.
func negotiate(daemonNewest, daemonOldest string) (string, error) {
	newest, err := parseAPIVersion(daemonNewest)
	if err != nil {
		return "", err
	}
	if newest.before(oldestAPI) {
		return "", fmt.Errorf("the daemon is too old: it speaks the Docker API up to %s, and scrim needs %s, which came with Docker 20.10: upgrade Docker", newest, oldestAPI)
	}
	oldest, err := parseAPIVersion(daemonOldest)
	if err != nil {
		return "", err
	}
	if newest.before(oldest) {
		return "", fmt.Errorf("the daemon says it speaks the Docker API from %s to %s, which is no version at all", oldest, newest)
	}
	if newestAPI.before(oldest) {
		return "", fmt.Errorf("scrim is too old for this daemon: it speaks the Docker API up to %s, and the daemon needs %s or newer: upgrade scrim", newestAPI, oldest)
	}
	if newest.before(newestAPI) {
		return newest.String(), nil
	}
	return newestAPI.String(), nil
}

// version returns the API version to speak. The first call asks the daemon
// which versions it knows, on the one path that carries no version. The
// answer is kept for as long as the client lives.
func (c *Client) version(ctx context.Context) (string, error) {
	c.asking.Lock()
	defer c.asking.Unlock()
	if c.api != "" {
		return c.api, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var daemon struct {
		Newest string `json:"ApiVersion"`
		Oldest string `json:"MinAPIVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&daemon); err != nil {
		return "", fmt.Errorf("docker: %w", err)
	}
	if c.api, err = negotiate(daemon.Newest, daemon.Oldest); err != nil {
		return "", fmt.Errorf("docker: %w", err)
	}
	return c.api, nil
}
