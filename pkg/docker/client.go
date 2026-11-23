// Package docker is a small client for the Docker Engine API.
//
// The agent needs about ten operations, so this package speaks HTTP to the
// Docker socket directly instead of pulling in Docker's own client library.
// Each exported method is one API call.
//
// Upstream counterpart: none. In Agones this is the kubelet's job.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// apiVersion is old enough for Docker 20.10 and understood by every newer one.
const apiVersion = "v1.41"

// Client talks to one Docker daemon.
type Client struct {
	http *http.Client
}

// DefaultHost returns $DOCKER_HOST, or the standard socket when it is unset.
func DefaultHost() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	return "unix:///var/run/docker.sock"
}

// New returns a client for the daemon listening on a unix socket,
// for example "unix:///var/run/docker.sock".
func New(host string) (*Client, error) {
	path, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		return nil, fmt.Errorf("docker host %q: only unix:// sockets are supported", host)
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}
	return &Client{http: &http.Client{Transport: &http.Transport{DialContext: dial}}}, nil
}

// Error is an error response from the daemon.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status)
}

// IsNotFound reports whether err says the container, image or volume is missing.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// call sends one request. in is sent as JSON when not nil; the response is
// decoded into out when not nil.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	resp, err := c.send(ctx, method, path, query, in)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// send returns the response of a successful request. The caller closes the body.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	target := "http://docker/" + apiVersion + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer func() { _ = resp.Body.Close() }()
		return nil, readError(resp)
	}
	return resp, nil
}

func readError(resp *http.Response) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	if payload.Message == "" {
		payload.Message = resp.Status
	}
	return &Error{Status: resp.StatusCode, Message: payload.Message}
}

// Ping checks that the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// BridgeGateway returns the address containers on the default network use to
// reach the host.
func (c *Client) BridgeGateway(ctx context.Context) (string, error) {
	var network struct {
		IPAM struct {
			Config []struct {
				Gateway string
			}
		}
	}
	if err := c.call(ctx, http.MethodGet, "/networks/bridge", nil, nil, &network); err != nil {
		return "", err
	}
	for _, cfg := range network.IPAM.Config {
		if cfg.Gateway != "" {
			return cfg.Gateway, nil
		}
	}
	return "", errors.New("docker: the bridge network has no gateway")
}
