package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/ponyo877/scrim/pkg/fleetautoscalers"
)

// runBuffer answers what a Buffer policy leaves to whoever writes it: how
// many? It asks no control plane: it is arithmetic, and
// fleetautoscalers.BufferFor says where it comes from.
func runBuffer(_ context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("buffer", flag.ContinueOnError)
	rate := fs.Float64("rate", 0, "allocations a second, at the busiest")
	startup := fs.Duration("startup", 0, "how long a game server takes from being created to being Ready")
	sync := fs.Duration("sync", 30*time.Second, "how often the FleetAutoscaler looks (spec.sync.fixedInterval.seconds)")
	miss := fs.Float64("miss", 0.01, "the share of looks after which a request may find nothing Ready")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *rate <= 0 || *startup < 0 || *sync <= 0 || *miss <= 0 || *miss >= 1 {
		return errors.New("usage: scrim buffer --rate PER_SECOND --startup DURATION [--sync DURATION] [--miss SHARE]")
	}
	size := fleetautoscalers.BufferFor(*rate, *startup, *sync, *miss)
	_, err := fmt.Fprintf(out, `bufferSize: %d

At %g allocations a second, %.0f are asked for in the %s between two looks of
the autoscaler and the %s a game server takes to be Ready. What is Ready or
on its way at one look has to do for that long. With %d, a request finds
nothing Ready after at most %g of every 100 looks.
`, size, *rate, *rate*(*startup+*sync).Seconds(), *sync, *startup, size, *miss*100)
	return err
}
