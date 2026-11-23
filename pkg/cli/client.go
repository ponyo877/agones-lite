package cli

import (
	"bufio"
	"bytes"
	"cmp"
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

// open starts a GET whose answer may never end, such as a log that is
// being followed. The caller closes the body.
func (c client) open(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
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
	if resp.StatusCode >= http.StatusBadRequest {
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return nil, failure(resp.StatusCode, data)
	}
	return resp.Body, nil
}

// stream copies an answer to out as it arrives.
func (c client) stream(ctx context.Context, path string, out io.Writer) error {
	body, err := c.open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	_, err = io.Copy(out, body)
	return err
}

// events reads Server-Sent Events and hands each one to handle, until the
// stream ends, handle fails, or the user has seen enough and ctx is done.
func (c client) events(ctx context.Context, path string, handle func(event string, data []byte) error) error {
	body, err := c.open(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	lines := bufio.NewScanner(body)
	lines.Buffer(nil, 16<<20) // one line is one whole object
	event := ""
	for lines.Scan() {
		field, value, _ := strings.Cut(lines.Text(), ": ")
		if field == "event" {
			event = value
		}
		if field != "data" {
			continue
		}
		if err := handle(event, []byte(value)); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil // stopped by the user
	}
	return lines.Err()
}

// failure reads why a request was refused. The management API says it under
// "error"; the allocation API answers the way gRPC does over HTTP, under
// "message".
func failure(status int, body []byte) error {
	var said struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &said)
	return &apiserver.Error{Status: status, Message: cmp.Or(said.Error, said.Message, http.StatusText(status))}
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
		return nil, failure(resp.StatusCode, data)
	}
	if out != nil {
		err = json.Unmarshal(data, out)
	}
	return resp.Header.Values("Warning"), err
}
