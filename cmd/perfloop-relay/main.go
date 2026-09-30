// Command perfloop-relay runs inside a customer network. It opens tunnels to
// the Perfloop API, answers validated telemetry reads from the configured
// upstreams, and collects and stores nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/perfloop/relay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "perfloop-relay: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	return runArgs(os.Args[1:], os.Stdout)
}

// runArgs is main without the process: args are the command-line flags and
// stdout is where -print-routes writes. A failed config exits non-zero with
// the validation error.
func runArgs(args []string, stdout io.Writer) error {
	var configPath string
	var printRoutes bool
	fs := flag.NewFlagSet("perfloop-relay", flag.ContinueOnError)
	fs.StringVar(&configPath, "config", "/etc/perfloop-relay/relay.yaml", "relay YAML config")
	fs.BoolVar(&printRoutes, "print-routes", false, "validate the config, print every request the relay would forward per upstream, and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := relay.Load(configPath)
	if err != nil {
		return err
	}
	if printRoutes {
		if err := relay.Routes(stdout, cfg); err != nil {
			return fmt.Errorf("relay config: %w", err)
		}
		return nil
	}
	// New validates the configuration, the log level included; an
	// unparsable level is its error, not a default.
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.Level()}))
	r, err := relay.New(cfg, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The first signal closes every tunnel at once and Run returns; there is
	// no drain, and a read in flight fails so Perfloop retries it elsewhere.
	// Stopping the notifier here restores the default signal action, so a
	// second signal ends the process even if Run has not returned yet.
	context.AfterFunc(ctx, stop)
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
