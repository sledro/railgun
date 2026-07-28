package gun

import (
	"crypto/ecdsa"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestGenerator builds a generator with no RPC dependency, exercising only
// the local nonce bookkeeping.
func newTestGenerator(t *testing.T, senderCount int) *TransactionGenerator {
	t.Helper()

	g := &TransactionGenerator{
		chainID:   big.NewInt(31337),
		gasTipCap: big.NewInt(DefaultGasTipCapWei),
		gasFeeCap: big.NewInt(DefaultGasFeeCapWei),
		log:       discardLogger(),
	}

	if senderCount > 0 {
		g.senderKeys = make([]*ecdsa.PrivateKey, senderCount)
		g.senderNonces = make([]uint64, senderCount)
		g.senderMutexes = make([]sync.Mutex, senderCount)

		for i := range g.senderKeys {
			key, err := crypto.GenerateKey()
			if err != nil {
				t.Fatalf("failed to generate key %d: %v", i, err)
			}
			g.senderKeys[i] = key
		}
		return g
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	g.fundedKey = key

	return g
}

// TestGetNextNonceIsUniqueUnderConcurrency covers the single-account path. A
// duplicate nonce means one transaction silently replaces another, and a gap
// stalls every later transaction from that sender.
func TestGetNextNonceIsUniqueUnderConcurrency(t *testing.T) {
	g := newTestGenerator(t, 0)
	g.currentNonce = 100

	const goroutines = 16
	const perGoroutine = 200
	total := goroutines * perGoroutine

	results := make([]uint64, total)
	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := range perGoroutine {
				results[worker*perGoroutine+j] = g.getNextNonce()
			}
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]bool, total)
	for _, nonce := range results {
		if seen[nonce] {
			t.Fatalf("nonce %d was handed out more than once", nonce)
		}
		seen[nonce] = true
	}

	// The allocated range must be contiguous: [100, 100+total).
	for want := uint64(100); want < uint64(100)+uint64(total); want++ {
		if !seen[want] {
			t.Fatalf("nonce %d was never allocated, leaving a gap", want)
		}
	}

	if g.currentNonce != uint64(100)+uint64(total) {
		t.Errorf("currentNonce = %d, want %d", g.currentNonce, uint64(100)+uint64(total))
	}
}

// TestGenerateTransactionMultiSenderNonces covers the multi-account path: each
// sender must get its own gapless nonce sequence starting at 0, even when
// transactions are generated concurrently.
func TestGenerateTransactionMultiSenderNonces(t *testing.T) {
	const senderCount = 8
	const txPerSender = 50

	g := newTestGenerator(t, senderCount)

	total := senderCount * txPerSender
	txs := make([]struct {
		from  string
		nonce uint64
	}, total)

	var wg sync.WaitGroup
	for i := range total {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			tx := g.GenerateTransaction()
			if tx == nil {
				t.Errorf("GenerateTransaction returned nil at %d", idx)
				return
			}

			// A self-transfer's recipient is its sender.
			txs[idx].from = tx.To().Hex()
			txs[idx].nonce = tx.Nonce()
		}(i)
	}
	wg.Wait()

	perSender := make(map[string][]uint64)
	for _, tx := range txs {
		perSender[tx.from] = append(perSender[tx.from], tx.nonce)
	}

	if len(perSender) != senderCount {
		t.Fatalf("transactions came from %d senders, want %d", len(perSender), senderCount)
	}

	for addr, nonces := range perSender {
		if len(nonces) != txPerSender {
			t.Errorf("sender %s produced %d transactions, want %d (round-robin should be even)", addr, len(nonces), txPerSender)
		}

		seen := make(map[uint64]bool, len(nonces))
		for _, n := range nonces {
			if seen[n] {
				t.Fatalf("sender %s reused nonce %d", addr, n)
			}
			seen[n] = true
		}

		// Fresh keys mean each sender must start at 0 with no gaps.
		for want := range uint64(len(nonces)) {
			if !seen[want] {
				t.Errorf("sender %s is missing nonce %d", addr, want)
			}
		}
	}
}

// TestGenerateTransactionUsesConfiguredGasCaps guards the flags added for
// chains whose base fee exceeds the old hardcoded 20 gwei cap.
func TestGenerateTransactionUsesConfiguredGasCaps(t *testing.T) {
	g := newTestGenerator(t, 0)
	g.gasTipCap = big.NewInt(3_000_000_000)
	g.gasFeeCap = big.NewInt(150_000_000_000)

	tx := g.GenerateTransaction()
	if tx == nil {
		t.Fatal("GenerateTransaction returned nil")
	}

	if tx.GasTipCap().Cmp(g.gasTipCap) != 0 {
		t.Errorf("GasTipCap = %s, want %s", tx.GasTipCap(), g.gasTipCap)
	}
	if tx.GasFeeCap().Cmp(g.gasFeeCap) != 0 {
		t.Errorf("GasFeeCap = %s, want %s", tx.GasFeeCap(), g.gasFeeCap)
	}
	if tx.Gas() != GasLimitETHTransfer {
		t.Errorf("Gas = %d, want %d", tx.Gas(), GasLimitETHTransfer)
	}
}

func TestWeiToETH(t *testing.T) {
	tests := []struct {
		wei  *big.Int
		want string
	}{
		{big.NewInt(0), "0.000000"},
		{big.NewInt(1_000_000_000_000_000_000), "1.000000"},
		{big.NewInt(MinBalanceForFunding), "0.067000"},
	}

	for _, tt := range tests {
		if got := weiToETH(tt.wei); got != tt.want {
			t.Errorf("weiToETH(%s) = %q, want %q", tt.wei, got, tt.want)
		}
	}
}
