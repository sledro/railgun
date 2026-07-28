package ethereum

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	gethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// batchCallTimeout bounds a single batch RPC round trip.
const batchCallTimeout = 30 * time.Second

// nonceProbeDelay spaces out nonce probes so a load balancer is likely to route
// them to different backend nodes.
const nonceProbeDelay = 50 * time.Millisecond

type Ethereum struct {
	Client      *ethclient.Client
	cfg         *Config
	isWebSocket bool
}

type Config struct {
	URL string
}

// isWebSocketURL checks if the RPC URL is a WebSocket connection
func isWebSocketURL(url string) bool {
	return strings.HasPrefix(url, "ws://") || strings.HasPrefix(url, "wss://")
}

func NewEthereum(cfg *Config) *Ethereum {
	return &Ethereum{
		cfg:         cfg,
		isWebSocket: isWebSocketURL(cfg.URL),
	}
}

func (e *Ethereum) Connect(ctx context.Context) error {
	ethClient, err := ethclient.DialContext(ctx, e.cfg.URL)
	if err != nil {
		return err
	}
	e.Client = ethClient
	return nil
}

// Close closes the underlying client connection
func (e *Ethereum) Close() {
	if e.Client != nil {
		e.Client.Close()
	}
}

// IsNotFound reports whether err means the node does not have the requested
// block, transaction or receipt. Prefer this over matching on error strings.
func IsNotFound(err error) bool {
	return errors.Is(err, gethereum.NotFound)
}

// GetHighestPendingNonce probes the pending nonce several times and returns the
// highest value seen. Load-balanced RPCs can route each call to a different node
// with a different view of the mempool, and the highest nonce is the only safe
// choice: starting below it would collide with transactions already pending.
//
// Individual probe failures are tolerated; an error is returned only if every
// probe fails. attempts is clamped to at least 1.
func (e *Ethereum) GetHighestPendingNonce(ctx context.Context, address common.Address, attempts int) (uint64, error) {
	if attempts < 1 {
		attempts = 1
	}

	var (
		maxNonce   uint64
		lastErr    error
		anySucceed bool
	)

	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := sleepCtx(ctx, nonceProbeDelay); err != nil {
				break
			}
		}

		nonce, err := e.Client.PendingNonceAt(ctx, address)
		if err != nil {
			lastErr = err
			continue
		}

		anySucceed = true
		if nonce > maxNonce {
			maxNonce = nonce
		}
	}

	if !anySucceed {
		return 0, fmt.Errorf("failed to fetch pending nonce for %s after %d attempt(s): %w", address.Hex(), attempts, lastErr)
	}

	return maxNonce, nil
}

// sleepCtx sleeps for d, returning early with ctx.Err() if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SleepCtx sleeps for d, returning early with ctx.Err() if ctx is cancelled.
// Exported so callers pacing their own work stay interruptible.
func SleepCtx(ctx context.Context, d time.Duration) error {
	return sleepCtx(ctx, d)
}

func (e *Ethereum) GetChainID(ctx context.Context) (*big.Int, error) {
	chainID, err := e.Client.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	return chainID, nil
}

func (e *Ethereum) GetBlockNumber(ctx context.Context) (uint64, error) {
	blockNumber, err := e.Client.BlockNumber(ctx)
	if err != nil {
		return 0, err
	}
	return blockNumber, nil
}

// GetClient returns the underlying ethclient.Client
func (e *Ethereum) GetClient() *ethclient.Client {
	return e.Client
}

// SendBatch sends a batch of transactions in a single RPC call.
// It returns a slice of rpc.BatchElem, where each element corresponds to an input transaction
// and contains either the transaction hash in its Result field or an error in its Error field.
// The second return value is the network time (excluding marshaling).
// The third return value is an error for the overall BatchCall operation itself.
func (e *Ethereum) SendBatch(ctx context.Context, inputBatch []*types.Transaction) ([]rpc.BatchElem, time.Duration, error) {
	if len(inputBatch) == 0 {
		return nil, 0, nil // Nothing to send, no error
	}

	rpcBatch := make([]rpc.BatchElem, len(inputBatch))
	for i, tx := range inputBatch {
		if tx == nil { // Add a nil check for safety
			rpcBatch[i] = rpc.BatchElem{
				Error: fmt.Errorf("transaction at index %d is nil", i),
			}
			continue
		}
		data, err := tx.MarshalBinary()
		if err != nil {
			rpcBatch[i] = rpc.BatchElem{
				Error: fmt.Errorf("failed to marshal tx at index %d: %w", i, err),
			}
			continue
		}

		rpcBatch[i] = rpc.BatchElem{
			Method: "eth_sendRawTransaction",
			Args:   []any{hexutil.Encode(data)},
			Result: new(common.Hash), // Initialize Result to hold a common.Hash
		}
	}

	// Bound the call so a stalled endpoint cannot hang the run indefinitely
	callCtx, cancel := context.WithTimeout(ctx, batchCallTimeout)
	defer cancel()

	// Time ONLY the actual network submission (excluding marshaling)
	networkStart := time.Now()
	err := e.Client.Client().BatchCallContext(callCtx, rpcBatch)
	networkTime := time.Since(networkStart)

	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return rpcBatch, networkTime, fmt.Errorf("batch call timed out after %s (rpc: %s, batch size: %d)", batchCallTimeout, e.cfg.URL, len(inputBatch))
		}
		return rpcBatch, networkTime, fmt.Errorf("batch call failed (rpc: %s, batch size: %d): %w", e.cfg.URL, len(inputBatch), err)
	}

	// The rpcBatch slice is now populated with results or errors for each element.
	return rpcBatch, networkTime, nil
}

// GetBlockByNumber returns a block by its number
func (e *Ethereum) GetBlockByNumber(ctx context.Context, blockNum uint64) (*types.Block, error) {
	return e.Client.BlockByNumber(ctx, new(big.Int).SetUint64(blockNum))
}

// GetBlockHeaderByNumber returns only the block header by its number (without transactions)
// This is useful for forks with custom transaction types
func (e *Ethereum) GetBlockHeaderByNumber(ctx context.Context, blockNum uint64) (*types.Header, error) {
	return e.Client.HeaderByNumber(ctx, new(big.Int).SetUint64(blockNum))
}

// GetBlockTransactionCount returns the number of transactions in a block without decoding them
// This is useful for chains with custom transaction types
func (e *Ethereum) GetBlockTransactionCount(ctx context.Context, blockNum uint64) (int, error) {
	// Get the underlying RPC client
	rpcClient := e.Client.Client()

	// Make raw RPC call to get block with transaction hashes only (not full transactions)
	type blockResult struct {
		Transactions []any `json:"transactions"`
	}

	var result blockResult
	err := rpcClient.CallContext(
		ctx,
		&result,
		"eth_getBlockByNumber",
		fmt.Sprintf("0x%x", blockNum),
		false, // false = only return tx hashes, not full tx objects
	)
	if err != nil {
		return 0, err
	}

	return len(result.Transactions), nil
}

// BatchGetTransactionReceipts fetches multiple transaction receipts in parallel batches
// with adaptive batch sizes based on connection type (WebSocket vs HTTP)
func (e *Ethereum) BatchGetTransactionReceipts(ctx context.Context, txHashes []common.Hash) (map[common.Hash]*types.Receipt, error) {
	if len(txHashes) == 0 {
		return make(map[common.Hash]*types.Receipt), nil
	}

	// Adaptive configuration based on connection type
	var batchSize, concurrency int
	if e.isWebSocket {
		// WebSocket: larger batches, more concurrency (lower latency, persistent connection)
		batchSize = 500
		concurrency = 20
	} else {
		// HTTP: smaller batches, moderate concurrency (avoid overwhelming server)
		batchSize = 200
		concurrency = 10
	}

	receipts := make(map[common.Hash]*types.Receipt)
	var mu sync.Mutex
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)
	errChan := make(chan error, 1)

	// Process batches in parallel
	for i := 0; i < len(txHashes); i += batchSize {
		end := min(i+batchSize, len(txHashes))

		wg.Add(1)
		go func(batch []common.Hash) {
			defer wg.Done()

			// Acquire semaphore slot
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}

			// Prepare batch RPC call
			rpcBatch := make([]rpc.BatchElem, len(batch))
			for j, txHash := range batch {
				rpcBatch[j] = rpc.BatchElem{
					Method: "eth_getTransactionReceipt",
					Args:   []any{txHash.Hex()},
					Result: new(*types.Receipt),
				}
			}

			// Execute batch call
			callCtx, cancel := context.WithTimeout(ctx, batchCallTimeout)
			defer cancel()

			if err := e.Client.Client().BatchCallContext(callCtx, rpcBatch); err != nil {
				select {
				case errChan <- fmt.Errorf("batch receipt call failed (rpc: %s, batch size: %d): %w", e.cfg.URL, len(batch), err):
				default:
				}
				return
			}

			// Collect results
			mu.Lock()
			for j, elem := range rpcBatch {
				if elem.Error != nil {
					// Skip failed receipts, they might not be mined yet
					continue
				}
				if receipt := elem.Result.(**types.Receipt); receipt != nil && *receipt != nil {
					receipts[batch[j]] = *receipt
				}
			}
			mu.Unlock()
		}(txHashes[i:end])
	}

	wg.Wait()
	close(errChan)

	// Check if any errors occurred
	if err := <-errChan; err != nil {
		return receipts, err
	}

	return receipts, nil
}

// IsWebSocket returns true if the connection is using WebSocket
func (e *Ethereum) IsWebSocket() bool {
	return e.isWebSocket
}
