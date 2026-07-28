package gun

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/sledro/railgun/ethereum"
)

// TransactionSender handles sending batches of transactions with concurrency control
type TransactionSender struct {
	eth         *ethereum.Ethereum
	log         *slog.Logger
	concurrency int
}

// SendResult contains the results of sending batches
type SendResult struct {
	TxHashes []common.Hash
	Failed   int
	Success  int
	// NetworkTime is the sum of per-batch RPC durations. In parallel mode
	// batches overlap, so this exceeds WallTime and must not be used as an
	// elapsed time. Use WallTime for any rate calculation.
	NetworkTime time.Duration
	// WallTime is the wall-clock duration of the whole send phase.
	WallTime time.Duration
}

// NewTransactionSender creates a new transaction sender
func NewTransactionSender(eth *ethereum.Ethereum, log *slog.Logger, concurrency int) *TransactionSender {
	return &TransactionSender{
		eth:         eth,
		log:         log,
		concurrency: concurrency,
	}
}

// SendBatches sends multiple batches of transactions with sequential or parallel mode.
// The batch schedule is anchored to the start of the send phase, so both modes
// pace batches identically regardless of how long earlier phases took.
func (s *TransactionSender) SendBatches(ctx context.Context, batches [][]*types.Transaction, batchDelay int64) *SendResult {
	sendStart := time.Now()

	var result *SendResult
	if s.concurrency == 1 {
		// Sequential mode with delays between completions
		result = s.sendBatchesSequential(ctx, batches, batchDelay, sendStart)
	} else {
		// Parallel mode with scheduled times
		result = s.sendBatchesParallel(ctx, batches, batchDelay, sendStart)
	}

	result.WallTime = time.Since(sendStart)
	return result
}

// sendBatchesSequential sends batches one at a time with scheduled timing
func (s *TransactionSender) sendBatchesSequential(ctx context.Context, batches [][]*types.Transaction, batchDelay int64, startTime time.Time) *SendResult {
	result := &SendResult{
		TxHashes: make([]common.Hash, 0),
	}

	var successCount, failedCount int
	var totalNetworkTime time.Duration

	totalBatches := len(batches)

	for i, batch := range batches {
		if ctx.Err() != nil {
			s.log.Warn("Send cancelled", "sentBatches", i, "totalBatches", totalBatches)
			break
		}

		// Calculate when this batch should be sent
		if batchDelay > 0 && i > 0 {
			targetTime := startTime.Add(time.Duration(int64(i)*batchDelay) * time.Millisecond)
			if sleepDuration := time.Until(targetTime); sleepDuration > 0 {
				if err := ethereum.SleepCtx(ctx, sleepDuration); err != nil {
					s.log.Warn("Send cancelled while pacing", "sentBatches", i, "totalBatches", totalBatches)
					break
				}
			}
		}

		s.log.Info("Sending batch", "batch", i+1, "total", totalBatches, "size", len(batch))

		// Send the batch
		successHashes, success, failed, networkTime := s.sendSingleBatch(ctx, batch)
		successCount += success
		failedCount += failed
		totalNetworkTime += networkTime

		// Add successful hashes
		if len(successHashes) > 0 {
			result.TxHashes = append(result.TxHashes, successHashes...)
		}

		s.log.Info("Batch completed", "batch", i+1, "total", totalBatches, "success", success, "failed", failed)
	}

	result.Success = successCount
	result.Failed = failedCount
	result.NetworkTime = totalNetworkTime

	s.log.Info("All batches sent", "total", totalBatches, "success", result.Success, "failed", result.Failed)

	return result
}

// sendBatchesParallel sends multiple batches of transactions concurrently with scheduled timing
func (s *TransactionSender) sendBatchesParallel(ctx context.Context, batches [][]*types.Transaction, batchDelay int64, startTime time.Time) *SendResult {
	result := &SendResult{
		TxHashes: make([]common.Hash, 0),
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var successCount, failedCount atomic.Int32
	var totalNetworkTime atomic.Int64 // Accumulated network time in nanoseconds

	// Semaphore for concurrency control
	semaphore := make(chan struct{}, s.concurrency)

	totalBatches := len(batches)

	for i, batch := range batches {
		wg.Add(1)
		go func(batchNum int, b []*types.Transaction) {
			defer wg.Done()

			// Wait for this batch's scheduled time BEFORE taking a concurrency
			// slot. Acquiring first would let a batch scheduled far in the
			// future hold a slot while sleeping, starving batches already due.
			if batchDelay > 0 {
				targetTime := startTime.Add(time.Duration(int64(batchNum)*batchDelay) * time.Millisecond)
				if sleepDuration := time.Until(targetTime); sleepDuration > 0 {
					if err := ethereum.SleepCtx(ctx, sleepDuration); err != nil {
						return
					}
				}
			}

			// Acquire semaphore (limit concurrency)
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}

			if ctx.Err() != nil {
				return
			}

			s.log.Info("Sending batch", "batch", batchNum+1, "total", totalBatches, "size", len(b))

			// Send the batch and get back successful hashes
			successHashes, success, failed, networkTime := s.sendSingleBatch(ctx, b)
			successCount.Add(int32(success))
			failedCount.Add(int32(failed))
			totalNetworkTime.Add(networkTime.Nanoseconds())

			// Only add successfully sent transaction hashes
			if len(successHashes) > 0 {
				mu.Lock()
				result.TxHashes = append(result.TxHashes, successHashes...)
				mu.Unlock()
			}

			s.log.Info("Batch completed", "batch", batchNum+1, "total", totalBatches, "success", success, "failed", failed)
		}(i, batch)
	}

	// Wait for all batches to complete
	wg.Wait()

	result.Success = int(successCount.Load())
	result.Failed = int(failedCount.Load())
	result.NetworkTime = time.Duration(totalNetworkTime.Load())

	s.log.Info("All batches sent", "total", totalBatches, "success", result.Success, "failed", result.Failed)

	return result
}

// sendSingleBatch sends a single batch and returns successful hashes, success/failure counts and network time
func (s *TransactionSender) sendSingleBatch(ctx context.Context, batch []*types.Transaction) (successHashes []common.Hash, success int, failed int, networkTime time.Duration) {
	successHashes = make([]common.Hash, 0)

	s.log.Debug("Preparing to send batch via RPC", "size", len(batch))

	// Log transaction details at debug level
	for i, tx := range batch {
		if tx != nil {
			s.log.Debug("Transaction in batch",
				"index", i,
				"hash", tx.Hash().Hex(),
				"nonce", tx.Nonce(),
				"to", tx.To(),
				"value", tx.Value(),
				"gas", tx.Gas(),
			)
		}
	}

	s.log.Debug("Calling eth.SendBatch", "batchSize", len(batch))
	results, netTime, err := s.eth.SendBatch(ctx, batch)
	s.log.Debug("eth.SendBatch returned", "networkTime", netTime, "hasError", err != nil)

	if err != nil {
		s.log.Error("Failed to execute batch RPC call", "attempted_batch_size", len(batch), "error", err, "networkTime", netTime)
		return nil, 0, len(batch), netTime
	}

	if results == nil {
		s.log.Warn("Received no results for batch, likely an empty batch was sent")
		return nil, 0, 0, netTime
	}

	s.log.Debug("Processing batch results", "resultCount", len(results))

	for i, res := range results {
		var originalTxHash common.Hash
		if i < len(batch) && batch[i] != nil {
			originalTxHash = batch[i].Hash()
		}

		if res.Error != nil {
			s.log.Error("Failed to send transaction in batch",
				"index", i,
				"tx_hash", originalTxHash.Hex(),
				"error", res.Error)
			failed++
		} else {
			if txHash, ok := res.Result.(*common.Hash); ok && txHash != nil {
				s.log.Debug("Transaction sent successfully",
					"index", i,
					"tx_hash", originalTxHash.Hex(),
					"result_hash", txHash.Hex())
				successHashes = append(successHashes, originalTxHash)
				success++
			} else {
				s.log.Error("Transaction in batch sent, but result format is unexpected or nil",
					"index", i,
					"tx_hash", originalTxHash.Hex(),
					"result", res.Result,
					"resultType", fmt.Sprintf("%T", res.Result))
				failed++
			}
		}
	}

	s.log.Debug("Batch processing complete", "success", success, "failed", failed)

	return successHashes, success, failed, netTime
}
