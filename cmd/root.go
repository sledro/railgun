package cmd

import (
	"context"
	"os"

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

// Execute runs the root command
func Execute() {
	cmd := NewRootCmd()
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		logger.DefaultLogger.Error("Command failed", "error", err)
		os.Exit(1)
	}
}
