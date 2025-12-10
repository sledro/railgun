package railgun

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	hdwallet "github.com/miguelmota/go-ethereum-hdwallet"
	"github.com/sledro/railgun/ethereum"
)

// DefaultMnemonic is the default test mnemonic (Foundry/Anvil default)
const DefaultMnemonic = "test test test test test test test test test test test junk"

// Gas and fee constants
const (
	GasLimitETHTransfer  = 21000                  // Standard ETH transfer gas limit
	GasTipCapGwei        = 1_000_000_000          // 1 gwei
	GasFeeCapGwei        = 20_000_000_000         // 20 gwei
	MinBalanceForFunding = 67_000_000_000_000_000 // 0.067 ETH minimum for parallel funding
	TxValueWei           = 1000                   // Value sent per transaction in wei
)

// TransactionGenerator handles the creation of transactions for benchmarking
type TransactionGenerator struct {
	chainID       *big.Int
	mnemonic      string
	privateKeyHex string
	log           *slog.Logger
	eth           *ethereum.Ethereum
	wallet        *hdwallet.Wallet
	fundedAccount accounts.Account
	fundedKey     *ecdsa.PrivateKey
	nonceMutex    sync.Mutex
	currentNonce  uint64

	// Multi-account support
	allFundedAccounts []fundedAccountInfo // All HD wallet accounts with balance
	senderKeys        []*ecdsa.PrivateKey // Pre-funded random accounts
	senderNonces      []uint64            // Nonce tracking per sender
	senderMutexes     []sync.Mutex        // Lock per sender
	currentSenderIdx  atomic.Uint64       // Round-robin index
}

// fundedAccountInfo stores information about a funded HD wallet account
type fundedAccountInfo struct {
	account accounts.Account
	key     *ecdsa.PrivateKey
	address common.Address
	nonce   uint64
}

// NewTransactionGenerator creates a new transaction generator
func NewTransactionGenerator(chainID int64, mnemonic string, privateKeyHex string, log *slog.Logger, eth *ethereum.Ethereum) *TransactionGenerator {
	// Use default mnemonic if neither mnemonic nor private key provided
	if mnemonic == "" && privateKeyHex == "" {
		mnemonic = DefaultMnemonic
	}

	g := &TransactionGenerator{
		chainID:       big.NewInt(chainID),
		mnemonic:      mnemonic,
		privateKeyHex: privateKeyHex,
		log:           log,
		eth:           eth,
	}

	// Set up the funded account
	if err := g.setupFundedAccount(); err != nil {
		log.Error("Failed to setup funded account", "error", err)
	}

	return g
}

// setupFundedAccount sets up the funded account from private key or mnemonic
func (g *TransactionGenerator) setupFundedAccount() error {
	// If private key is provided, use it directly
	if g.privateKeyHex != "" {
		return g.setupFromPrivateKey()
	}

	// Otherwise use mnemonic (either provided or default)
	return g.setupFromMnemonic()
}

// setupFromPrivateKey sets up a single funded account from a private key
func (g *TransactionGenerator) setupFromPrivateKey() error {
	// Strip 0x prefix if present
	keyHex := g.privateKeyHex
	if len(keyHex) > 2 && keyHex[:2] == "0x" {
		keyHex = keyHex[2:]
	}

	privateKey, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return fmt.Errorf("failed to parse private key: %w", err)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey)

	// Get balance
	balance, err := g.eth.GetClient().BalanceAt(context.Background(), address, nil)
	if err != nil {
		return fmt.Errorf("failed to get balance for private key account: %w", err)
	}

	if balance.Cmp(big.NewInt(0)) == 0 {
		return fmt.Errorf("private key account %s has zero balance", address.Hex())
	}

	// Get nonce
	nonce, err := g.eth.GetNonce(address)
	if err != nil {
		return fmt.Errorf("failed to get nonce: %w", err)
	}

	g.fundedKey = privateKey
	g.currentNonce = nonce

	// Also add to allFundedAccounts for parallel funding support
	g.allFundedAccounts = []fundedAccountInfo{{
		key:     privateKey,
		address: address,
		nonce:   nonce,
	}}

	g.log.Info("Using private key account",
		"address", address.Hex(),
		"balance", balance.String(),
		"nonce", nonce)

	return nil
}

// setupFromMnemonic derives accounts from mnemonic and stores all funded accounts
func (g *TransactionGenerator) setupFromMnemonic() error {
	// Create HD wallet from mnemonic
	wallet, err := hdwallet.NewFromMnemonic(g.mnemonic)
	if err != nil {
		return fmt.Errorf("failed to create wallet from mnemonic: %w", err)
	}
	g.wallet = wallet

	var maxBalance *big.Int
	var fundedAccount accounts.Account
	var fundedKey *ecdsa.PrivateKey
	g.allFundedAccounts = make([]fundedAccountInfo, 0, 20)

	// For custom mnemonics, truncate for security; show full default mnemonic
	mnemonicDisplay := g.mnemonic
	if g.mnemonic != DefaultMnemonic && len(g.mnemonic) > 20 {
		mnemonicDisplay = g.mnemonic[:10] + "..." + g.mnemonic[len(g.mnemonic)-10:]
	}

	// Check first 20 accounts
	g.log.Info("Checking first 20 accounts from mnemonic for funds...", "mnemonic", mnemonicDisplay)
	for i := 0; i < 20; i++ {
		// Derive account using standard Ethereum path: m/44'/60'/0'/0/i
		path := hdwallet.MustParseDerivationPath(fmt.Sprintf("m/44'/60'/0'/0/%d", i))
		account, err := wallet.Derive(path, false)
		if err != nil {
			g.log.Warn("Failed to derive account", "index", i, "error", err)
			continue
		}

		// Get balance
		balance, err := g.eth.GetClient().BalanceAt(context.Background(), account.Address, nil)
		if err != nil {
			g.log.Warn("Failed to get balance", "index", i, "address", account.Address.Hex(), "error", err)
			continue
		}

		g.log.Debug("Account balance", "index", i, "address", account.Address.Hex(), "balance", balance.String())

		// Calculate minimum balance needed to be useful for parallel funding
		// Assume account should be able to fund at least 10 random accounts
		// Cost per account: (gas × maxGasPrice × 2 buffer) + (value × txsPerSender)
		// For 10 accounts with ~8 txs each: ~0.067 ETH minimum
		minBalance := big.NewInt(MinBalanceForFunding)

		// Store accounts with sufficient balance for parallel funding
		if balance.Cmp(minBalance) >= 0 {
			key, err := wallet.PrivateKey(account)
			if err != nil {
				g.log.Warn("Failed to get private key", "index", i, "address", account.Address.Hex(), "error", err)
				continue
			}

			nonce, err := g.eth.GetNonce(account.Address)
			if err != nil {
				g.log.Warn("Failed to get nonce", "index", i, "address", account.Address.Hex(), "error", err)
				continue
			}

			g.allFundedAccounts = append(g.allFundedAccounts, fundedAccountInfo{
				account: account,
				key:     key,
				address: account.Address,
				nonce:   nonce,
			})

			g.log.Debug("Added account for parallel funding", "index", i, "address", account.Address.Hex())
		} else if balance.Cmp(big.NewInt(0)) > 0 {
			g.log.Debug("Skipping underfunded account", "index", i, "address", account.Address.Hex(), "balance", balance.String(), "minimum", minBalance.String())
		}

		// Track the account with highest balance for backward compatibility (regardless of minimum)
		if balance.Cmp(big.NewInt(0)) > 0 && (maxBalance == nil || balance.Cmp(maxBalance) > 0) {
			maxBalance = balance
			fundedAccount = account

			// Get key for this account
			key, err := wallet.PrivateKey(account)
			if err == nil {
				fundedKey = key
			}
		}
	}

	if maxBalance == nil || fundedKey == nil {
		return fmt.Errorf("no funded account found in first 20 addresses")
	}

	g.fundedAccount = fundedAccount
	g.fundedKey = fundedKey

	// Get initial nonce
	nonce, err := g.eth.GetNonce(fundedAccount.Address)
	if err != nil {
		return fmt.Errorf("failed to get initial nonce: %w", err)
	}
	g.currentNonce = nonce

	g.log.Info("Using largest funded account",
		"address", fundedAccount.Address.Hex(),
		"balance", maxBalance.String(),
		"nonce", nonce)

	if len(g.allFundedAccounts) == 0 {
		g.log.Warn("No accounts with sufficient balance for parallel funding (minimum 0.067 ETH). Will use single-sender mode only.")
	} else {
		g.log.Info("Found funded accounts for parallel operation", "count", len(g.allFundedAccounts))
	}

	return nil
}

// getNextNonce returns and increments the nonce for the funded account
func (g *TransactionGenerator) getNextNonce() uint64 {
	g.nonceMutex.Lock()
	defer g.nonceMutex.Unlock()
	nonce := g.currentNonce
	g.currentNonce++
	return nonce
}

// PrefundRandomSendersParallel creates and funds random sender accounts in parallel
func (g *TransactionGenerator) PrefundRandomSendersParallel(count int, txsPerSender int) error {
	if len(g.allFundedAccounts) == 0 {
		return fmt.Errorf("no funded source accounts available for parallel funding (minimum 0.067 ETH per account required). Use --accounts 1 for single-sender mode")
	}

	g.log.Info("Starting parallel prefunding",
		"targetAccounts", count,
		"sourceAccounts", len(g.allFundedAccounts),
		"txsPerSender", txsPerSender)

	// Calculate minimal funding per sender
	gasLimit := uint64(GasLimitETHTransfer)

	maxGasPrice := big.NewInt(GasFeeCapGwei)
	valuePerTx := big.NewInt(TxValueWei)
	bufferMultiplier := big.NewInt(2) // 2x buffer for gas price spikes

	// Cost per tx = (gas × maxGasPrice × buffer) + value
	costPerTx := new(big.Int).Mul(big.NewInt(int64(gasLimit)), maxGasPrice)
	costPerTx.Mul(costPerTx, bufferMultiplier)
	costPerTx.Add(costPerTx, valuePerTx)

	// Total funding per sender
	fundingPerSender := new(big.Int).Mul(costPerTx, big.NewInt(int64(txsPerSender)))

	ethAmount := new(big.Float).Quo(new(big.Float).SetInt(fundingPerSender), big.NewFloat(1e18))
	g.log.Info("Calculated minimal funding",
		"gasLimit", gasLimit,
		"costPerTx", costPerTx.String()+" wei",
		"fundingPerSender", fmt.Sprintf("%.8f ETH", ethAmount))

	// Generate all random recipient accounts
	g.log.Info("Generating random sender accounts", "count", count)
	g.senderKeys = make([]*ecdsa.PrivateKey, count)
	g.senderNonces = make([]uint64, count)
	g.senderMutexes = make([]sync.Mutex, count)

	for i := 0; i < count; i++ {
		key, err := crypto.GenerateKey()
		if err != nil {
			return fmt.Errorf("failed to generate key %d: %w", i, err)
		}
		g.senderKeys[i] = key
		g.senderNonces[i] = 0
	}

	// Distribute funding work across source accounts
	sendersPerSource := count / len(g.allFundedAccounts)
	remainder := count % len(g.allFundedAccounts)

	var wg sync.WaitGroup
	allTxHashes := make([]common.Hash, 0, count)
	var mu sync.Mutex
	var errorOccurred atomic.Bool
	var lastError error

	offset := 0
	for i, sourceAccount := range g.allFundedAccounts {
		accountSenders := sendersPerSource
		if i < remainder {
			accountSenders++
		}

		if accountSenders == 0 {
			continue
		}

		wg.Add(1)
		go func(srcAccount fundedAccountInfo, startIdx, endIdx int, accountIdx int) {
			defer wg.Done()

			// Build batch for this source account
			batch := make([]*types.Transaction, 0, endIdx-startIdx)
			for j := startIdx; j < endIdx; j++ {
				addr := crypto.PubkeyToAddress(g.senderKeys[j].PublicKey)

				tx := types.NewTx(&types.DynamicFeeTx{
					ChainID:   g.chainID,
					Nonce:     srcAccount.nonce + uint64(j-startIdx),
					To:        &addr,
					Value:     fundingPerSender,
					Gas:       GasLimitETHTransfer,
					GasTipCap: big.NewInt(GasTipCapGwei),
					GasFeeCap: big.NewInt(GasFeeCapGwei),
				})

				signer := types.LatestSignerForChainID(g.chainID)
				signedTx, err := types.SignTx(tx, signer, srcAccount.key)
				if err != nil {
					g.log.Error("Failed to sign funding tx", "error", err)
					errorOccurred.Store(true)
					return
				}
				batch = append(batch, signedTx)
			}

			// Send batch
			g.log.Info("Sending funding batch", "sourceAccount", srcAccount.address.Hex(), "size", len(batch))
			results, _, err := g.eth.SendBatch(batch)
			if err != nil {
				g.log.Error("Failed to send funding batch", "sourceAccount", srcAccount.address.Hex(), "error", err)
				errorOccurred.Store(true)
				lastError = err
				return
			}

			// Collect successful tx hashes
			successCount := 0
			for _, res := range results {
				if res.Error == nil {
					if hash, ok := res.Result.(*common.Hash); ok {
						mu.Lock()
						allTxHashes = append(allTxHashes, *hash)
						mu.Unlock()
						successCount++
					}
				} else {
					g.log.Warn("Funding transaction failed", "error", res.Error)
				}
			}

			g.log.Info("Funding batch sent", "sourceAccount", srcAccount.address.Hex(), "successful", successCount, "total", len(batch))

		}(sourceAccount, offset, offset+accountSenders, i)

		offset += accountSenders
	}

	wg.Wait()

	if errorOccurred.Load() {
		return fmt.Errorf("errors occurred during parallel funding: %w", lastError)
	}

	g.log.Info("All funding transactions sent", "total", len(allTxHashes))

	// Poll for confirmations
	g.log.Info("Waiting for funding transaction confirmations...")
	confirmedHashes := make([]common.Hash, 0)
	pendingHashes := make([]common.Hash, len(allTxHashes))
	copy(pendingHashes, allTxHashes)

	pollStart := time.Now()
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			g.log.Warn("Timeout waiting for funding confirmations",
				"confirmed", len(confirmedHashes),
				"pending", len(pendingHashes),
				"elapsed", time.Since(pollStart))
			if len(confirmedHashes) < len(allTxHashes)/2 {
				return fmt.Errorf("less than half of funding transactions confirmed")
			}
			// Continue with partial confirmations
			g.log.Info("Continuing with partial confirmations")
			return nil

		case <-ticker.C:
			if len(pendingHashes) == 0 {
				g.log.Info("All funding transactions confirmed",
					"total", len(confirmedHashes),
					"elapsed", time.Since(pollStart))
				return nil
			}

			receipts, err := g.eth.BatchGetTransactionReceipts(pendingHashes)
			if err != nil {
				g.log.Error("Failed to fetch receipts", "error", err)
				continue
			}

			newPending := make([]common.Hash, 0)
			for _, hash := range pendingHashes {
				if receipt, found := receipts[hash]; found && receipt != nil {
					confirmedHashes = append(confirmedHashes, hash)
				} else {
					newPending = append(newPending, hash)
				}
			}
			pendingHashes = newPending

			g.log.Info("Funding confirmation progress",
				"confirmed", len(confirmedHashes),
				"pending", len(pendingHashes),
				"elapsed", time.Since(pollStart).Round(time.Second))
		}
	}
}

// GenerateBatch creates a batch of transactions
func (g *TransactionGenerator) GenerateBatch(count int64) []*types.Transaction {
	batch := make([]*types.Transaction, 0, count)
	for i := int64(0); i < count; i++ {
		tx := g.GenerateTransaction()
		if tx != nil {
			batch = append(batch, tx)
		}
	}
	return batch
}

// GenerateTransaction creates a single transaction
func (g *TransactionGenerator) GenerateTransaction() *types.Transaction {
	var privateKey *ecdsa.PrivateKey
	var nonce uint64
	var value *big.Int

	if len(g.senderKeys) > 0 {
		// Multi-sender mode: use pre-funded accounts in round-robin
		// Note: Add(1) returns the NEW value, so subtract 1 to get current index before wrap
		senderIdx := int((g.currentSenderIdx.Add(1) - 1) % uint64(len(g.senderKeys)))

		g.senderMutexes[senderIdx].Lock()
		privateKey = g.senderKeys[senderIdx]
		nonce = g.senderNonces[senderIdx]
		g.senderNonces[senderIdx]++
		g.senderMutexes[senderIdx].Unlock()

		value = big.NewInt(TxValueWei)
	} else {
		// Single-sender mode: use funded account
		if g.fundedKey == nil {
			g.log.Error("No funded account available")
			return nil
		}
		privateKey = g.fundedKey
		nonce = g.getNextNonce()
		value = big.NewInt(TxValueWei)
	}

	// Set gas fees
	gasTipCap := big.NewInt(GasTipCapGwei)
	gasFeeCap := big.NewInt(GasFeeCapGwei)

	// Self-transfer: send to sender's own address (funds not lost, only gas spent)
	senderAddr := crypto.PubkeyToAddress(privateKey.PublicKey)

	// Create the transaction
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   g.chainID,
		Nonce:     nonce,
		To:        &senderAddr,
		Value:     value,
		Gas:       GasLimitETHTransfer,
		GasTipCap: gasTipCap,
		GasFeeCap: gasFeeCap,
	})

	// Sign the transaction
	signer := types.LatestSignerForChainID(g.chainID)
	signedTx, err := types.SignTx(tx, signer, privateKey)
	if err != nil {
		g.log.Error("Failed to sign transaction", "error", err)
		return nil
	}

	return signedTx
}
