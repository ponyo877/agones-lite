package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ponyo877/agones-lite/pkg/apiserver"
	"github.com/ponyo877/agones-lite/pkg/auth"
)

const defaultDataDir = "agones-lite-data"

// client calls one of the control plane's HTTP APIs.
type client struct {
	base  string
	token string
	http  *http.Client
}

// connection is how a client command finds and trusts the control plane.
// Each setting comes from a flag, else from the environment, else from the
// control plane's data directory when the command runs on the same machine.
type connection struct {
	server    string
	token     string
	caFile    string
	dataDir   string
	namespace string
}

// clientFlags adds the flags every client command has.
func clientFlags(fs *flag.FlagSet) *connection {
	c := &connection{}
	fs.StringVar(&c.server, "server", env("AGONES_LITE_SERVER", "https://127.0.0.1:9443"), "address of the management API (or $AGONES_LITE_SERVER)")
	fs.StringVar(&c.token, "token", os.Getenv("AGONES_LITE_TOKEN"), "token to present (or $AGONES_LITE_TOKEN)")
	fs.StringVar(&c.caFile, "ca-file", os.Getenv("AGONES_LITE_CA_FILE"), "CA certificate of the control plane (or $AGONES_LITE_CA_FILE)")
	fs.StringVar(&c.dataDir, "data-dir", env("AGONES_LITE_DATA_DIR", defaultDataDir), "data directory of a control plane on this machine, for its token and CA")
	fs.StringVar(&c.namespace, "n", "default", "namespace")
	return c
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// client returns a client for the address, presenting the token of role
// unless one was given explicitly.
func (c *connection) client(address string, role auth.Role) (client, error) {
	token := c.token
	if token == "" {
		raw, _ := os.ReadFile(filepath.Join(c.dataDir, "tokens", string(role)))
		token = strings.TrimSpace(string(raw))
	}
	out := client{base: strings.TrimSuffix(address, "/"), token: token, http: http.DefaultClient}
	if !strings.HasPrefix(address, "https://") {
		return out, nil
	}
	caFile := c.caFile
	if caFile == "" {
		caFile = filepath.Join(c.dataDir, "tls", "ca.crt")
	}
	trust, err := auth.TrustFile(caFile)
	if err != nil {
		return client{}, err
	}
	out.http = &http.Client{Transport: &http.Transport{TLSClientConfig: trust}}
	return out, nil
}

// admin returns a client for the management API.
func (c *connection) admin() (client, error) {
	return c.client(c.server, auth.Admin)
}

// stream copies a response body to out as it arrives. It is for answers
// that may never end, such as a log that is being followed.
func (c client) stream(ctx context.Context, path string, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		failure := &apiserver.Error{Status: resp.StatusCode}
		_ = json.NewDecoder(resp.Body).Decode(failure)
		return failure
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// call sends one request. The response is decoded into out when not nil.
// Warnings the server attached are returned to be shown to the user.
func (c client) call(ctx context.Context, method, path string, body []byte, out any) (warnings []string, err error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		failure := &apiserver.Error{Status: resp.StatusCode}
		_ = json.Unmarshal(data, failure)
		return nil, failure
	}
	if out != nil {
		err = json.Unmarshal(data, out)
	}
	return resp.Header.Values("Warning"), err
}
