package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"sigs.k8s.io/yaml"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/apiserver"
	"github.com/ponyo877/agones-lite/pkg/events"
)

// runGet lists resources, or shows one in full when a name is given.
func runGet(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	conn := clientFlags(fs)
	watch := fs.Bool("w", false, "after the list, keep printing every change")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return errors.New("usage: agones-lite get KIND [NAME] [-w]")
	}
	c, err := conn.admin()
	if err != nil {
		return err
	}
	if positional[0] == "nodes" || positional[0] == "node" {
		return listNodes(ctx, c, out)
	}
	k, err := findKind(positional[0])
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return show(ctx, c, k.path(conn.namespace, positional[1]), out)
	}
	return k.list(ctx, c, conn.namespace, *watch, out)
}

// runDescribe shows one resource in full, followed by its history.
func runDescribe(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("describe", flag.ContinueOnError)
	conn := clientFlags(fs)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: agones-lite describe KIND NAME")
	}
	k, err := findKind(positional[0])
	if err != nil {
		return err
	}
	c, err := conn.admin()
	if err != nil {
		return err
	}
	if err := show(ctx, c, k.path(conn.namespace, positional[1]), out); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "\nEvents (newest first):")
	query := url.Values{"kind": {k.name}, "name": {positional[1]}}
	row := func(e events.Event) string {
		return fmt.Sprintf("%s\t%s\t%s\t%s", age(e.Time), e.Type, e.Reason, e.Message)
	}
	return table(ctx, c, "/v1/namespaces/"+conn.namespace+"/events?"+query.Encode(), "AGE\tTYPE\tREASON\tMESSAGE", row, false, out)
}

// show prints one object as YAML, the way it would be written in a manifest.
func show(ctx context.Context, c client, path string, out io.Writer) error {
	var obj any
	if _, err := c.call(ctx, http.MethodGet, path, nil, &obj); err != nil {
		return err
	}
	text, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = out.Write(text)
	return err
}

// table fetches a list and prints one row per item under header.
func table[T any](ctx context.Context, c client, path, header string, row func(T) string, watch bool, out io.Writer) error {
	if watch {
		return follow(ctx, c, path, header, row, out)
	}
	var list struct {
		Items []T `json:"items"`
	}
	if _, err := c.call(ctx, http.MethodGet, path, nil, &list); err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, header)
	for _, item := range list.Items {
		_, _ = fmt.Fprintln(w, row(item))
	}
	return w.Flush()
}

// follow prints a table that keeps growing: everything there is, then a
// line for every change. The first column says what happened.
func follow[T any](ctx context.Context, c client, path, header string, row func(T) string, out io.Writer) error {
	table := &columns{}
	table.measure("Modified") // the longest word the first column will hold
	rows := []string{"EVENT\t" + header}
	synced := false
	return c.events(ctx, path+"?watch=true", func(event string, data []byte) error {
		if event == apiserver.Synced {
			synced = true
			return table.print(out, rows...)
		}
		var item T
		if err := json.Unmarshal(data, &item); err != nil {
			return err
		}
		line := event + "\t" + row(item)
		if !synced {
			rows = append(rows, line) // still the list: wait, so it lines up
			return nil
		}
		return table.print(out, line)
	})
}

// columns lines rows up that are printed one at a time: a column is as
// wide as the widest cell it has held so far. Cells are separated by tabs.
type columns struct {
	widths []int
}

func (c *columns) measure(row string) {
	for i, cell := range strings.Split(row, "\t") {
		if i == len(c.widths) {
			c.widths = append(c.widths, 0)
		}
		c.widths[i] = max(c.widths[i], len(cell))
	}
}

// print writes the rows, after making room for their widest cells.
func (c *columns) print(out io.Writer, rows ...string) error {
	for _, row := range rows {
		c.measure(row)
	}
	for _, row := range rows {
		var line strings.Builder
		for i, cell := range strings.Split(row, "\t") {
			fmt.Fprintf(&line, "%-*s", c.widths[i]+3, cell)
		}
		if _, err := fmt.Fprintln(out, strings.TrimRight(line.String(), " ")); err != nil {
			return err
		}
	}
	return nil
}

func listGameServers(ctx context.Context, c client, namespace string, watch bool, out io.Writer) error {
	row := func(gs agonesv1.GameServer) string {
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s",
			gs.Name, gs.Status.State, gs.Status.Address, firstPort(gs), gs.Status.NodeName, age(gs.CreationTimestamp.Time))
	}
	return table(ctx, c, collection(namespace, "gameservers"), "NAME\tSTATE\tADDRESS\tPORT\tNODE\tAGE", row, watch, out)
}

func firstPort(gs agonesv1.GameServer) string {
	if len(gs.Status.Ports) == 0 {
		return ""
	}
	return fmt.Sprint(gs.Status.Ports[0].Port)
}

const sizeColumns = "NAME\tSCHEDULING\tDESIRED\tCURRENT\tALLOCATED\tREADY\tAGE"

func listGameServerSets(ctx context.Context, c client, namespace string, watch bool, out io.Writer) error {
	row := func(set agonesv1.GameServerSet) string {
		return fmt.Sprintf("%s\t%s\t%d\t%d\t%d\t%d\t%s", set.Name, set.Spec.Scheduling, set.Spec.Replicas,
			set.Status.Replicas, set.Status.AllocatedReplicas, set.Status.ReadyReplicas, age(set.CreationTimestamp.Time))
	}
	return table(ctx, c, collection(namespace, "gameserversets"), sizeColumns, row, watch, out)
}

func listFleets(ctx context.Context, c client, namespace string, watch bool, out io.Writer) error {
	row := func(fleet agonesv1.Fleet) string {
		return fmt.Sprintf("%s\t%s\t%d\t%d\t%d\t%d\t%s", fleet.Name, fleet.Spec.Scheduling, fleet.Spec.Replicas,
			fleet.Status.Replicas, fleet.Status.AllocatedReplicas, fleet.Status.ReadyReplicas, age(fleet.CreationTimestamp.Time))
	}
	return table(ctx, c, collection(namespace, "fleets"), sizeColumns, row, watch, out)
}

func listFleetAutoscalers(ctx context.Context, c client, namespace string, watch bool, out io.Writer) error {
	row := func(fas autoscalingv1.FleetAutoscaler) string {
		return fmt.Sprintf("%s\t%s\t%s\t%d\t%d\t%t\t%s", fas.Name, fas.Spec.FleetName, fas.Spec.Policy.Type,
			fas.Status.CurrentReplicas, fas.Status.DesiredReplicas, fas.Status.ScalingLimited, age(fas.CreationTimestamp.Time))
	}
	return table(ctx, c, collection(namespace, "fleetautoscalers"), "NAME\tFLEET\tPOLICY\tCURRENT\tDESIRED\tLIMITED\tAGE", row, watch, out)
}

func listNodes(ctx context.Context, c client, out io.Writer) error {
	row := func(n litev1.Node) string {
		return fmt.Sprintf("%s\t%s\t%s\t%d-%d\t%s",
			n.Name, nodeStatus(n), n.Status.Address, n.Status.PortRange.Min, n.Status.PortRange.Max, age(n.CreationTimestamp.Time))
	}
	return table(ctx, c, "/v1/nodes", "NAME\tSTATUS\tADDRESS\tPORTS\tAGE", row, false, out)
}

// nodeStatus shows whether a node is connected and whether it takes new
// game servers, as "kubectl get nodes" does.
func nodeStatus(n litev1.Node) string {
	status := string(n.Status.Phase)
	switch {
	case n.Spec.Drain:
		status += ",Draining"
	case n.Spec.Unschedulable:
		status += ",Cordoned"
	}
	return status
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

// runLogs prints what a game server wrote to its standard output and error.
func runLogs(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	conn := clientFlags(fs)
	container := fs.String("c", "", "container to read; the game server's main container by default")
	follow := fs.Bool("f", false, "keep printing new output")
	tail := fs.Int("tail", 0, "only the last lines; 0 means everything")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: agones-lite logs GAMESERVER [-c CONTAINER] [-f] [--tail N]")
	}
	c, err := conn.admin()
	if err != nil {
		return err
	}
	query := url.Values{"container": {*container}, "follow": {fmt.Sprint(*follow)}, "tail": {fmt.Sprint(*tail)}}
	return c.stream(ctx, "/v1/namespaces/"+conn.namespace+"/gameservers/"+positional[0]+"/logs?"+query.Encode(), out)
}
