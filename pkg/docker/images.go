package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HasImage reports whether the daemon already has the image.
func (c *Client) HasImage(ctx context.Context, image string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/images/"+image+"/json", nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Pull downloads an image, using whatever registry credentials the daemon has.
func (c *Client) Pull(ctx context.Context, image string) error {
	name, tag := SplitTag(image)
	resp, err := c.send(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := pullError(resp.Body); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

// pullError reads the progress stream to its end. The daemon answers 200 as
// soon as the pull starts, and reports failures as a message in the stream.
func pullError(progress io.Reader) error {
	decoder := json.NewDecoder(progress)
	for {
		var message struct {
			Error string `json:"error"`
		}
		err := decoder.Decode(&message)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if message.Error != "" {
			return errors.New(message.Error)
		}
	}
}

// SplitTag separates "repo/name:tag" into its two parts. A digest reference
// ("name@sha256:...") is passed whole, and a missing tag means "latest".
func SplitTag(image string) (name, tag string) {
	if strings.Contains(image, "@") {
		return image, ""
	}
	colon := strings.LastIndex(image, ":")
	if colon <= strings.LastIndex(image, "/") {
		return image, "latest" // no colon, or the only one belongs to a registry port
	}
	return image[:colon], image[colon+1:]
}
