package docker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The grammar of an image reference, [registry/]path[:tag][@digest], as the
// daemon reads it. It is the one of github.com/distribution/reference.
const (
	hostLabel    = `[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?`
	registryHost = `(?:` + hostLabel + `(?:\.` + hostLabel + `)*|\[[a-fA-F0-9:]+\])(?::[0-9]+)?`
	pathPart     = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	imageName    = `(?:` + registryHost + `/)?` + pathPart + `(?:/` + pathPart + `)*`
	imageTag     = `\w[\w.-]{0,127}`
	imageDigest  = `[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}`

	// maxImageName is the longest name, tag and digest aside.
	maxImageName = 255
)

var imageReference = regexp.MustCompile(`^(` + imageName + `)(?::` + imageTag + `)?(?:@` + imageDigest + `)?$`)

// ValidImage reports whether image is a reference the daemon can parse.
func ValidImage(image string) bool {
	parts := imageReference.FindStringSubmatch(image)
	return parts != nil && len(parts[1]) <= maxImageName
}

// HasImage reports whether the daemon already has the image.
func (c *Client) HasImage(ctx context.Context, image string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/images/"+image+"/json", nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Pull downloads an image. The credentials that ConfigFile holds for the
// image's registry, if any, go with the request: see registry.go.
func (c *Client) Pull(ctx context.Context, image string) error {
	if err := c.pull(ctx, image); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

func (c *Client) pull(ctx context.Context, image string) error {
	name, tag := SplitTag(image)
	req, err := c.newRequest(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	auth, err := registryAuth(c.ConfigFile, image)
	if err != nil {
		return err
	}
	if auth != "" {
		req.Header.Set("X-Registry-Auth", auth)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return pullError(resp.Body)
}

// pullError reads the progress stream to its end. The daemon answers 200 as
// soon as the pull starts, and reports failures as a message in the stream.
//
// The failure is read from "errorDetail" first. The older "error" beside it
// is deprecated since API 1.48 and will be left out one day.
func pullError(progress io.Reader) error {
	decoder := json.NewDecoder(progress)
	for {
		var message struct {
			Error  string `json:"error"`
			Detail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		err := decoder.Decode(&message)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if failure := cmp.Or(message.Detail.Message, message.Error); failure != "" {
			return errors.New(failure)
		}
	}
}

// SplitTag separates "repo/name:tag" into its two parts. A digest reference
// ("name@sha256:...") is passed whole, and a missing tag means "latest".
//
// The tag of a name without a digest is never empty, whatever image is: the
// daemon takes an empty tag as a request for every tag there is.
func SplitTag(image string) (name, tag string) {
	if strings.Contains(image, "@") {
		return image, ""
	}
	colon := strings.LastIndex(image, ":")
	if colon <= strings.LastIndex(image, "/") {
		return image, "latest" // no colon, or the only one belongs to a registry port
	}
	return image[:colon], cmp.Or(image[colon+1:], "latest")
}
