package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/sledro/railgun/logger"
	"github.com/urfave/cli/v3"
)

// NewRootCmd creates and returns the root command
func NewRootCmd() *cli.Command {
	return &cli.Command{
		Name:  "railgun",
		Usage: "An EVM benchmarking toolbox 🧰",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "loglevel",
				Aliases: []string{"l"},
				Usage:   "Log level (debug, info, warn, error)",
				Value:   "info",
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			// Set log level from flag
			if err := logger.SetLevel(cmd.String("loglevel")); err != nil {
				return ctx, err
			}
			logger.DefaultLogger.Info("Starting Railgun", "version", Version, "commit", Commit, "date", Date)
			return ctx, nil
		},
		Commands: []*cli.Command{
			NewVersionCmd(),
			NewTPSCmd(),
		},
	}
}

// Execute runs the root command.
//
// The context is cancelled on SIGINT/SIGTERM so a long benchmark can be
// interrupted cleanly: in-flight polling returns what it has and a partial
// report is still printed, rather than the process dying mid-run.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := NewRootCmd()
	if err := cmd.Run(ctx, os.Args); err != nil {
		// A cancelled run is an expected outcome of Ctrl-C, not a failure to report.
		if errors.Is(err, context.Canceled) {
			logger.DefaultLogger.Warn("Interrupted")
			os.Exit(130)
		}
		logger.DefaultLogger.Error("Command failed", "error", err)
		os.Exit(1)
	}
}
