package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/ponyo877/agones-lite/pkg/apiserver"
)

// client calls the management API.
type client struct {
	base string
}

// clientFlags adds the flags every client command has.
func clientFlags(fs *flag.FlagSet) (server, namespace *string) {
	fallback := os.Getenv("AGONES_LITE_SERVER")
	if fallback == "" {
		fallback = "http://127.0.0.1:9443"
	}
	server = fs.String("server", fallback, "address of the management API (or $AGONES_LITE_SERVER)")
	namespace = fs.String("n", "default", "namespace")
	return server, namespace
}

// call sends one request. The response is decoded into out when not nil.
// Warnings the server attached are returned to be shown to the user.
func (c client) call(ctx context.Context, method, path string, body []byte, out any) (warnings []string, err error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
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
