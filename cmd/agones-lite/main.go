// Command agones-lite is the control plane and its command line client.
// See pkg/cli for the commands.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // schedules of FleetAutoscalers name time zones

	"github.com/ponyo877/agones-lite/pkg/cli"
)

func main() {
	os.Exit(run())
}

// run is main, with a way out that lets what was deferred happen first.
func run() (exitCode int) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := cli.Main(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
