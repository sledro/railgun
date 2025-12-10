package ethereum

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

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

func (e *Ethereum) Connect() error {
	ethClient, err := ethclient.Dial(e.cfg.URL)
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

func (e *Ethereum) GetNonce(address common.Address) (uint64, error) {
	nonce, err := e.Client.PendingNonceAt(context.Background(), address)
	if err != nil {
		return 0, err
	}
	return nonce, nil
}

func (e *Ethereum) GetGasPrice() (*big.Int, error) {
	gasPrice, err := e.Client.SuggestGasPrice(context.Background())
	if err != nil {
		return nil, err
	}
	return gasPrice, nil
}

func (e *Ethereum) GetChainID() (*big.Int, error) {
	chainID, err := e.Client.ChainID(context.Background())
	if err != nil {
		return nil, err
	}
	return chainID, nil
}

func (e *Ethereum) GetBlockNumber() (uint64, error) {
	blockNumber, err := e.Client.BlockNumber(context.Background())
	if err != nil {
		return 0, err
	}
	return blockNumber, nil
}

// GetClient returns the underlying ethclient.Client
func (e *Ethereum) GetClient() *ethclient.Client {
	return e.Client
}

// WaitMined waits for a transaction to be mined
func (e *Ethereum) WaitMined(ctx context.Context, tx *types.Transaction) (*types.Receipt, error) {
	return bind.WaitMined(ctx, e.Client, tx)
}

// SendBatch sends a batch of transactions in a single RPC call.
// It returns a slice of rpc.BatchElem, where each element corresponds to an input transaction
// and contains either the transaction hash in its Result field or an error in its Error field.
// The second return value is the network time (excluding marshaling).
// The third return value is an error for the overall BatchCall operation itself.
func (e *Ethereum) SendBatch(inputBatch []*types.Transaction) ([]rpc.BatchElem, time.Duration, error) {
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

	// Use a context with timeout to prevent hanging indefinitely
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Time ONLY the actual network submission (excluding marshaling)
	networkStart := time.Now()
	err := e.Client.Client().BatchCallContext(ctx, rpcBatch)
	networkTime := time.Since(networkStart)

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return rpcBatch, networkTime, fmt.Errorf("batch call timed out after 30s (rpc: %s, batch size: %d)", e.cfg.URL, len(inputBatch))
		}
		return rpcBatch, networkTime, fmt.Errorf("batch call failed (rpc: %s, batch size: %d): %v", e.cfg.URL, len(inputBatch), err)
	}

	// The rpcBatch slice is now populated with results or errors for each element.
	return rpcBatch, networkTime, nil
}

// SendTransaction sends a single transaction
func (e *Ethereum) SendTransaction(tx *types.Transaction) error {
	return e.Client.SendTransaction(context.Background(), tx)
}

// GetLatestBlockNumber returns the latest block number
func (e *Ethereum) GetLatestBlockNumber() (uint64, error) {
	return e.Client.BlockNumber(context.Background())
}

// GetBlockByNumber returns a block by its number
func (e *Ethereum) GetBlockByNumber(blockNum uint64) (*types.Block, error) {
	return e.Client.BlockByNumber(context.Background(), big.NewInt(int64(blockNum)))
}

// GetBlockHeaderByNumber returns only the block header by its number (without transactions)
// This is useful for forks with custom transaction types
func (e *Ethereum) GetBlockHeaderByNumber(blockNum uint64) (*types.Header, error) {
	return e.Client.HeaderByNumber(context.Background(), big.NewInt(int64(blockNum)))
}

// GetBlockTransactionCount returns the number of transactions in a block without decoding them
// This is useful for chains with custom transaction types
func (e *Ethereum) GetBlockTransactionCount(blockNum uint64) (int, error) {
	// Get the underlying RPC client
	rpcClient := e.Client.Client()

	// Make raw RPC call to get block with transaction hashes only (not full transactions)
	type blockResult struct {
		Transactions []interface{} `json:"transactions"`
	}

	var result blockResult
	err := rpcClient.CallContext(
		context.Background(),
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

// GetTransactionReceipt returns the receipt for a transaction
func (e *Ethereum) GetTransactionReceipt(txHash common.Hash) (*types.Receipt, error) {
	return e.Client.TransactionReceipt(context.Background(), txHash)
}

// BatchGetTransactionReceipts fetches multiple transaction receipts in parallel batches
// with adaptive batch sizes based on connection type (WebSocket vs HTTP)
func (e *Ethereum) BatchGetTransactionReceipts(txHashes []common.Hash) (map[common.Hash]*types.Receipt, error) {
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
		end := i + batchSize
		if end > len(txHashes) {
			end = len(txHashes)
		}

		wg.Add(1)
		go func(batch []common.Hash) {
			defer wg.Done()

			// Acquire semaphore slot
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

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
			err := e.Client.Client().BatchCall(rpcBatch)
			if err != nil {
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
