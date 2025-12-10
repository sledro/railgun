package railgun

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/sledro/railgun/ethereum"
)

// BlockReport contains metrics for a single block
type BlockReport struct {
	BlockNumber  uint64
	TxCount      int    // Our transactions in this block
	TotalTxCount int    // Total transactions in this block (including others)
	TxSizeBytes  uint64 // Total size of all transactions in bytes
	GasUsed      uint64
	GasLimit     uint64
	Timestamp    time.Time
}

// Reporter handles transaction receipt polling and report generation
type Reporter struct {
	eth *ethereum.Ethereum
	log *slog.Logger
}

// Polling constants
const (
	PollInterval = 500 // milliseconds - how often to check for receipts
	StallTimeout = 30  // seconds - exit if no new confirmations for this long
)

// ReportData contains all the data needed for the final report
type ReportData struct {
	TotalSubmitted    int
	Confirmed         int
	Failed            int
	Pending           int
	ElapsedTime       time.Duration
	GenerationTime    time.Duration
	SubmissionTime    time.Duration
	ConfirmationTime  time.Duration
	BlockReports      []BlockReport
	OverallTPS        float64
	SubmissionRate    float64
	AvgTxPerBlock     float64
	MinTxPerBlock     int
	MaxTxPerBlock     int
	AvgGasUsedPercent float64
}

// NewReporter creates a new reporter
func NewReporter(eth *ethereum.Ethereum, log *slog.Logger) *Reporter {
	return &Reporter{
		eth: eth,
		log: log,
	}
}

// GenerateReport polls for receipts and generates the final report
func (r *Reporter) GenerateReport(txHashes []common.Hash, startTime time.Time, genTime time.Duration, subTime time.Duration) *ReportData {
	// Poll for receipts
	pollStart := time.Now()
	confirmedHashes, receipts := r.pollForReceipts(txHashes)
	confirmationTime := time.Since(pollStart)

	// Calculate elapsed time
	elapsedTime := time.Since(startTime)

	// Calculate submission rate
	submissionRate := 0.0
	if subTime.Seconds() > 0 {
		submissionRate = float64(len(txHashes)) / subTime.Seconds()
	}

	// Find which blocks contain our transactions (using already-fetched receipts)
	blockTxCounts := r.findTransactionBlocksFromReceipts(receipts)

	if len(blockTxCounts) == 0 {
		r.log.Warn("No transaction receipts found - transactions may not have been mined")
		return &ReportData{
			TotalSubmitted:   len(txHashes),
			Confirmed:        0,
			Failed:           len(txHashes) - len(confirmedHashes),
			Pending:          len(txHashes),
			ElapsedTime:      elapsedTime,
			GenerationTime:   genTime,
			SubmissionTime:   subTime,
			ConfirmationTime: confirmationTime,
			SubmissionRate:   submissionRate,
		}
	}

	r.log.Info("Found transactions in blocks", "blockCount", len(blockTxCounts))

	// Get details for each block that contains our transactions
	blockReports := make([]BlockReport, 0, len(blockTxCounts))
	for blockNum, ourTxCount := range blockTxCounts {
		report := r.trackBlock(blockNum, ourTxCount)
		if report != nil {
			blockReports = append(blockReports, *report)
		}
	}

	// Sort by block number
	sort.Slice(blockReports, func(i, j int) bool {
		return blockReports[i].BlockNumber < blockReports[j].BlockNumber
	})

	// Fill in empty blocks between first and last block
	if len(blockReports) > 1 {
		blockReports = r.fillEmptyBlocks(blockReports)
	}

	// Calculate overall TPS based on block timespan
	overallTPS := r.calculateOverallTPS(blockReports, len(confirmedHashes), elapsedTime)

	// Calculate block statistics (only for non-empty blocks)
	var totalTxInBlocks, minTx, maxTx int
	var nonEmptyBlocks int
	if len(blockReports) > 0 {
		// Find first non-empty block for initial min/max
		for _, report := range blockReports {
			if report.TxCount > 0 {
				minTx = report.TxCount
				maxTx = report.TxCount
				break
			}
		}

		for _, report := range blockReports {
			if report.TxCount > 0 {
				nonEmptyBlocks++
				totalTxInBlocks += report.TxCount
				if report.TxCount < minTx {
					minTx = report.TxCount
				}
				if report.TxCount > maxTx {
					maxTx = report.TxCount
				}
			}
		}
	}

	// Calculate average across ALL blocks (including empty)
	avgTxPerBlock := 0.0
	if len(blockReports) > 0 {
		avgTxPerBlock = float64(totalTxInBlocks) / float64(len(blockReports))
	}

	// Calculate average gas used percentage
	avgGasUsedPercent := 0.0
	if len(blockReports) > 0 {
		totalGasPercent := 0.0
		for _, report := range blockReports {
			totalGasPercent += float64(report.GasUsed) / float64(report.GasLimit) * 100
		}
		avgGasUsedPercent = totalGasPercent / float64(len(blockReports))
	}

	return &ReportData{
		TotalSubmitted:    len(txHashes),
		Confirmed:         len(confirmedHashes),
		Failed:            len(txHashes) - len(confirmedHashes),
		Pending:           0,
		ElapsedTime:       elapsedTime,
		GenerationTime:    genTime,
		SubmissionTime:    subTime,
		ConfirmationTime:  confirmationTime,
		BlockReports:      blockReports,
		OverallTPS:        overallTPS,
		SubmissionRate:    submissionRate,
		AvgTxPerBlock:     avgTxPerBlock,
		MinTxPerBlock:     minTx,
		MaxTxPerBlock:     maxTx,
		AvgGasUsedPercent: avgGasUsedPercent,
	}
}

// pollForReceipts polls for transaction receipts until all are confirmed or stalled
// Uses stall detection: exits if no new confirmations for StallTimeout seconds
// Returns confirmed hashes and their receipts (avoiding need to re-fetch)
func (r *Reporter) pollForReceipts(txHashes []common.Hash) ([]common.Hash, map[common.Hash]*types.Receipt) {
	// Log connection type for polling
	connType := "HTTP"
	if r.eth.IsWebSocket() {
		connType = "WebSocket"
	}
	r.log.Info("Polling for transaction receipts", "total", len(txHashes), "connectionType", connType)

	confirmedHashes := make([]common.Hash, 0)
	confirmedReceipts := make(map[common.Hash]*types.Receipt)
	pendingHashes := make([]common.Hash, len(txHashes))
	copy(pendingHashes, txHashes)

	pollStart := time.Now()
	lastProgressTime := time.Now()
	lastConfirmedCount := 0

	ticker := time.NewTicker(time.Duration(PollInterval) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		// Check if all confirmed
		if len(pendingHashes) == 0 {
			r.log.Info("All transactions confirmed", "total", len(confirmedHashes), "elapsed", time.Since(pollStart))
			return confirmedHashes, confirmedReceipts
		}

		// Check for stall (no progress for StallTimeout seconds)
		if time.Since(lastProgressTime) > time.Duration(StallTimeout)*time.Second {
			r.log.Warn("No new confirmations, assuming complete",
				"confirmed", len(confirmedHashes),
				"pending", len(pendingHashes),
				"stallTimeout", StallTimeout,
				"elapsed", time.Since(pollStart))
			return confirmedHashes, confirmedReceipts
		}

		// Check receipts in batches
		receipts, err := r.eth.BatchGetTransactionReceipts(pendingHashes)
		if err != nil {
			r.log.Error("Failed to fetch receipts", "error", err)
			continue
		}

		// Update confirmed and pending lists, cache receipts
		newPending := make([]common.Hash, 0)
		for _, hash := range pendingHashes {
			if receipt, found := receipts[hash]; found && receipt != nil {
				confirmedHashes = append(confirmedHashes, hash)
				confirmedReceipts[hash] = receipt // Cache the receipt
			} else {
				newPending = append(newPending, hash)
			}
		}
		pendingHashes = newPending

		// Check if we made progress (reset stall timer)
		if len(confirmedHashes) > lastConfirmedCount {
			lastProgressTime = time.Now()
			lastConfirmedCount = len(confirmedHashes)
		}

		r.log.Info("Polling progress",
			"confirmed", len(confirmedHashes),
			"pending", len(pendingHashes),
			"total", len(txHashes),
			"elapsed", time.Since(pollStart).Round(time.Second))
	}

	return confirmedHashes, confirmedReceipts
}

// findTransactionBlocksFromReceipts returns a map of block numbers to our transaction counts
// Uses already-fetched receipts instead of making another RPC call
func (r *Reporter) findTransactionBlocksFromReceipts(receipts map[common.Hash]*types.Receipt) map[uint64]int {
	blockTxCounts := make(map[uint64]int)

	if len(receipts) == 0 {
		return blockTxCounts
	}

	r.log.Info("Processing receipts", "total", len(receipts))

	for _, receipt := range receipts {
		if receipt != nil {
			blockTxCounts[receipt.BlockNumber.Uint64()]++
		}
	}

	return blockTxCounts
}

// trackBlock fetches block details and creates a BlockReport
func (r *Reporter) trackBlock(blockNum uint64, ourTxCount int) *BlockReport {
	// Get block header for gas/time info
	header, err := r.eth.GetBlockHeaderByNumber(blockNum)
	if err != nil {
		r.log.Error("Failed to get block header", "blockNumber", blockNum, "error", err)
		return nil
	}

	// Get transaction count using RPC call (works with custom tx types)
	totalTxCount, err := r.eth.GetBlockTransactionCount(blockNum)
	if err != nil {
		r.log.Warn("Failed to get block tx count", "blockNumber", blockNum, "error", err)
		// Fall back to 0 if we can't get the count
		totalTxCount = 0
	}

	// Calculate total transaction size in bytes
	var txSizeBytes uint64
	block, err := r.eth.GetBlockByNumber(blockNum)
	if err != nil {
		r.log.Debug("Failed to get full block for tx size calculation", "blockNumber", blockNum, "error", err)
		// Continue without tx size - not critical
	} else if block != nil {
		for _, tx := range block.Transactions() {
			txSizeBytes += tx.Size()
		}
	}

	blockTime := time.Unix(int64(header.Time), 0)

	return &BlockReport{
		BlockNumber:  blockNum,
		TxCount:      ourTxCount,
		TotalTxCount: totalTxCount,
		TxSizeBytes:  txSizeBytes,
		GasUsed:      header.GasUsed,
		GasLimit:     header.GasLimit,
		Timestamp:    blockTime,
	}
}

// fillEmptyBlocks fills in missing blocks between the first and last block to show empty blocks
func (r *Reporter) fillEmptyBlocks(blockReports []BlockReport) []BlockReport {
	if len(blockReports) < 2 {
		return blockReports
	}

	firstBlock := blockReports[0].BlockNumber
	lastBlock := blockReports[len(blockReports)-1].BlockNumber

	// Create a map of existing blocks for quick lookup
	existingBlocks := make(map[uint64]BlockReport)
	for _, report := range blockReports {
		existingBlocks[report.BlockNumber] = report
	}

	// Build new list with all blocks filled in
	completeReports := make([]BlockReport, 0, int(lastBlock-firstBlock)+1)
	emptyBlockCount := 0

	for blockNum := firstBlock; blockNum <= lastBlock; blockNum++ {
		if report, exists := existingBlocks[blockNum]; exists {
			// Block with our transactions
			completeReports = append(completeReports, report)
		} else {
			// Empty block - fetch its data
			emptyReport := r.trackBlock(blockNum, 0)
			if emptyReport != nil {
				completeReports = append(completeReports, *emptyReport)
				emptyBlockCount++
			}
		}
	}

	if emptyBlockCount > 0 {
		r.log.Info("Detected empty blocks", "emptyBlocks", emptyBlockCount, "totalBlocks", len(completeReports))
	}

	return completeReports
}

// calculateOverallTPS calculates the overall TPS based on block timespan
func (r *Reporter) calculateOverallTPS(blockReports []BlockReport, totalConfirmed int, elapsedWallTime time.Duration) float64 {
	if len(blockReports) == 0 || totalConfirmed == 0 {
		return 0
	}

	// If only one block, assume 1 second block time for throughput calculation
	if len(blockReports) == 1 {
		return float64(totalConfirmed) // Assume 1 second block time
	}

	// Calculate TPS based on first and last block timestamps
	firstBlock := blockReports[0]
	lastBlock := blockReports[len(blockReports)-1]

	timespan := lastBlock.Timestamp.Sub(firstBlock.Timestamp).Seconds()

	// If blocks have same timestamp, use wall time
	if timespan <= 0 {
		if elapsedWallTime.Seconds() > 0 {
			return float64(totalConfirmed) / elapsedWallTime.Seconds()
		}
		return float64(totalConfirmed) // Assume 1 second
	}

	// Add 1 second to account for the last block's duration
	// (timespan measures from start of first block to start of last block)
	timespan += 1.0

	return float64(totalConfirmed) / timespan
}

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

// PrintReport outputs the formatted report with improved UI
func (r *Reporter) PrintReport(data *ReportData) {
	// Header (91 chars wide to match table)
	fmt.Printf("\n%s╔%s╗%s\n", colorBold, strings.Repeat("═", 89), colorReset)
	fmt.Printf("%s║%s║%s\n", colorBold, centerText("RAILGUN BENCHMARK REPORT", 89), colorReset)
	fmt.Printf("%s╚%s╝%s\n\n", colorBold, strings.Repeat("═", 89), colorReset)

	// Summary Section
	successRate := float64(data.Confirmed) / float64(data.TotalSubmitted) * 100
	fmt.Printf("%s⚡ TRANSACTION SUMMARY%s\n", colorBold+colorCyan, colorReset)
	fmt.Println(strings.Repeat("━", 91))

	fmt.Printf("  Total Submitted:      %s%s%s\n", colorGreen, formatNumber(data.TotalSubmitted), colorReset)
	fmt.Printf("  Confirmed:            %s%s%s (%.1f%%)\n",
		colorGreen, formatNumber(data.Confirmed), colorReset, successRate)
	if data.Failed > 0 {
		fmt.Printf("  Failed:               %s%d%s\n", colorYellow, data.Failed, colorReset)
	}
	if data.Pending > 0 {
		fmt.Printf("  Pending:              %s%d%s\n", colorYellow, data.Pending, colorReset)
	}
	fmt.Println()

	// Timing Breakdown
	if data.GenerationTime > 0 || data.SubmissionTime > 0 {
		fmt.Printf("%s⏱️  TIMING BREAKDOWN%s\n", colorBold+colorCyan, colorReset)
		fmt.Println(strings.Repeat("━", 91))
		if data.GenerationTime > 0 {
			fmt.Printf("  Generation:           %s%s%s\n", colorGray, formatDuration(data.GenerationTime), colorReset)
		}
		if data.SubmissionTime > 0 {
			fmt.Printf("  Submission:           %s%s%s", colorGray, formatDuration(data.SubmissionTime), colorReset)
			if data.SubmissionRate > 0 {
				fmt.Printf(" %s(%.0f tx/s)%s", colorBlue, data.SubmissionRate, colorReset)
			}
			fmt.Println()
		}
		if data.ConfirmationTime > 0 {
			fmt.Printf("  Confirmation:         %s%s%s\n", colorGray, formatDuration(data.ConfirmationTime), colorReset)
		}
		fmt.Printf("  %sTotal Elapsed:        %s%.2fs%s%s\n",
			colorBold, colorGreen, data.ElapsedTime.Seconds(), colorReset, colorReset)
		fmt.Println()
	}

	// Block Report
	if len(data.BlockReports) > 0 {
		fmt.Printf("%s📊 BLOCK REPORT%s\n", colorBold+colorCyan, colorReset)
		fmt.Println(strings.Repeat("━", 91))

		// Table header (width: 2 + 8 + 2 + 10 + 2 + 10 + 2 + 12 + 2 + 9 + 2 + 20 + 2 + 8 = 91)
		fmt.Printf("  %8s  %10s  %10s  %12s  %9s  %-20s  %8s\n",
			"Block", "Our Txs", "Total Txs", "Tx Bytes", "Gas Used", "Gas Bar", "Time")
		fmt.Println("  " + strings.Repeat("─", 89))

		// Table rows
		for _, report := range data.BlockReports {
			gasPercent := float64(report.GasUsed) / float64(report.GasLimit) * 100

			// Color code gas usage and handle empty blocks
			var gasColor, txColor string
			if report.TxCount == 0 {
				// Empty block - use gray
				gasColor = colorGray
				txColor = colorGray
			} else {
				// Non-empty block - normal colors
				txColor = colorReset
				if gasPercent > 80 {
					gasColor = colorYellow
				} else if gasPercent > 50 {
					gasColor = colorBlue
				} else {
					gasColor = colorGreen
				}
			}

			// Create visual bar for gas usage
			barWidth := int(gasPercent / 5) // 20 chars max
			if barWidth > 20 {
				barWidth = 20
			}
			bar := strings.Repeat("█", barWidth) + strings.Repeat("░", 20-barWidth)

		// Display with appropriate colors - separate color codes from width format
		fmt.Printf("  %8d  %s%10s%s  %10s  %12s  %s%8.2f%%%s  %s%-20s%s  %8s\n",
			report.BlockNumber,
			txColor,
			formatNumber(report.TxCount),
			colorReset,
			formatNumber(report.TotalTxCount),
			formatNumber(int(report.TxSizeBytes)),
			gasColor,
			gasPercent,
			colorReset,
			colorGray,
			bar,
			colorReset,
			report.Timestamp.Format("15:04:05"))
		}

		fmt.Println("  " + strings.Repeat("─", 89))

		// Summary stats
		fmt.Printf("\n%s📈 STATISTICS%s\n", colorBold+colorCyan, colorReset)
		fmt.Println(strings.Repeat("━", 91))

		// Count empty blocks and total tx size
		emptyBlocks := 0
		nonEmptyBlocks := 0
		var totalTxSizeBytes uint64
		for _, report := range data.BlockReports {
			if report.TxCount == 0 {
				emptyBlocks++
			} else {
				nonEmptyBlocks++
			}
			totalTxSizeBytes += report.TxSizeBytes
		}

		fmt.Printf("  Total Blocks:         %s%d%s", colorGreen, len(data.BlockReports), colorReset)
		if emptyBlocks > 0 {
			fmt.Printf(" %s(%d empty)%s\n", colorGray, emptyBlocks, colorReset)
		} else {
			fmt.Println()
		}
		fmt.Printf("  Avg Tx/Block:         %s%.2f%s\n", colorBlue, data.AvgTxPerBlock, colorReset)
		if data.MinTxPerBlock > 0 {
			fmt.Printf("  Min Tx/Block:         %s%s%s\n", colorGray, formatNumber(data.MinTxPerBlock), colorReset)
			fmt.Printf("  Max Tx/Block:         %s%s%s\n", colorGray, formatNumber(data.MaxTxPerBlock), colorReset)
		}
		if data.AvgGasUsedPercent > 0 {
			fmt.Printf("  Avg Gas Usage:        %s%.2f%%%s\n", colorBlue, data.AvgGasUsedPercent, colorReset)
		}
		if totalTxSizeBytes > 0 {
			fmt.Printf("  Total Tx Bytes:       %s%s%s\n", colorBlue, formatNumber(int(totalTxSizeBytes)), colorReset)
		}
		fmt.Printf("\n  %s🚀 CHAIN THROUGHPUT:    %s%.2f TPS%s%s\n",
			colorBold, colorGreen, data.OverallTPS, colorReset, colorReset)
	}

	fmt.Printf("\n%s%s%s\n\n", colorGray, strings.Repeat("═", 91), colorReset)
}

// Helper functions for formatting
func formatNumber(n int) string {
	str := fmt.Sprintf("%d", n)
	if len(str) <= 3 {
		return str
	}
	// Add thousands separators
	result := ""
	for i, c := range str {
		if i > 0 && (len(str)-i)%3 == 0 {
			result += ","
		}
		result += string(c)
	}
	return result
}

func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func centerText(text string, width int) string {
	if len(text) >= width {
		return text
	}
	leftPad := (width - len(text)) / 2
	rightPad := width - len(text) - leftPad
	return strings.Repeat(" ", leftPad) + text + strings.Repeat(" ", rightPad)
}
