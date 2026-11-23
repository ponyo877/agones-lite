package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"sigs.k8s.io/yaml"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
)

// runApply creates every resource in a YAML file.
func runApply(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	server, namespace := clientFlags(fs)
	file := fs.String("f", "", "YAML file to apply, or - for standard input")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	raw, err := readFile(*file)
	if err != nil {
		return err
	}
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		if err := applyOne(ctx, client{*server}, *namespace, []byte(doc), out); err != nil {
			return err
		}
	}
	return nil
}

func readFile(name string) ([]byte, error) {
	switch name {
	case "":
		return nil, errors.New("apply needs -f FILE")
	case "-":
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(name)
}

// applyOne sends one YAML document to the API. The document goes as it is:
// the server does the defaulting and the validation.
func applyOne(ctx context.Context, c client, namespace string, doc []byte, out io.Writer) error {
	var head struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(doc, &head); err != nil {
		return err
	}
	if head.Kind != "GameServer" {
		return fmt.Errorf("kind %q is not supported yet", head.Kind)
	}
	if head.Metadata.Namespace != "" {
		namespace = head.Metadata.Namespace
	}
	created := &agonesv1.GameServer{}
	warnings, err := c.call(ctx, http.MethodPost, "/v1/namespaces/"+namespace+"/gameservers", doc, created)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		_, _ = fmt.Fprintln(out, "warning:", warning)
	}
	_, _ = fmt.Fprintf(out, "gameserver/%s created\n", created.Name)
	return nil
}

// runGet lists resources, or shows one in full when a name is given.
func runGet(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	server, namespace := clientFlags(fs)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return errors.New("usage: agones-lite get <gameservers|nodes> [NAME]")
	}
	c := client{*server}
	switch kind := positional[0]; {
	case isGameServer(kind) && len(positional) > 1:
		return showGameServer(ctx, c, *namespace, positional[1], out)
	case isGameServer(kind):
		return listGameServers(ctx, c, *namespace, out)
	case kind == "nodes" || kind == "node":
		return listNodes(ctx, c, out)
	}
	return fmt.Errorf("unknown kind %q", positional[0])
}

func isGameServer(kind string) bool {
	return kind == "gameservers" || kind == "gameserver" || kind == "gs"
}

func showGameServer(ctx context.Context, c client, namespace, name string, out io.Writer) error {
	gs := &agonesv1.GameServer{}
	if _, err := c.call(ctx, http.MethodGet, "/v1/namespaces/"+namespace+"/gameservers/"+name, nil, gs); err != nil {
		return err
	}
	text, err := yaml.Marshal(gs)
	if err != nil {
		return err
	}
	_, err = out.Write(text)
	return err
}

func listGameServers(ctx context.Context, c client, namespace string, out io.Writer) error {
	list := &agonesv1.GameServerList{}
	if _, err := c.call(ctx, http.MethodGet, "/v1/namespaces/"+namespace+"/gameservers", nil, list); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tSTATE\tADDRESS\tPORT\tNODE\tAGE")
	for _, gs := range list.Items {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			gs.Name, gs.Status.State, gs.Status.Address, firstPort(gs), gs.Status.NodeName, age(gs.CreationTimestamp.Time))
	}
	return table.Flush()
}

func firstPort(gs agonesv1.GameServer) string {
	if len(gs.Status.Ports) == 0 {
		return ""
	}
	return fmt.Sprint(gs.Status.Ports[0].Port)
}

func listNodes(ctx context.Context, c client, out io.Writer) error {
	var list struct {
		Items []litev1.Node `json:"items"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/v1/nodes", nil, &list); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tPHASE\tADDRESS\tPORTS\tAGE")
	for _, n := range list.Items {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%d-%d\t%s\n",
			n.Name, n.Status.Phase, n.Status.Address, n.Status.PortRange.Min, n.Status.PortRange.Max, age(n.CreationTimestamp.Time))
	}
	return table.Flush()
}

// age formats how long ago t was, the way kubectl does: 12s, 5m, 3h, 2d.
func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// runDelete asks for a GameServer to be removed.
func runDelete(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	server, namespace := clientFlags(fs)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 || !isGameServer(positional[0]) {
		return errors.New("usage: agones-lite delete gameserver NAME")
	}
	path := "/v1/namespaces/" + *namespace + "/gameservers/" + positional[1]
	if _, err := (client{*server}).call(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "gameserver/%s deleted\n", positional[1])
	return nil
}
