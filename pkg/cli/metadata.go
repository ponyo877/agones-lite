package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// runLabel changes the labels of a GameServer, as "kubectl label" does:
// KEY=VALUE sets a key, KEY- removes it, and the other keys stay.
func runLabel(ctx context.Context, args []string, out io.Writer) error {
	return changeMetadata(ctx, metadataChange{verb: "label", field: "labels", done: "labeled"}, args, out)
}

// runAnnotate changes the annotations of a GameServer, as "kubectl
// annotate" does.
func runAnnotate(ctx context.Context, args []string, out io.Writer) error {
	return changeMetadata(ctx, metadataChange{verb: "annotate", field: "annotations", done: "annotated"}, args, out)
}

type metadataChange struct {
	verb  string // the command
	field string // what of the metadata it changes
	done  string // what it says when it has
}

func changeMetadata(ctx context.Context, m metadataChange, args []string, out io.Writer) error {
	fs := flag.NewFlagSet(m.verb, flag.ContinueOnError)
	conn := clientFlags(fs)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 3 {
		return fmt.Errorf("usage: scrim %s gameserver NAME KEY=VALUE or KEY- (one or more)", m.verb)
	}
	k, err := findKind(positional[0])
	if err != nil {
		return err
	}
	if k.name != "GameServer" {
		return fmt.Errorf("only a GameServer is changed this way: apply the manifest of a %s again", k.name)
	}
	body, err := metadataPatch(m.field, positional[2:])
	if err != nil {
		return err
	}
	c, err := conn.admin()
	if err != nil {
		return err
	}
	if _, err := c.call(ctx, http.MethodPatch, k.path(conn.namespace, positional[1]), body, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "gameserver/%s %s\n", positional[1], m.done)
	return nil
}

// metadataPatch turns KEY=VALUE and KEY- into the JSON merge patch the API
// takes: a key with a value sets it, a key with null removes it.
func metadataPatch(field string, words []string) ([]byte, error) {
	changes := map[string]*string{}
	for _, word := range words {
		if key, value, ok := strings.Cut(word, "="); ok {
			changes[key] = &value
			continue
		}
		if key, ok := strings.CutSuffix(word, "-"); ok && key != "" {
			changes[key] = nil
			continue
		}
		return nil, fmt.Errorf("%q is neither KEY=VALUE nor KEY-", word)
	}
	return json.Marshal(map[string]any{"metadata": map[string]any{field: changes}})
}
