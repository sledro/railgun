package railgun

import (
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

// NewRailgun creates a new Railgun instance
func NewRailgun(log *slog.Logger, eth *ethereum.Ethereum, config *Config) *Railgun {
	return &Railgun{
		log:       log,
		eth:       eth,
		config:    config,
		generator: NewTransactionGenerator(config.ChainID, config.Mnemonic, config.PrivateKey, log, eth),
		sender:    NewTransactionSender(eth, log, config.Concurrency),
		reporter:  NewReporter(eth, log),
	}
}

// StartTPS executes the TPS benchmark
func (r *Railgun) StartTPS(txCount int64, batchSize int64, batchDelay int64, accounts int) error {
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
		if err := r.generator.PrefundRandomSendersParallel(accounts, txsPerSender); err != nil {
			r.log.Error("Failed to prefund sender accounts", "error", err)
			return err
		}
		prefundTime := time.Since(prefundStart)
		r.log.Info("Prefunding completed", "duration", prefundTime, "accounts", accounts)
	}

	// Generate all batches
	r.log.Info("Generating transactions...")
	genStart := time.Now()
	batches := make([][]*types.Transaction, 0)
	for i := int64(0); i < txCount; i += batchSize {
		remainingTxs := txCount - i
		currentBatchSize := batchSize
		if remainingTxs < batchSize {
			currentBatchSize = remainingTxs
		}

		batch := r.generator.GenerateBatch(currentBatchSize)
		batches = append(batches, batch)
	}
	genTime := time.Since(genStart)
	r.log.Info("Transactions generated", "batches", len(batches), "totalTxs", txCount)

	// Send all batches
	r.log.Info("Sending batches...")
	sendResult := r.sender.SendBatches(batches, batchDelay, startTime)

	// Generate and print report
	r.log.Info("Generating report...")
	reportData := r.reporter.GenerateReport(sendResult.TxHashes, startTime, genTime, sendResult.NetworkTime)
	r.reporter.PrintReport(reportData)

	return nil
}
