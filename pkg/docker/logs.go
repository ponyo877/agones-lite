package docker

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Logs returns what a container wrote to its standard output and standard
// error, as one stream. With follow, the stream stays open and carries new
// output until the container exits or the caller closes it. tail limits the
// history to that many lines; zero means all of it.
func (c *Client) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	query := url.Values{"stdout": {"true"}, "stderr": {"true"}, "follow": {strconv.FormatBool(follow)}}
	if tail > 0 {
		query.Set("tail", strconv.Itoa(tail))
	}
	resp, err := c.send(ctx, http.MethodGet, "/containers/"+id+"/logs", query, nil)
	if err != nil {
		return nil, err
	}
	return &frames{body: resp.Body}, nil
}

// frames unwraps the stream Docker sends for a container without a
// terminal: a sequence of frames, each an 8-byte header followed by output.
// The header's first byte says stdout or stderr, its last four the length.
type frames struct {
	body io.ReadCloser
	left int // bytes of the current frame not yet read
}

// Read implements io.Reader.
func (f *frames) Read(p []byte) (int, error) {
	if f.left == 0 {
		var header [8]byte
		if _, err := io.ReadFull(f.body, header[:]); err != nil {
			return 0, err
		}
		f.left = int(binary.BigEndian.Uint32(header[4:]))
	}
	if len(p) > f.left {
		p = p[:f.left]
	}
	n, err := f.body.Read(p)
	f.left -= n
	return n, err
}

// Close implements io.Closer.
func (f *frames) Close() error {
	return f.body.Close()
}
