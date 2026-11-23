package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Spec describes a container to create.
type Spec struct {
	Name       string
	Image      string
	Command    []string // replaces the image's entrypoint when set
	Args       []string // replaces the image's default arguments when set
	Env        []string // "KEY=value"
	WorkingDir string
	User       string // "uid" or "uid:gid"
	Labels     map[string]string

	// ShareNetworkWith names a container whose network this one joins, so
	// both see the same localhost. Ports and ExtraHosts must then be empty:
	// they belong to the container that owns the network.
	ShareNetworkWith string
	Ports            []Port
	ExtraHosts       []string // "name:address"

	NanoCPUs       int64 // CPU limit in billionths of a core; zero means none
	MemoryBytes    int64 // memory limit; zero means none
	Binds          []string
	ReadOnlyRootFS bool
}

// Port publishes a container port on the host.
type Port struct {
	Container int32
	Host      int32
	Protocol  string // "tcp" or "udp"
}

// Container is what the daemon knows about an existing container.
type Container struct {
	ID       string
	Name     string
	Labels   map[string]string
	State    string // "created", "running", "exited", ...
	ExitCode int    // set when State is "exited"
}

// Exited reports whether the container ran and stopped.
func (c Container) Exited() bool {
	return c.State == "exited" || c.State == "dead"
}

// createRequest is the body of POST /containers/create.
type createRequest struct {
	Image        string              `json:"Image"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	User         string              `json:"User,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   hostConfig          `json:"HostConfig"`
}

type hostConfig struct {
	NetworkMode    string                   `json:"NetworkMode,omitempty"`
	PortBindings   map[string][]portBinding `json:"PortBindings,omitempty"`
	ExtraHosts     []string                 `json:"ExtraHosts,omitempty"`
	NanoCPUs       int64                    `json:"NanoCpus,omitempty"`
	Memory         int64                    `json:"Memory,omitempty"`
	Binds          []string                 `json:"Binds,omitempty"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs,omitempty"`
}

type portBinding struct {
	HostPort string `json:"HostPort"`
}

// Create makes a container without starting it and returns its ID.
func (c *Client) Create(ctx context.Context, spec Spec) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	query := url.Values{"name": {spec.Name}}
	if err := c.call(ctx, http.MethodPost, "/containers/create", query, newCreateRequest(spec), &created); err != nil {
		return "", fmt.Errorf("create %s: %w", spec.Name, err)
	}
	return created.ID, nil
}

func newCreateRequest(spec Spec) createRequest {
	req := createRequest{
		Image:      spec.Image,
		Entrypoint: spec.Command,
		Cmd:        spec.Args,
		Env:        spec.Env,
		WorkingDir: spec.WorkingDir,
		User:       spec.User,
		Labels:     spec.Labels,
		HostConfig: hostConfig{
			ExtraHosts:     spec.ExtraHosts,
			NanoCPUs:       spec.NanoCPUs,
			Memory:         spec.MemoryBytes,
			Binds:          spec.Binds,
			ReadonlyRootfs: spec.ReadOnlyRootFS,
		},
	}
	if spec.ShareNetworkWith != "" {
		req.HostConfig.NetworkMode = "container:" + spec.ShareNetworkWith
		return req
	}
	req.ExposedPorts = map[string]struct{}{}
	req.HostConfig.PortBindings = map[string][]portBinding{}
	for _, p := range spec.Ports {
		key := fmt.Sprintf("%d/%s", p.Container, p.Protocol)
		req.ExposedPorts[key] = struct{}{}
		req.HostConfig.PortBindings[key] = []portBinding{{HostPort: strconv.Itoa(int(p.Host))}}
	}
	return req
}

// Start runs a created container.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// Stop asks a container to exit and kills it after grace.
// Stopping a container that is already gone is not an error.
func (c *Client) Stop(ctx context.Context, id string, grace time.Duration) error {
	query := url.Values{"t": {strconv.Itoa(int(grace.Seconds()))}}
	return ignoreNotFound(c.call(ctx, http.MethodPost, "/containers/"+id+"/stop", query, nil, nil))
}

// Remove deletes a container and its anonymous volumes, killing it if needed.
// Removing a container that is already gone is not an error.
func (c *Client) Remove(ctx context.Context, id string) error {
	query := url.Values{"force": {"true"}, "v": {"true"}}
	return ignoreNotFound(c.call(ctx, http.MethodDelete, "/containers/"+id, query, nil, nil))
}

// List returns every container, running or not, that carries the label.
func (c *Client) List(ctx context.Context, label string) ([]Container, error) {
	var listed []struct {
		ID     string `json:"Id"`
		Names  []string
		Labels map[string]string
		State  string
	}
	if err := c.call(ctx, http.MethodGet, "/containers/json", listQuery("label", label), nil, &listed); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(listed))
	for _, l := range listed {
		container := Container{ID: l.ID, Labels: l.Labels, State: l.State}
		if len(l.Names) > 0 {
			container.Name = strings.TrimPrefix(l.Names[0], "/")
		}
		if container.Exited() {
			container.ExitCode = c.exitCode(ctx, l.ID)
		}
		out = append(out, container)
	}
	return out, nil
}

// exitCode asks for the detail the list call leaves out. A container that
// vanished in between counts as a failure.
func (c *Client) exitCode(ctx context.Context, id string) int {
	var detail struct {
		State struct {
			ExitCode int
		}
	}
	if err := c.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &detail); err != nil {
		return -1
	}
	return detail.State.ExitCode
}

// RemoveVolumes deletes the named volumes whose name starts with prefix.
func (c *Client) RemoveVolumes(ctx context.Context, prefix string) error {
	var listed struct {
		Volumes []struct {
			Name string
		}
	}
	if err := c.call(ctx, http.MethodGet, "/volumes", listQuery("name", prefix), nil, &listed); err != nil {
		return err
	}
	for _, v := range listed.Volumes {
		if !strings.HasPrefix(v.Name, prefix) {
			continue // the daemon matches substrings, we want prefixes
		}
		if err := ignoreNotFound(c.call(ctx, http.MethodDelete, "/volumes/"+v.Name, nil, nil, nil)); err != nil {
			return err
		}
	}
	return nil
}

// listQuery builds the filter the list endpoints expect.
func listQuery(field, value string) url.Values {
	filters, _ := json.Marshal(map[string][]string{field: {value}})
	return url.Values{"all": {"true"}, "filters": {string(filters)}}
}

func ignoreNotFound(err error) error {
	if IsNotFound(err) {
		return nil
	}
	return err
}
