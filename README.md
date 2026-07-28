<p align="center">
  <img src="banner.jpg" alt="Railgun Banner" width="100%">
</p>

# Railgun

A fast, efficient, and reliable Ethereum / EVM throughput benchmarking tool.

## Overview

Railgun is a command-line tool designed to stress test Ethereum-compatible networks by sending large batches of transactions and measuring throughput. It provides detailed metrics including transactions per second (TPS), block inclusion rates, gas usage, and timing analysis.

## Features

- **Concurrent Batch Sending**: Send multiple transaction batches concurrently with configurable concurrency limits
- **Intelligent Receipt Polling**: Automatically polls for transaction receipts with configurable intervals
- **Detailed Metrics**: Block-by-block analysis with TPS calculations, gas usage, and timing data
- **Flexible Configuration**: Configure RPC endpoints, chain IDs, batch sizes, delays, and more
- **Real-time Progress Tracking**: Monitor transaction submission and confirmation in real-time
- **Accurate TPS Calculation**: Calculates overall TPS based on actual block timestamps

## Requirements

- Go 1.25.1 or later
- Access to an Ethereum-compatible RPC endpoint
- A funded wallet (default: Foundry test mnemonic)

## Installation

### From Source

```bash
git clone https://github.com/sledro/railgun
cd railgun
go build
```

This produces a `railgun` binary in the repository root.

### Using Go Install

```bash
go install github.com/sledro/railgun@latest
```

## Usage

### Basic Usage

Run a TPS benchmark with default settings (50 ETH transfers, batch size of 10):

```bash
railgun tps -r https://rpc.example.com
```

### Custom Configuration

```bash
railgun tps \
  --rpc https://rpc.example.com \
  --txCount 1000 \
  --batchSize 100 \
  --batchDelay 100 \
  --concurrent 10
```

### Available Flags

| Flag             | Alias | Default      | Description                                                                    |
| ---------------- | ----- | ------------ | ------------------------------------------------------------------------------ |
| `--rpc`          | `-r`  | **required** | Ethereum RPC URL (`http(s)://` or `ws(s)://`)                                  |
| `--txCount`      | `-t`  | `50`         | Number of transactions to send                                                 |
| `--batchSize`    | `-b`  | `10`         | Number of transactions per batch                                               |
| `--batchDelay`   | `-d`  | `10`         | Delay between batches (milliseconds)                                           |
| `--concurrent`   | `-c`  | `1`          | Concurrent batch sends: 1=sequential with delays, >1=parallel (max 50)         |
| `--accounts`     | `-a`  | `1`          | Number of sender accounts for parallel nonce management (max 10,000)           |
| `--mnemonic`     | `-m`  | (below)      | BIP39 mnemonic phrase. Prefer `$RAILGUN_MNEMONIC`                              |
| `--privatekey`   | `-k`  | (empty)      | Private key hex, takes precedence over mnemonic. Prefer `$RAILGUN_PRIVATE_KEY` |
| `--gasTipCap`    |       | `1`          | Max priority fee per gas, in gwei                                              |
| `--gasFeeCap`    |       | `20`         | Max fee per gas, in gwei. Must exceed the chain's base fee                     |
| `--singleSource` |       | `false`      | Prefund from only the first funded account instead of all of them              |
| `--reportDir`    |       | `reports`    | Directory for saved reports, one file per run named for the start time         |
| `--noReport`     |       | `false`      | Do not save a report file (the report is still printed to stdout)              |

The root-level `--loglevel` flag (`debug`, `info`, `warn`, `error`) must come _before_ the
subcommand: `railgun --loglevel debug tps -r ...`.

### Passing secrets

`--mnemonic` and `--privatekey` are visible in `ps` output and shell history. Prefer the
environment variables:

```bash
export RAILGUN_PRIVATE_KEY=0x...
railgun tps -r https://rpc.example.com
```

**Default Mnemonic** (Foundry/Anvil test mnemonic):

```bash
test test test test test test test test test test test junk
```

## How It Works

Railgun sends standard ETH transfer transactions to benchmark network throughput:

- **To**: Sender's own address (funds not lost, only gas spent)
- **Value**: 1000 wei
- **Gas Limit**: 21,000
- **Gas Fees**: 1 gwei tip, 20 gwei fee cap by default; raise with `--gasTipCap` / `--gasFeeCap`

### Example Commands

```bash
# Light test
railgun tps -r https://rpc.example.com -t 100

# Stress test
railgun tps -r https://rpc.example.com -t 50000 -b 5000 -c 10
```

### Other Commands

#### Version Information

```bash
railgun version
# or
railgun v
```

## Output Explanation

Structured logs go to **stderr**; the report goes to **stdout**, so the report can be
redirected on its own:

```bash
railgun tps -r https://rpc.example.com -t 250 > report.txt
```

```
╔═════════════════════════════════════════════════════════════════════════════════════════╗
║                                RAILGUN BENCHMARK REPORT                                 ║
╚═════════════════════════════════════════════════════════════════════════════════════════╝

⚡ TRANSACTION SUMMARY
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Total Submitted:      250
  Confirmed:            250 (100.0%)

⏱️  TIMING BREAKDOWN
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Generation:           0.01s
  Submission:           0.28s (899 tx/s)
  Confirmation:         1.00s
  Total Elapsed:        1.29s

📊 BLOCK REPORT
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
     Block     Our Txs   Total Txs      Tx Bytes   Gas Used  Gas Bar                   Time
  ─────────────────────────────────────────────────────────────────────────────────────────
         6         250         250        28,618     17.50%  ███░░░░░░░░░░░░░░░░░  20:17:01
  ─────────────────────────────────────────────────────────────────────────────────────────

📈 STATISTICS
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Total Blocks:         1
  Avg Tx/Block:         250.00
  Min Tx/Block:         250
  Max Tx/Block:         250
  Avg Gas Usage:        17.50%
  Total Tx Bytes:       28,618
  Measured Block Time:  1.00s

  🚀 CHAIN THROUGHPUT:    250.00 TPS

═══════════════════════════════════════════════════════════════════════════════════════════
```

### Saved reports

Every run also writes a plain-text copy of the report to `reports/`, named for the time the
run started:

```
reports/railgun-2026-07-28_20-26-06.txt
```

The saved file has no colour codes and carries a preamble recording the parameters that
produced it, so a directory of timestamped reports stays interpretable later:

```
🧾 RUN
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Started:              2026-07-28T20:26:06+01:00
  RPC:                  http://127.0.0.1:8599
  Chain ID:             31337
  Transactions:         120
  Batch Size:           20
  Batch Delay:          10ms
  Concurrency:          4
  Sender Accounts:      1
  Gas Tip Cap:          1 gwei
  Gas Fee Cap:          20 gwei
  Railgun Version:      dev
```

Change the location with `--reportDir` (nested paths are created as needed) or turn it off
with `--noReport`:

```bash
railgun tps -r https://rpc.example.com --reportDir results/mainnet
railgun tps -r https://rpc.example.com --noReport
```

Two runs starting in the same second get a numeric suffix rather than overwriting each
other. An interrupted run still saves its partial report.

### Transaction summary

Transaction accounting is a funnel:

```
Total Submitted = Accepted + Rejected on Submit   (everything Railgun tried to send)
Accepted        = Confirmed + Unconfirmed         (everything the node took)
```

- **Total Submitted**: transactions attempted
- **Accepted** / **Rejected on Submit**: shown only when the node rejected something, for
  example an underpriced transaction or a nonce conflict
- **Confirmed**: found in a block, with the percentage taken against Total Submitted
- **Reverted**: confirmed but with a failed receipt status (shown only when non-zero)
- **Unconfirmed**: accepted but still not mined when polling stopped

### Timing breakdown

- **Generation**: building and signing every transaction, which happens before any are sent
- **Submission**: wall-clock time of the send phase, with the accepted-transaction rate
- **Confirmation**: time spent polling for receipts
- **Total Elapsed**: the whole run, including prefunding and generation

### Block report

- **Block**: block number
- **Our Txs**: Railgun transactions in this block (empty blocks in the range are shown in grey)
- **Total Txs**: all transactions in the block, including other users'
- **Tx Bytes**: total encoded size of the block's transactions
- **Gas Used**: percentage of the block gas limit consumed
- **Measured Block Time**: the block interval derived from the chain, which is the
  denominator behind Chain Throughput
- **Chain Throughput**: confirmed transactions divided by the block-timestamp window, not
  by wall-clock time

## Architecture

Railgun is organized into modular components:

- **Generator** (`gun/generator.go`): Discovers funded accounts, optionally prefunds ephemeral senders (`--accounts > 1`), and signs every transaction
- **Sender** (`gun/sender.go`): Manages concurrent batch sending with rate limiting
- **Reporter** (`gun/reporter.go`): Polls for receipts and generates performance reports
- **Ethereum Client** (`ethereum/ethereum.go`): Handles RPC communication and batch operations

## Benchmark Flow

0. **Prefunding Phase** (only with `--accounts > 1`): generates ephemeral keys and funds them from the discovered accounts
1. **Generation Phase**: builds and signs every transaction upfront, before any is sent
2. **Sending Phase**: sends batches with the configured concurrency and pacing
3. **Polling Phase**: polls for receipts until all confirm or no new confirmation arrives for 30s
4. **Reporting Phase**: analyses block inclusion and derives throughput from block timestamps

Because generation completes before sending begins, generation time is excluded from
throughput but still counted in Total Elapsed.

## Troubleshooting

### "Transaction underpriced" errors

The chain's base fee exceeds `--gasFeeCap` (20 gwei by default), so every transaction is
rejected. Raise the cap:

```bash
railgun tps -r https://rpc.example.com --gasFeeCap 200 --gasTipCap 5
```

The report distinguishes this case explicitly: `Rejected on Submit` will equal
`Total Submitted`.

### "no funded account found in first 20 addresses"

Railgun scans `m/44'/60'/0'/0/{0..19}` for a funded account. Either the mnemonic is wrong or
the accounts are empty on this chain. Pass `--privatekey` (via `$RAILGUN_PRIVATE_KEY`) to
use a specific account instead.

### "source account ... needs N ETH to fund M sender(s)"

`--accounts` multiplied by the per-sender funding requirement exceeds the source balance.
Lower `--accounts` or `--txCount`, or fund the account. This is checked before any funding
transaction is sent, so it fails immediately rather than after a timeout.

### Transactions not confirming

- Check the account has enough balance for gas
- Railgun uses stall detection and gives up 30 seconds after the last new confirmation
- Anything accepted but unmined at that point is reported as `Unconfirmed`

### Low TPS results

- Reduce `--batchDelay` to send batches faster
- Increase `--concurrent` to send more batches in parallel
- Use `--accounts N` so nonces come from several senders rather than one serial sequence
- Compare `Chain Throughput` against `Measured Block Time`: on a slow-block chain the
  ceiling is set by the chain, not by Railgun

### Interrupting a run

Ctrl-C cancels cleanly. In-flight receipt polling returns what it has and a partial report
is still printed; the exit code is 130.

## Development

### Building

```bash
go build             # produces ./railgun
go build -o bin/railgun .   # or somewhere else
```

### Testing

```bash
go test ./...              # unit tests
go test -race ./...        # unit tests under the race detector
go vet ./...
gofmt -l .                 # should print nothing
```

Tests that need a live chain are opt-in and skip when `RAILGUN_TEST_RPC` is unset:

```bash
anvil --port 8545 &
RAILGUN_TEST_RPC=http://127.0.0.1:8545 go test ./...
```

## Contributing

Contributions are welcome! Please feel free to submit issues or pull requests.
