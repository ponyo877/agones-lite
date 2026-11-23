package docker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
)

// Registry credentials.
//
// The daemon keeps none. The docker command reads them from its
// configuration file and sends them with every pull, and so do we: from the
// same file, in the same header.
//
// Only credentials written into the file are used. A file that names a
// credential helper instead ("credsStore" or "credHelpers", as Docker
// Desktop writes it) holds no secret, and running helpers is not supported:
// such images are pulled without credentials.

// hub is Docker Hub, as an image reference names it.
const hub = "docker.io"

// credentials are what one entry of "auths" in the configuration file
// holds, and also what the daemon wants in the X-Registry-Auth header.
type credentials struct {
	// Auth is base64 of "user:password", the form "docker login" writes.
	Auth          string `json:"auth,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	// ServerAddress is the entry's key. It is not in the file.
	ServerAddress string `json:"serveraddress,omitempty"`
}

// registryAuth returns the X-Registry-Auth header for pulling image: the
// credentials configFile holds for the image's registry, or "" when it holds
// none. A file that is not there holds none.
func registryAuth(configFile, image string) (string, error) {
	if configFile == "" {
		return "", nil
	}
	config, err := os.ReadFile(configFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	found, err := credentialsFor(config, registryOf(image))
	if err != nil {
		return "", fmt.Errorf("%s: %w", configFile, err)
	}
	return found.header(), nil
}

// registryOf returns the registry an image comes from, by Docker's rule:
// what stands before the first "/" is a registry when it has a "." or a ":"
// in it, is "localhost", or has a capital letter. Anything else is the
// first part of a repository on Docker Hub.
func registryOf(image string) string {
	first, _, more := strings.Cut(image, "/")
	named := strings.ContainsAny(first, ".:") || first == "localhost" || first != strings.ToLower(first)
	if !more || !named {
		return hub
	}
	return hostOf(first)
}

// hostOf reduces a key of "auths" to the bare host. Docker has written the
// keys as "https://host/v1/", as "host" and as "host:port" over the years,
// and Docker Hub under more than one name.
func hostOf(key string) string {
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	host, _, _ := strings.Cut(key, "/")
	if host == "index.docker.io" || host == "registry-1.docker.io" {
		return hub
	}
	return host
}

// credentialsFor reads a Docker configuration file and returns what it
// holds for registry, or nothing when it holds nothing.
func credentialsFor(config []byte, registry string) (credentials, error) {
	var file struct {
		Auths map[string]credentials `json:"auths"`
	}
	if err := json.Unmarshal(config, &file); err != nil {
		return credentials{}, err
	}
	// In the order of the keys, so that two entries for one registry
	// always give the same answer.
	for _, key := range slices.Sorted(maps.Keys(file.Auths)) {
		if hostOf(key) != registry {
			continue
		}
		found, err := file.Auths[key].decoded()
		if err != nil {
			return credentials{}, fmt.Errorf("auths[%q]: %w", key, err)
		}
		if found.header() != "" {
			found.ServerAddress = key
			return found, nil
		}
	}
	return credentials{}, nil
}

// decoded returns c with "auth" taken apart into the user and the password.
func (c credentials) decoded() (credentials, error) {
	if c.Auth == "" {
		return c, nil
	}
	raw, err := base64.StdEncoding.DecodeString(c.Auth)
	if err != nil {
		return c, fmt.Errorf("auth: %w", err)
	}
	user, password, found := strings.Cut(string(raw), ":")
	if !found || user == "" {
		return c, errors.New(`auth is not base64 of "user:password"`)
	}
	c.Auth, c.Username, c.Password = "", user, password
	return c, nil
}

// header is the value of X-Registry-Auth: the credentials as JSON, in
// URL-safe base64. It is "" when there is nothing to log in with.
func (c credentials) header() string {
	if c.Username == "" && c.IdentityToken == "" {
		return ""
	}
	raw, _ := json.Marshal(c) // a struct of strings always marshals
	return base64.URLEncoding.EncodeToString(raw)
}
