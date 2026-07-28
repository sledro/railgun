package gun

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/sledro/railgun/ethereum"
)

// Config holds configuration for the Railgun benchmark
type Config struct {
	ChainID     int64
	Concurrency int
	Mnemonic    string
	PrivateKey  string
	// GasTipCapWei and GasFeeCapWei set the EIP-1559 fees for every generated
	// transaction. Zero or negative values fall back to the package defaults.
	GasTipCapWei int64
	GasFeeCapWei int64
	// SingleSource restricts prefunding to the first eligible mnemonic account
	// instead of fanning out across every funded account.
	SingleSource bool
	// RPCURL is recorded in saved reports so a run can be identified later.
	RPCURL string
	// ReportDir is where plain-text reports are saved, one file per run, named
	// for the run start time. Empty disables saving.
	ReportDir string
	// Version is recorded in saved reports. Optional.
	Version string
}

// Railgun orchestrates the TPS benchmark
type Railgun struct {
	eth       *ethereum.Ethereum
	log       *slog.Logger
	config    *Config
	generator *TransactionGenerator
	sender    *TransactionSender
	reporter  *Reporter
}

// NewRailgun creates a new Railgun instance.
// It fails if no funded account can be set up, rather than proceeding to a run
// that would submit nothing and report a meaningless result.
func NewRailgun(ctx context.Context, log *slog.Logger, eth *ethereum.Ethereum, config *Config) (*Railgun, error) {
	generator, err := NewTransactionGenerator(ctx, config, log, eth)
	if err != nil {
		return nil, fmt.Errorf("failed to set up funded account: %w", err)
	}

	return &Railgun{
		log:       log,
		eth:       eth,
		config:    config,
		generator: generator,
		sender:    NewTransactionSender(eth, log, config.Concurrency),
		reporter:  NewReporter(eth, log),
	}, nil
}

// StartTPS executes the TPS benchmark
func (r *Railgun) StartTPS(ctx context.Context, txCount int64, batchSize int64, batchDelay int64, accounts int) error {
	startTime := time.Now()
	r.log.Info("Starting TPS benchmark",
		"txCount", txCount,
		"batchSize", batchSize,
		"batchDelay", batchDelay,
		"concurrency", r.config.Concurrency,
		"chainID", r.config.ChainID,
		"accounts", accounts)

	// Prefund random senders if in multi-sender mode
	if accounts > 1 {
		txsPerSender := int(txCount)/accounts + 1
		r.log.Info("Prefunding random sender accounts", "accounts", accounts, "txsPerSender", txsPerSender)

		prefundStart := time.Now()
		if err := r.generator.PrefundRandomSendersParallel(ctx, accounts, txsPerSender); err != nil {
			r.log.Error("Failed to prefund sender accounts", "error", err)
			return err
		}
		prefundTime := time.Since(prefundStart)
		r.log.Info("Prefunding completed", "duration", prefundTime, "accounts", accounts)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("benchmark cancelled before generation: %w", err)
	}

	// Generate all batches
	r.log.Info("Generating transactions...")
	genStart := time.Now()
	batches := make([][]*types.Transaction, 0)
	generated := 0
	for i := int64(0); i < txCount; i += batchSize {
		remainingTxs := txCount - i
		currentBatchSize := batchSize
		if remainingTxs < batchSize {
			currentBatchSize = remainingTxs
		}

		batch := r.generator.GenerateBatch(currentBatchSize)
		generated += len(batch)
		batches = append(batches, batch)
	}
	genTime := time.Since(genStart)

	// Signing failures produce short batches. Sending nothing would otherwise
	// yield a report full of zeroes and a success exit code.
	if generated == 0 {
		return fmt.Errorf("no transactions could be generated for the benchmark")
	}
	if int64(generated) < txCount {
		r.log.Warn("Fewer transactions generated than requested", "requested", txCount, "generated", generated)
	}
	r.log.Info("Transactions generated", "batches", len(batches), "totalTxs", generated)

	// Send all batches
	r.log.Info("Sending batches...")
	sendResult := r.sender.SendBatches(ctx, batches, batchDelay)

	// Generate and print report
	r.log.Info("Generating report...")
	reportData := r.reporter.GenerateReport(ctx, sendResult, startTime, genTime)
	r.reporter.PrintReport(reportData)

	// Saving is best-effort: the report has already been printed, so a write
	// failure must not turn a completed benchmark into a failed command.
	if r.config.ReportDir != "" {
		meta := &RunMetadata{
			StartedAt:    startTime,
			RPCURL:       r.config.RPCURL,
			ChainID:      r.config.ChainID,
			TxCount:      txCount,
			BatchSize:    batchSize,
			BatchDelay:   batchDelay,
			Concurrency:  r.config.Concurrency,
			Accounts:     accounts,
			GasTipCapWei: r.config.GasTipCapWei,
			GasFeeCapWei: r.config.GasFeeCapWei,
			Version:      r.config.Version,
		}

		if path, err := r.reporter.SaveReport(r.config.ReportDir, reportData, meta); err != nil {
			r.log.Error("Failed to save report", "dir", r.config.ReportDir, "error", err)
		} else {
			r.log.Info("Report saved", "path", path)
		}
	}

	// Report first, then surface the interruption. The partial numbers are still
	// worth seeing, but the exit code must not claim a completed run.
	return ctx.Err()
}
