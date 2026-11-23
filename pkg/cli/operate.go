package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

	"github.com/ponyo877/agones-lite/pkg/auth"
)

// runNode takes a node out of service, or back in.
func runNode(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	conn := clientFlags(fs)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: agones-lite node cordon|uncordon|drain|delete NAME")
	}
	operation, name := positional[0], positional[1]
	c, err := conn.admin()
	if err != nil {
		return err
	}
	switch operation {
	case "cordon", "uncordon":
		_, err = c.call(ctx, http.MethodPost, "/v1/nodes/"+name+"/"+operation, nil, nil)
	case "delete":
		_, err = c.call(ctx, http.MethodDelete, "/v1/nodes/"+name, nil, nil)
	case "drain":
		return drain(ctx, c, name, out)
	default:
		return fmt.Errorf("unknown operation %q", operation)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "node/%s %s done\n", name, operation)
	return nil
}

// drain starts a drain and waits for the node to be empty. Game servers
// with a session are left to finish, so this can take as long as a session.
func drain(ctx context.Context, c client, name string, out io.Writer) error {
	if _, err := c.call(ctx, http.MethodPost, "/v1/nodes/"+name+"/drain", nil, nil); err != nil {
		return err
	}
	last := -1
	for {
		left, err := gameServersOn(ctx, c, name)
		if err != nil {
			return err
		}
		if left == 0 {
			_, _ = fmt.Fprintf(out, "node/%s drained\n", name)
			return nil
		}
		if left != last {
			_, _ = fmt.Fprintf(out, "node/%s: waiting for %d game servers to finish\n", name, left)
			last = left
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func gameServersOn(ctx context.Context, c client, node string) (int, error) {
	var list struct {
		Items []agonesv1.GameServer `json:"items"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/v1/gameservers", nil, &list); err != nil {
		return 0, err
	}
	count := 0
	for _, gs := range list.Items {
		if gs.Status.NodeName == node {
			count++
		}
	}
	return count, nil
}

// runToken shows or replaces one of the control plane's tokens. It works on
// the data directory, so it must run on the control plane's machine.
func runToken(_ context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	dataDir := fs.String("data-dir", env("AGONES_LITE_DATA_DIR", defaultDataDir), "data directory of the control plane")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: agones-lite token show|rotate admin|allocator|join")
	}
	role, err := auth.ParseRole(positional[1])
	if err != nil {
		return err
	}
	secrets, err := auth.Load(*dataDir, nil)
	if err != nil {
		return err
	}
	switch positional[0] {
	case "show":
	case "rotate":
		if err := secrets.Rotate(role); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown operation %q", positional[0])
	}
	_, _ = fmt.Fprintln(out, secrets.Token(role))
	return nil
}
