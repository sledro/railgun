<p align="center">
  <img src="banner.jpg" alt="Railgun Banner" width="100%">
</p>

# Railgun

A fast, efficient, and reliable Ethereum / EVM Throughput Benchmarking Tool.

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
  --concurrency 10
```

### Available Flags

| Flag           | Alias | Default      | Description                                                            |
| -------------- | ----- | ------------ | ---------------------------------------------------------------------- |
| `--rpc`        | `-r`  | **required** | Ethereum RPC URL                                                       |
| `--txCount`    | `-t`  | `50`         | Number of transactions to send                                         |
| `--batchSize`  | `-b`  | `10`         | Number of transactions per batch                                       |
| `--batchDelay` | `-d`  | `10`         | Delay between batches (milliseconds)                                   |
| `--concurrent` | `-c`  | `1`          | Concurrent batch sends: 1=sequential with delays, >1=parallel (max 50) |
| `--accounts`   | `-a`  | `1`          | Number of sender accounts for parallel nonce management                |
| `--mnemonic`   | `-m`  | (below)      | BIP39 mnemonic phrase (see default below)                              |
| `--privatekey` | `-k`  | (empty)      | Private key hex (takes precedence over mnemonic)                       |

**Default Mnemonic** (Foundry/Anvil test mnemonic):

```bash
test test test test test test test test test test test junk
```

## How It Works

Railgun sends standard ETH transfer transactions to benchmark network throughput:

- **To**: Sender's own address (funds not lost, only gas spent)
- **Value**: 1000 wei
- **Gas Limit**: 21,000
- **Gas Fees**: 1 gwei tip, 20 gwei fee cap

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

Railgun produces a comprehensive report with two main sections:

### Transaction Summary

```bash
=== TRANSACTION SUMMARY ===
Total Submitted:    1000
Confirmed:          987
Failed:             13
Elapsed Time:       4.52s
```

- **Total Submitted**: Number of transactions sent to the network
- **Confirmed**: Transactions successfully mined in blocks
- **Failed**: Transactions that failed to send or were rejected
- **Elapsed Time**: Total time from start to finish

### Block Report

```bash
=== BLOCK REPORT ===
Block        TxCount  GasUsed%    GasUsed    Timestamp      TPS
--------------------------------------------------------------------------------
1234567      329      45.23       45230000   14:32:01       329.00
1234568      331      46.10       46100000   14:32:02       331.00
1234569      327      44.87       44870000   14:32:03       327.00
--------------------------------------------------------------------------------
Total Blocks:       3
Avg Tx/Block:       329.00
Min Tx/Block:       327
Max Tx/Block:       331
Overall TPS:        329.00
===================
```

- **Block**: Block number containing your transactions
- **TxCount**: Number of your transactions in this block
- **GasUsed%**: Percentage of block gas limit used
- **GasUsed**: Total gas used by the block
- **Timestamp**: Block creation time
- **TPS**: Transactions per second for this block
- **Overall TPS**: Average TPS calculated from `(total confirmed txs) / (last block time - first block time)`

## Architecture

Railgun is organized into modular components:

- **Generator** (`railgun/generator.go`): Creates signed transactions with random sender addresses
- **Sender** (`railgun/sender.go`): Manages concurrent batch sending with rate limiting
- **Reporter** (`railgun/reporter.go`): Polls for receipts and generates performance reports
- **Ethereum Client** (`ethereum/ethereum.go`): Handles RPC communication and batch operations

## Benchmark Flow

1. **Generation Phase**: Creates all transactions upfront with unique random sender addresses
2. **Sending Phase**: Sends batches concurrently with controlled concurrency and optional delays
3. **Polling Phase**: Actively polls for transaction receipts until all are confirmed or timeout
4. **Reporting Phase**: Analyzes block inclusion and calculates comprehensive metrics

## Troubleshooting

### "Transaction underpriced" errors

This typically means:

- The network requires non-zero gas fees
- Try reducing concurrency: `--concurrency 2`
- Increase batch delay: `--batchDelay 100`

### "Connection refused" errors

- Verify RPC URL is correct and accessible
- Check firewall settings
- Ensure the RPC endpoint supports the required methods

### Transactions not confirming

- Verify the chain ID matches your network
- Check that your account has sufficient funds
- Note: Railgun uses stall detection (exits if no new confirmations for 30 seconds)

### Low TPS results

- Reduce `--batchDelay` to send batches faster
- Increase `--concurrency` to send more batches in parallel
- Check network capacity and block time

### Memory issues with large tests

- Break large tests into smaller batches
- Reduce `--concurrency` to limit parallel operations
- Monitor system resources

## Development

### Building

```bash
go build -o railgun
```

### Running Tests

```bash
go test ./...
```

## Contributing

Contributions are welcome! Please feel free to submit issues or pull requests.
