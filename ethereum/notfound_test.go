package ethereum

import (
	"context"
	"os"
	"testing"
)

// TestIsNotFoundOnMissingBlock verifies that a header request for a block the
// node does not have produces an error IsNotFound recognises. This guards the
// reporter's retry path, which relies on the ethereum.NotFound sentinel rather
// than matching on error strings.
//
// Opt in by pointing RAILGUN_TEST_RPC at a running node, e.g.
//
//	anvil --port 8545 &
//	RAILGUN_TEST_RPC=http://127.0.0.1:8545 go test ./ethereum/
func TestIsNotFoundOnMissingBlock(t *testing.T) {
	url := os.Getenv("RAILGUN_TEST_RPC")
	if url == "" {
		t.Skip("set RAILGUN_TEST_RPC to a running node to run this test")
	}

	ctx := context.Background()

	e := NewEthereum(&Config{URL: url})
	if err := e.Connect(ctx); err != nil {
		t.Fatalf("failed to connect to %s: %v", url, err)
	}
	defer e.Close()

	latest, err := e.GetBlockNumber(ctx)
	if err != nil {
		t.Fatalf("failed to read chain head from %s: %v", url, err)
	}

	// Far beyond the chain head, so the node cannot have it.
	_, err = e.GetBlockHeaderByNumber(ctx, latest+1_000_000)
	if err == nil {
		t.Fatal("expected an error for a block beyond the chain head")
	}

	if !IsNotFound(err) {
		t.Fatalf("IsNotFound did not recognise the missing-block error: %v (type %T)", err, err)
	}
}
