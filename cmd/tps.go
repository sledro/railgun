package cmd

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/sledro/railgun/ethereum"
	"github.com/sledro/railgun/gun"
	"github.com/sledro/railgun/logger"
	"github.com/urfave/cli/v3"
)

// maxSenderAccounts bounds --accounts. Each account costs a generated key, a
// prefunding transaction and a slot in the funding batch, so an unbounded value
// turns a typo into a very expensive run.
const maxSenderAccounts = 10_000

// defaultReportDir is where per-run reports are written unless overridden.
const defaultReportDir = "reports"

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
				Usage:   "Optional: BIP39 mnemonic phrase for funded wallet (default: test mnemonic). Prefer $RAILGUN_MNEMONIC",
				Sources: cli.EnvVars("RAILGUN_MNEMONIC"),
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "privatekey",
				Aliases: []string{"k"},
				Usage:   "Optional: Private key hex for funded account (takes precedence over mnemonic). Prefer $RAILGUN_PRIVATE_KEY",
				Sources: cli.EnvVars("RAILGUN_PRIVATE_KEY"),
				Value:   "",
			},
			&cli.FloatFlag{
				Name:  "gasTipCap",
				Usage: "Max priority fee per gas, in gwei",
				Value: float64(gun.DefaultGasTipCapWei) / 1e9,
			},
			&cli.FloatFlag{
				Name:  "gasFeeCap",
				Usage: "Max fee per gas, in gwei. Must be >= gasTipCap and above the chain's base fee",
				Value: float64(gun.DefaultGasFeeCapWei) / 1e9,
			},
			&cli.BoolFlag{
				Name:  "singleSource",
				Usage: "Prefund from only the first eligible mnemonic account instead of every funded account",
			},
			&cli.StringFlag{
				Name:  "reportDir",
				Usage: "Directory for saved plain-text reports, one file per run named for the start time",
				Value: defaultReportDir,
			},
			&cli.BoolFlag{
				Name:  "noReport",
				Usage: "Do not save a report file (the report is still printed to stdout)",
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
			gasTipCapGwei := cmd.Float("gasTipCap")
			gasFeeCapGwei := cmd.Float("gasFeeCap")
			singleSource := cmd.Bool("singleSource")
			reportDir := cmd.String("reportDir")
			if cmd.Bool("noReport") {
				reportDir = ""
			}

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
			if accounts < 1 || accounts > maxSenderAccounts {
				return fmt.Errorf("accounts must be between 1 and %d", maxSenderAccounts)
			}
			if gasTipCapGwei <= 0 {
				return fmt.Errorf("gasTipCap must be greater than 0")
			}
			if gasFeeCapGwei < gasTipCapGwei {
				return fmt.Errorf("gasFeeCap (%g gwei) must be greater than or equal to gasTipCap (%g gwei)", gasFeeCapGwei, gasTipCapGwei)
			}

			gasTipCapWei := int64(math.Round(gasTipCapGwei * 1e9))
			gasFeeCapWei := int64(math.Round(gasFeeCapGwei * 1e9))

			log := logger.New(nil)

			// Create Ethereum client
			eth := ethereum.NewEthereum(&ethereum.Config{URL: rpcURL})
			if err := eth.Connect(ctx); err != nil {
				return fmt.Errorf("failed to connect to %s: %w", rpcURL, err)
			}
			defer eth.Close()

			// Determine connection type
			connType := "HTTP/HTTPS"
			if eth.IsWebSocket() {
				connType = "WebSocket"
			}

			// Fetch chain ID from RPC
			chainID, err := eth.GetChainID(ctx)
			if err != nil {
				return fmt.Errorf("failed to fetch chain ID from RPC: %w", err)
			}
			log.Info("Connected to chain", "chainID", chainID, "rpcURL", rpcURL, "connectionType", connType)

			// Create Railgun
			r, err := gun.NewRailgun(ctx, log, eth, &gun.Config{
				ChainID:      chainID.Int64(),
				Concurrency:  int(concurrency),
				Mnemonic:     mnemonic,
				PrivateKey:   privateKey,
				GasTipCapWei: gasTipCapWei,
				GasFeeCapWei: gasFeeCapWei,
				SingleSource: singleSource,
				RPCURL:       rpcURL,
				ReportDir:    reportDir,
				Version:      Version,
			})
			if err != nil {
				return fmt.Errorf("failed to initialise benchmark: %w", err)
			}

			// Start TPS benchmark
			if err := r.StartTPS(ctx, txCount, batchSize, batchDelay, accounts); err != nil {
				// An interrupted run is not a failure; pass it through so the
				// caller can exit 130 without a misleading message.
				if errors.Is(err, context.Canceled) {
					return err
				}
				return fmt.Errorf("TPS benchmark failed: %w", err)
			}

			return nil
		},
	}
}
