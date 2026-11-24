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

// Read implements io.Reader. A stream that ends inside a frame ends with
// io.ErrUnexpectedEOF, so that a log cut short is not taken for a whole one.
func (f *frames) Read(p []byte) (int, error) {
	// A loop, because Docker sends a frame of nothing for an empty write,
	// and a reader that answers with nothing and no error is asked again.
	for f.left == 0 {
		if err := f.nextFrame(); err != nil {
			return 0, err
		}
	}
	if len(p) > f.left {
		p = p[:f.left]
	}
	n, err := f.body.Read(p)
	f.left -= n
	if err == io.EOF && f.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// nextFrame reads the header of the next frame. The stream may end before
// one, with io.EOF, but not inside one.
func (f *frames) nextFrame() error {
	var header [8]byte
	if _, err := io.ReadFull(f.body, header[:]); err != nil {
		return err
	}
	f.left = int(binary.BigEndian.Uint32(header[4:]))
	return nil
}

// Close implements io.Closer.
func (f *frames) Close() error {
	return f.body.Close()
}
