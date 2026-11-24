package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ponyo877/agones-lite/pkg/auth"
)

// kind is what the command line knows about one kind of resource.
type kind struct {
	name    string   // as in a manifest: "Fleet"
	plural  string   // as in the API path: "fleets"
	aliases []string // what else a user may type
	// named kinds are applied with PUT to their own URL, so applying twice
	// updates. The others are created with POST, which allows generateName.
	named bool
	// list prints a table of every object of the kind. With watch it goes
	// on to print every change.
	list func(ctx context.Context, c client, namespace string, watch bool, out io.Writer) error
}

var kinds = []kind{
	{name: "GameServer", plural: "gameservers", aliases: []string{"gameserver", "gs"}, list: listGameServers},
	{name: "GameServerSet", plural: "gameserversets", aliases: []string{"gameserverset", "gss"}, list: listGameServerSets},
	{name: "Fleet", plural: "fleets", aliases: []string{"fleet", "flt"}, named: true, list: listFleets},
	{name: "FleetAutoscaler", plural: "fleetautoscalers", aliases: []string{"fleetautoscaler", "fas"}, named: true, list: listFleetAutoscalers},
}

// findKind looks a kind up by any of the names a user or a manifest uses.
func findKind(word string) (kind, error) {
	for _, k := range kinds {
		if word == k.name || word == k.plural || contains(k.aliases, word) {
			return k, nil
		}
	}
	return kind{}, fmt.Errorf("unknown kind %q", word)
}

func contains(words []string, word string) bool {
	for _, w := range words {
		if w == word {
			return true
		}
	}
	return false
}

// path is the URL of the kind's collection, or of one object when name is set.
func (k kind) path(namespace, name string) string {
	if name == "" {
		return collection(namespace, k.plural)
	}
	return collection(namespace, k.plural) + "/" + name
}

func collection(namespace, plural string) string {
	return "/v1/namespaces/" + namespace + "/" + plural
}

// runApply creates or updates every resource in a YAML file.
func runApply(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	conn := clientFlags(fs)
	file := fs.String("f", "", "YAML file to apply, or - for standard input")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	raw, err := readFile(*file)
	if err != nil {
		return err
	}
	c, err := conn.admin()
	if err != nil {
		return err
	}
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		if err := applyOne(ctx, c, conn.namespace, []byte(doc), out); err != nil {
			return err
		}
	}
	return nil
}

func readFile(name string) ([]byte, error) {
	switch name {
	case "":
		return nil, errors.New("this command needs -f FILE")
	case "-":
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(name)
}

// applyOne sends one YAML document to the API as it is: the server does the
// defaulting and the validation.
func applyOne(ctx context.Context, c client, namespace string, doc []byte, out io.Writer) error {
	var head struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(doc, &head); err != nil {
		return err
	}
	k, err := findKind(head.Kind)
	if err != nil {
		return err
	}
	if head.Metadata.Namespace != "" {
		namespace = head.Metadata.Namespace
	}
	method, path := http.MethodPost, k.path(namespace, "")
	if k.named {
		method, path = http.MethodPut, k.path(namespace, head.Metadata.Name)
	}
	var saved struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	warnings, err := c.call(ctx, method, path, doc, &saved)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		_, _ = fmt.Fprintln(out, "warning:", warning)
	}
	_, _ = fmt.Fprintf(out, "%s/%s applied\n", strings.ToLower(k.name), saved.Metadata.Name)
	return nil
}

// runDelete asks for a resource to be removed.
func runDelete(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	conn := clientFlags(fs)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: agones-lite delete KIND NAME")
	}
	k, err := findKind(positional[0])
	if err != nil {
		return err
	}
	c, err := conn.admin()
	if err != nil {
		return err
	}
	if _, err := c.call(ctx, http.MethodDelete, k.path(conn.namespace, positional[1]), nil, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s/%s deleted\n", strings.ToLower(k.name), positional[1])
	return nil
}

// runScale changes the size of a Fleet.
func runScale(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("scale", flag.ContinueOnError)
	conn := clientFlags(fs)
	replicas := fs.Int("replicas", -1, "the new number of game servers")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 || positional[0] != "fleet" || *replicas < 0 {
		return errors.New("usage: agones-lite scale fleet NAME --replicas N")
	}
	body, err := json.Marshal(map[string]int{"replicas": *replicas})
	if err != nil {
		return err
	}
	c, err := conn.admin()
	if err != nil {
		return err
	}
	path := "/v1/namespaces/" + conn.namespace + "/fleets/" + positional[1] + "/scale"
	if _, err := c.call(ctx, http.MethodPut, path, body, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "fleet/%s scaled to %d\n", positional[1], *replicas)
	return nil
}

// runAllocate asks the allocation API for a game server, the way a
// matchmaker would. It is here for trying things out.
func runAllocate(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("allocate", flag.ContinueOnError)
	conn := clientFlags(fs)
	allocator := fs.String("allocator", env("AGONES_LITE_ALLOCATOR", "https://127.0.0.1:9444"), "address of the allocation API (or $AGONES_LITE_ALLOCATOR)")
	fleet := fs.String("fleet", "", "allocate from this Fleet")
	file := fs.String("f", "", "an allocation request as JSON or YAML, instead of --fleet")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	request, err := allocationRequest(conn.namespace, *fleet, *file)
	if err != nil {
		return err
	}
	c, err := conn.client(*allocator, auth.Allocator)
	if err != nil {
		return err
	}
	var allocated struct {
		GameServerName string `json:"gameServerName"`
		Address        string `json:"address"`
		Ports          []struct {
			Port int32 `json:"port"`
		} `json:"ports"`
	}
	if _, err := c.call(ctx, http.MethodPost, "/gameserverallocation", request, &allocated); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s %s", allocated.GameServerName, allocated.Address)
	for _, p := range allocated.Ports {
		_, _ = fmt.Fprintf(out, " %d", p.Port)
	}
	_, _ = fmt.Fprintln(out)
	return nil
}

// allocationRequest builds the JSON body: the file as given, or a request
// for any Ready game server of the Fleet.
func allocationRequest(namespace, fleet, file string) ([]byte, error) {
	if file != "" {
		raw, err := readFile(file)
		if err != nil {
			return nil, err
		}
		return yaml.YAMLToJSON(raw)
	}
	if fleet == "" {
		return nil, errors.New("usage: agones-lite allocate --fleet NAME, or -f FILE")
	}
	return json.Marshal(map[string]any{
		"namespace": namespace,
		"gameServerSelectors": []map[string]any{
			{"matchLabels": map[string]string{"agones.dev/fleet": fleet}},
		},
	})
}
