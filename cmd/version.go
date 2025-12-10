package cmd

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

// These variables are set during build time using -ldflags
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// NewVersionCmd creates and returns the version command
func NewVersionCmd() *cli.Command {
	return &cli.Command{
		Name:    "version",
		Aliases: []string{"v"},
		Usage:   "Show version information",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			fmt.Printf("Railgun CLI v%s\n", Version)
			return nil
		},
	}
}
