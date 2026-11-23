// Package cli is the agones-lite command line: one binary that is the
// control plane ("server") and the tool to talk to it ("apply", "get",
// "delete").
//
// Each command is one function in its own file. They share nothing but the
// small HTTP client in client.go.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// command runs one subcommand with its arguments, writing results to out.
type command func(ctx context.Context, args []string, out io.Writer) error

var commands = map[string]command{
	"server": runServer,
	"apply":  runApply,
	"get":    runGet,
	"delete": runDelete,
}

// Main runs the command named by args[0].
func Main(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return usage()
	}
	run, ok := commands[args[0]]
	if !ok {
		return fmt.Errorf("unknown command %q\n%w", args[0], usage())
	}
	return run(ctx, args[1:], out)
}

func usage() error {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("usage: agones-lite <%s> [arguments]", strings.Join(names, "|"))
}

// parse reads flags that may come before or after the positional arguments,
// as in "get gameservers -n prod", and returns the positional ones.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return positional, nil
}
