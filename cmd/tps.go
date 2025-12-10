package cmd

import (
	"context"
	"fmt"

	"github.com/sledro/railgun/ethereum"
	"github.com/sledro/railgun/logger"
	"github.com/sledro/railgun/railgun"
	"github.com/urfave/cli/v3"
)

// NewTPSCmd creates and returns the TPS command
func NewTPSCmd() *cli.Command {
	return &cli.Command{
		Name:  "tps",
		Usage: "Measure transactions per second",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "rpc",
				Aliases:  []string{"r"},
				Usage:    "Ethereum RPC URL",
				Required: true,
			},
			&cli.IntFlag{
				Name:    "txCount",
				Aliases: []string{"t"},
				Usage:   "Number of transactions to send",
				Value:   50,
			},
			&cli.IntFlag{
				Name:    "batchSize",
				Aliases: []string{"b"},
				Usage:   "Number of transactions to send in each batch",
				Value:   10,
			},
			&cli.IntFlag{
				Name:    "batchDelay",
				Aliases: []string{"d"},
				Usage:   "Delay between batches in milliseconds",
				Value:   10,
			},
			&cli.IntFlag{
				Name:    "concurrent",
				Aliases: []string{"c"},
				Usage:   "Maximum number of concurrent batch sends (use >1 for parallel mode)",
				Value:   1,
			},
			&cli.IntFlag{
				Name:    "accounts",
				Aliases: []string{"a"},
				Usage:   "Number of random sender accounts to use (default: 1 for single-account mode)",
				Value:   1,
			},
			&cli.StringFlag{
				Name:    "mnemonic",
				Aliases: []string{"m"},
				Usage:   "Optional: BIP39 mnemonic phrase for funded wallet (default: test mnemonic)",
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "privatekey",
				Aliases: []string{"k"},
				Usage:   "Optional: Private key hex for funded account (takes precedence over mnemonic)",
				Value:   "",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Get all flags
			rpcURL := cmd.String("rpc")
			txCount := int64(cmd.Int("txCount"))
			batchSize := int64(cmd.Int("batchSize"))
			batchDelay := int64(cmd.Int("batchDelay"))
			concurrency := cmd.Int("concurrent")
			accounts := cmd.Int("accounts")
			mnemonic := cmd.String("mnemonic")
			privateKey := cmd.String("privatekey")

			// Validate inputs
			if txCount <= 0 {
				return fmt.Errorf("txCount must be greater than 0")
			}
			if batchSize <= 0 {
				return fmt.Errorf("batchSize must be greater than 0")
			}
			if batchDelay < 0 {
				return fmt.Errorf("batchDelay cannot be negative")
			}
			if concurrency < 1 || concurrency > 50 {
				return fmt.Errorf("concurrency must be between 1 and 50")
			}
			if accounts < 1 {
				return fmt.Errorf("accounts must be at least 1")
			}

			log := logger.New(nil)

			// Create Ethereum client
			eth := ethereum.NewEthereum(&ethereum.Config{URL: rpcURL})
			if err := eth.Connect(); err != nil {
				return fmt.Errorf("failed to connect to %s: %w", rpcURL, err)
			}
			defer eth.Close()

			// Determine connection type
			connType := "HTTP/HTTPS"
			if eth.IsWebSocket() {
				connType = "WebSocket"
			}

			// Fetch chain ID from RPC
			chainID, err := eth.GetChainID()
			if err != nil {
				return fmt.Errorf("failed to fetch chain ID from RPC: %w", err)
			}
			log.Info("Connected to chain", "chainID", chainID, "rpcURL", rpcURL, "connectionType", connType)

			// Create Railgun
			r := railgun.NewRailgun(log, eth, &railgun.Config{
				ChainID:     chainID.Int64(),
				Concurrency: int(concurrency),
				Mnemonic:    mnemonic,
				PrivateKey:  privateKey,
			})

			// Start TPS benchmark
			if err := r.StartTPS(txCount, batchSize, batchDelay, accounts); err != nil {
				return fmt.Errorf("TPS benchmark failed: %w", err)
			}

			return nil
		},
	}
}
