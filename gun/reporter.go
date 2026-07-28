package gun

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/sledro/railgun/ethereum"
)

// reportFileTimeFormat names saved reports by local date and time. Colons are
// avoided so the name is valid on every filesystem.
const reportFileTimeFormat = "2006-01-02_15-04-05"

// RunMetadata records the parameters that produced a report, so a saved file is
// self-describing rather than an anonymous wall of numbers.
type RunMetadata struct {
	StartedAt    time.Time
	RPCURL       string
	ChainID      int64
	TxCount      int64
	BatchSize    int64
	BatchDelay   int64
	Concurrency  int
	Accounts     int
	GasTipCapWei int64
	GasFeeCapWei int64
	Version      string
}

type metaRow struct {
	label string
	value string
}

func (m *RunMetadata) rows() []metaRow {
	gwei := func(wei int64) string {
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", float64(wei)/1e9), "0"), ".") + " gwei"
	}

	rows := []metaRow{
		{"Started", m.StartedAt.Format(time.RFC3339)},
		{"RPC", m.RPCURL},
		{"Chain ID", fmt.Sprintf("%d", m.ChainID)},
		{"Transactions", formatNumber(int(m.TxCount))},
		{"Batch Size", formatNumber(int(m.BatchSize))},
		{"Batch Delay", fmt.Sprintf("%dms", m.BatchDelay)},
		{"Concurrency", fmt.Sprintf("%d", m.Concurrency)},
		{"Sender Accounts", fmt.Sprintf("%d", m.Accounts)},
		{"Gas Tip Cap", gwei(m.GasTipCapWei)},
		{"Gas Fee Cap", gwei(m.GasFeeCapWei)},
	}

	if m.Version != "" {
		rows = append(rows, metaRow{"Railgun Version", m.Version})
	}

	return rows
}

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

// Block fetch retry constants. A load-balanced RPC can route a request to a node
// that has not yet imported a block we already have a receipt for, so a missing
// block is retried within a bounded budget rather than indefinitely.
const (
	blockFetchRetryBudget  = 3 * time.Second
	blockFetchRetryInitial = 250 * time.Millisecond
	blockFetchRetryMax     = 1 * time.Second
)

// ReportData contains all the data needed for the final report.
//
// Transaction accounting is a funnel:
//
//	TotalSubmitted = Accepted + SubmitFailed   (everything we tried to send)
//	Accepted       = Confirmed + Pending       (everything the node took)
type ReportData struct {
	TotalSubmitted    int // Transactions attempted
	Accepted          int // Accepted into the mempool by the node
	SubmitFailed      int // Rejected by the node at submission time
	Confirmed         int // Accepted and later found in a block
	Reverted          int // Confirmed but with a failed receipt status
	Pending           int // Accepted but still unconfirmed when polling ended
	ElapsedTime       time.Duration
	GenerationTime    time.Duration
	SubmissionTime    time.Duration // Wall-clock duration of the send phase
	NetworkTime       time.Duration // Sum of per-batch RPC durations (overlaps when concurrent)
	ConfirmationTime  time.Duration
	BlockReports      []BlockReport
	OverallTPS        float64
	BlockTimeSeconds  float64 // Measured block interval underpinning OverallTPS (0 if undetermined)
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

// GenerateReport polls for receipts and generates the final report.
// It takes the whole SendResult so that submission failures are carried into
// the report rather than being inferred from the accepted hashes alone.
func (r *Reporter) GenerateReport(ctx context.Context, sendResult *SendResult, startTime time.Time, genTime time.Duration) *ReportData {
	txHashes := sendResult.TxHashes

	// Poll for receipts
	pollStart := time.Now()
	confirmedHashes, receipts := r.pollForReceipts(ctx, txHashes)
	confirmationTime := time.Since(pollStart)

	// Calculate elapsed time
	elapsedTime := time.Since(startTime)

	accepted := len(txHashes)
	confirmed := len(confirmedHashes)

	// A receipt only proves inclusion, not success. Count reverts separately so
	// a run of failing transactions cannot masquerade as healthy throughput.
	reverted := 0
	for _, receipt := range receipts {
		if receipt != nil && receipt.Status == types.ReceiptStatusFailed {
			reverted++
		}
	}
	if reverted > 0 {
		r.log.Warn("Some confirmed transactions reverted", "reverted", reverted, "confirmed", confirmed)
	}

	// Submission rate must use wall-clock time. NetworkTime is a sum across
	// concurrent goroutines and would understate the rate by ~concurrency.
	submissionRate := 0.0
	if sendResult.WallTime.Seconds() > 0 {
		submissionRate = float64(accepted) / sendResult.WallTime.Seconds()
	}

	data := &ReportData{
		TotalSubmitted:   accepted + sendResult.Failed,
		Accepted:         accepted,
		SubmitFailed:     sendResult.Failed,
		Confirmed:        confirmed,
		Reverted:         reverted,
		Pending:          accepted - confirmed,
		ElapsedTime:      elapsedTime,
		GenerationTime:   genTime,
		SubmissionTime:   sendResult.WallTime,
		NetworkTime:      sendResult.NetworkTime,
		ConfirmationTime: confirmationTime,
		SubmissionRate:   submissionRate,
	}

	// Find which blocks contain our transactions (using already-fetched receipts)
	blockTxCounts := r.findTransactionBlocksFromReceipts(receipts)

	if len(blockTxCounts) == 0 {
		r.log.Warn("No transaction receipts found - transactions may not have been mined")
		return data
	}

	r.log.Info("Found transactions in blocks", "blockCount", len(blockTxCounts))

	// Get details for each block that contains our transactions
	blockReports := make([]BlockReport, 0, len(blockTxCounts))
	for blockNum, ourTxCount := range blockTxCounts {
		report := r.trackBlock(ctx, blockNum, ourTxCount, true)
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
		blockReports = r.fillEmptyBlocks(ctx, blockReports)
	}

	// Calculate overall TPS based on block timespan
	overallTPS, blockTime := r.calculateOverallTPS(ctx, blockReports, len(confirmedHashes), elapsedTime)

	// Calculate block statistics (only for non-empty blocks)
	var totalTxInBlocks, minTx, maxTx int
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
			totalGasPercent += gasUsedPercent(report)
		}
		avgGasUsedPercent = totalGasPercent / float64(len(blockReports))
	}

	data.BlockReports = blockReports
	data.OverallTPS = overallTPS
	data.BlockTimeSeconds = blockTime
	data.AvgTxPerBlock = avgTxPerBlock
	data.MinTxPerBlock = minTx
	data.MaxTxPerBlock = maxTx
	data.AvgGasUsedPercent = avgGasUsedPercent

	return data
}

// gasUsedPercent returns a block's gas utilisation, guarding against a zero
// gas limit (which some chains report for empty or synthetic blocks).
func gasUsedPercent(report BlockReport) float64 {
	if report.GasLimit == 0 {
		return 0
	}
	return float64(report.GasUsed) / float64(report.GasLimit) * 100
}

// pollForReceipts polls for transaction receipts until all are confirmed or stalled
// Uses stall detection: exits if no new confirmations for StallTimeout seconds
// Returns confirmed hashes and their receipts (avoiding need to re-fetch)
func (r *Reporter) pollForReceipts(ctx context.Context, txHashes []common.Hash) ([]common.Hash, map[common.Hash]*types.Receipt) {
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

	for {
		select {
		case <-ctx.Done():
			// Interrupted: return what we have so a partial report can still print.
			r.log.Warn("Receipt polling cancelled",
				"confirmed", len(confirmedHashes),
				"pending", len(pendingHashes),
				"elapsed", time.Since(pollStart))
			return confirmedHashes, confirmedReceipts
		case <-ticker.C:
		}

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
		receipts, err := r.eth.BatchGetTransactionReceipts(ctx, pendingHashes)
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

// fetchBlockHeader gets a block header, optionally retrying while the node
// reports it as missing. The retry budget is bounded so a genuinely absent block
// cannot stall the report.
func (r *Reporter) fetchBlockHeader(ctx context.Context, blockNum uint64, retryOnMissing bool) (*types.Header, error) {
	header, err := r.eth.GetBlockHeaderByNumber(ctx, blockNum)
	if err == nil || !retryOnMissing || !ethereum.IsNotFound(err) {
		return header, err
	}

	deadline := time.Now().Add(blockFetchRetryBudget)
	delay := blockFetchRetryInitial

	for time.Now().Before(deadline) {
		r.log.Debug("Block header not found, retrying", "blockNumber", blockNum, "delay", delay)
		if sleepErr := ethereum.SleepCtx(ctx, delay); sleepErr != nil {
			return header, err
		}

		header, err = r.eth.GetBlockHeaderByNumber(ctx, blockNum)
		if err == nil || !ethereum.IsNotFound(err) {
			return header, err
		}

		if delay < blockFetchRetryMax {
			delay *= 2
		}
	}

	return header, err
}

// trackBlock fetches block details and creates a BlockReport.
// retryOnMissing should be true only for blocks we know contain our
// transactions; backfilled empty blocks are fetched without retries so a long
// gap cannot multiply the retry budget across hundreds of blocks.
func (r *Reporter) trackBlock(ctx context.Context, blockNum uint64, ourTxCount int, retryOnMissing bool) *BlockReport {
	header, err := r.fetchBlockHeader(ctx, blockNum, retryOnMissing)
	if err != nil {
		r.log.Error("Failed to get block header", "blockNumber", blockNum, "error", err)
		return nil
	}

	// Get transaction count using RPC call (works with custom tx types)
	totalTxCount, err := r.eth.GetBlockTransactionCount(ctx, blockNum)
	if err != nil {
		r.log.Warn("Failed to get block tx count", "blockNumber", blockNum, "error", err)
		// Fall back to 0 if we can't get the count
		totalTxCount = 0
	}

	// Calculate total transaction size in bytes
	var txSizeBytes uint64
	block, err := r.eth.GetBlockByNumber(ctx, blockNum)
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
func (r *Reporter) fillEmptyBlocks(ctx context.Context, blockReports []BlockReport) []BlockReport {
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
			// Empty block - fetch its data without retries
			emptyReport := r.trackBlock(ctx, blockNum, 0, false)
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

// calculateOverallTPS derives throughput from block timestamps, which describe
// what the chain actually did rather than how long this client took.
//
// The block interval is measured from the observed blocks instead of assumed, so
// the result is correct on chains that do not produce one block per second.
func (r *Reporter) calculateOverallTPS(ctx context.Context, blockReports []BlockReport, totalConfirmed int, elapsedWallTime time.Duration) (tps float64, blockTime float64) {
	if len(blockReports) == 0 || totalConfirmed == 0 {
		return 0, 0
	}

	blockTime = r.estimateBlockTime(ctx, blockReports)

	firstBlock := blockReports[0]
	lastBlock := blockReports[len(blockReports)-1]
	timespan := lastBlock.Timestamp.Sub(firstBlock.Timestamp).Seconds()

	return overallTPSFromWindow(totalConfirmed, timespan, blockTime, elapsedWallTime), blockTime
}

// overallTPSFromWindow is the pure arithmetic behind calculateOverallTPS.
//
// timespan runs from the first block's timestamp to the last block's, so it
// excludes the last block's own window; one block interval is added to cover it.
// Block timestamps have one-second resolution, so a chain producing more than one
// block per second can report a zero timespan, in which case wall-clock elapsed
// time is the only usable denominator.
func overallTPSFromWindow(totalConfirmed int, timespan, blockTime float64, elapsedWallTime time.Duration) float64 {
	if totalConfirmed == 0 {
		return 0
	}

	window := timespan + blockTime
	if window <= 0 {
		if elapsedWallTime.Seconds() > 0 {
			return float64(totalConfirmed) / elapsedWallTime.Seconds()
		}
		return 0
	}

	return float64(totalConfirmed) / window
}

// estimateBlockTime infers the chain's block interval from the observed blocks,
// falling back to the block preceding the range when the observed blocks do not
// span enough time to measure. Returns 0 when no interval can be determined.
func (r *Reporter) estimateBlockTime(ctx context.Context, blockReports []BlockReport) float64 {
	if len(blockReports) >= 2 {
		first := blockReports[0]
		last := blockReports[len(blockReports)-1]

		if span := last.Timestamp.Sub(first.Timestamp).Seconds(); span > 0 {
			return span / float64(len(blockReports)-1)
		}
	}

	// A single block, or every block sharing a timestamp: ask the chain directly
	// by comparing against the block before our range.
	target := blockReports[0].BlockNumber
	if target == 0 {
		return 0
	}

	prev, err := r.eth.GetBlockHeaderByNumber(ctx, target-1)
	if err != nil {
		r.log.Debug("Could not derive block time from preceding block", "blockNumber", target-1, "error", err)
		return 0
	}

	interval := blockReports[0].Timestamp.Sub(time.Unix(int64(prev.Time), 0)).Seconds()
	if interval <= 0 {
		return 0
	}

	return interval
}

// palette holds the escape sequences used to colour the report. Every field is
// empty in plainPalette, so the same rendering code produces a colour terminal
// report or a clean text file. Colour codes are always kept out of the width
// specifiers so alignment is identical either way.
type palette struct {
	reset  string
	bold   string
	green  string
	yellow string
	blue   string
	cyan   string
	gray   string
}

var colorPalette = palette{
	reset:  "\033[0m",
	bold:   "\033[1m",
	green:  "\033[32m",
	yellow: "\033[33m",
	blue:   "\033[34m",
	cyan:   "\033[36m",
	gray:   "\033[90m",
}

// plainPalette renders the report without any escape sequences.
var plainPalette = palette{}

// PrintReport writes the formatted, coloured report to stdout.
func (r *Reporter) PrintReport(data *ReportData) {
	renderReport(os.Stdout, data, colorPalette, nil)
}

// SaveReport writes a plain-text copy of the report into dir, named for the run
// start time, and returns the path written. The directory is created if needed.
func (r *Reporter) SaveReport(dir string, data *ReportData, meta *RunMetadata) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create report directory %s: %w", dir, err)
	}

	stamp := meta.StartedAt
	if stamp.IsZero() {
		stamp = time.Now()
	}

	path, err := uniqueReportPath(dir, stamp)
	if err != nil {
		return "", err
	}

	// O_EXCL so a concurrent run cannot silently overwrite this file between the
	// name being chosen and the file being created.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("failed to create report file %s: %w", path, err)
	}
	defer f.Close()

	renderReport(f, data, plainPalette, meta)

	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("failed to flush report file %s: %w", path, err)
	}

	return path, nil
}

// uniqueReportPath returns a path in dir named for stamp, adding a numeric
// suffix if a report from the same second already exists.
func uniqueReportPath(dir string, stamp time.Time) (string, error) {
	base := stamp.Format(reportFileTimeFormat)

	for attempt := 0; attempt < 100; attempt++ {
		name := fmt.Sprintf("railgun-%s.txt", base)
		if attempt > 0 {
			name = fmt.Sprintf("railgun-%s-%d.txt", base, attempt+1)
		}

		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		}
	}

	return "", fmt.Errorf("could not find an unused report name in %s for %s", dir, base)
}

// renderReport writes the report to w. When meta is non-nil a run-parameter
// preamble is written first, so a saved report records what produced it.
func renderReport(w io.Writer, data *ReportData, p palette, meta *RunMetadata) {
	colorReset := p.reset
	colorBold := p.bold
	colorGreen := p.green
	colorYellow := p.yellow
	colorBlue := p.blue
	colorCyan := p.cyan
	colorGray := p.gray

	fmt.Fprintf(w, "\n%s╔%s╗%s\n", colorBold, strings.Repeat("═", 89), colorReset)
	fmt.Fprintf(w, "%s║%s║%s\n", colorBold, centerText("RAILGUN BENCHMARK REPORT", 89), colorReset)
	fmt.Fprintf(w, "%s╚%s╝%s\n\n", colorBold, strings.Repeat("═", 89), colorReset)

	if meta != nil {
		fmt.Fprintf(w, "%s🧾 RUN%s\n", colorBold+colorCyan, colorReset)
		fmt.Fprintln(w, strings.Repeat("━", 91))
		for _, row := range meta.rows() {
			fmt.Fprintf(w, "  %-21s %s%s%s\n", row.label+":", colorGray, row.value, colorReset)
		}
		fmt.Fprintln(w)
	}

	// Summary Section
	successRate := 0.0
	if data.TotalSubmitted > 0 {
		successRate = float64(data.Confirmed) / float64(data.TotalSubmitted) * 100
	}
	fmt.Fprintf(w, "%s⚡ TRANSACTION SUMMARY%s\n", colorBold+colorCyan, colorReset)
	fmt.Fprintln(w, strings.Repeat("━", 91))

	fmt.Fprintf(w, "  Total Submitted:      %s%s%s\n", colorGreen, formatNumber(data.TotalSubmitted), colorReset)
	if data.SubmitFailed > 0 {
		fmt.Fprintf(w, "  Accepted:             %s%s%s\n", colorGreen, formatNumber(data.Accepted), colorReset)
		fmt.Fprintf(w, "  Rejected on Submit:   %s%s%s\n", colorYellow, formatNumber(data.SubmitFailed), colorReset)
	}
	fmt.Fprintf(w, "  Confirmed:            %s%s%s (%.1f%%)\n",
		colorGreen, formatNumber(data.Confirmed), colorReset, successRate)
	if data.Reverted > 0 {
		fmt.Fprintf(w, "  Reverted:             %s%s%s\n", colorYellow, formatNumber(data.Reverted), colorReset)
	}
	if data.Pending > 0 {
		fmt.Fprintf(w, "  Unconfirmed:          %s%s%s\n", colorYellow, formatNumber(data.Pending), colorReset)
	}
	fmt.Fprintln(w)

	// Timing Breakdown
	if data.GenerationTime > 0 || data.SubmissionTime > 0 {
		fmt.Fprintf(w, "%s⏱️  TIMING BREAKDOWN%s\n", colorBold+colorCyan, colorReset)
		fmt.Fprintln(w, strings.Repeat("━", 91))
		if data.GenerationTime > 0 {
			fmt.Fprintf(w, "  Generation:           %s%s%s\n", colorGray, formatDuration(data.GenerationTime), colorReset)
		}
		if data.SubmissionTime > 0 {
			fmt.Fprintf(w, "  Submission:           %s%s%s", colorGray, formatDuration(data.SubmissionTime), colorReset)
			if data.SubmissionRate > 0 {
				fmt.Fprintf(w, " %s(%.0f tx/s)%s", colorBlue, data.SubmissionRate, colorReset)
			}
			fmt.Fprintln(w)
		}
		if data.ConfirmationTime > 0 {
			fmt.Fprintf(w, "  Confirmation:         %s%s%s\n", colorGray, formatDuration(data.ConfirmationTime), colorReset)
		}
		fmt.Fprintf(w, "  %sTotal Elapsed:        %s%.2fs%s%s\n",
			colorBold, colorGreen, data.ElapsedTime.Seconds(), colorReset, colorReset)
		fmt.Fprintln(w)
	}

	// Block Report
	if len(data.BlockReports) > 0 {
		fmt.Fprintf(w, "%s📊 BLOCK REPORT%s\n", colorBold+colorCyan, colorReset)
		fmt.Fprintln(w, strings.Repeat("━", 91))

		// Table header (width: 2 + 8 + 2 + 10 + 2 + 10 + 2 + 12 + 2 + 9 + 2 + 20 + 2 + 8 = 91)
		fmt.Fprintf(w, "  %8s  %10s  %10s  %12s  %9s  %-20s  %8s\n",
			"Block", "Our Txs", "Total Txs", "Tx Bytes", "Gas Used", "Gas Bar", "Time")
		fmt.Fprintln(w, "  "+strings.Repeat("─", 89))

		// Table rows
		for _, report := range data.BlockReports {
			gasPercent := gasUsedPercent(report)

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
			fmt.Fprintf(w, "  %8d  %s%10s%s  %10s  %12s  %s%8.2f%%%s  %s%-20s%s  %8s\n",
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

		fmt.Fprintln(w, "  "+strings.Repeat("─", 89))

		// Summary stats
		fmt.Fprintf(w, "\n%s📈 STATISTICS%s\n", colorBold+colorCyan, colorReset)
		fmt.Fprintln(w, strings.Repeat("━", 91))

		// Count empty blocks and total tx size
		emptyBlocks := 0
		var totalTxSizeBytes uint64
		for _, report := range data.BlockReports {
			if report.TxCount == 0 {
				emptyBlocks++
			}
			totalTxSizeBytes += report.TxSizeBytes
		}

		fmt.Fprintf(w, "  Total Blocks:         %s%d%s", colorGreen, len(data.BlockReports), colorReset)
		if emptyBlocks > 0 {
			fmt.Fprintf(w, " %s(%d empty)%s\n", colorGray, emptyBlocks, colorReset)
		} else {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "  Avg Tx/Block:         %s%.2f%s\n", colorBlue, data.AvgTxPerBlock, colorReset)
		if data.MinTxPerBlock > 0 {
			fmt.Fprintf(w, "  Min Tx/Block:         %s%s%s\n", colorGray, formatNumber(data.MinTxPerBlock), colorReset)
			fmt.Fprintf(w, "  Max Tx/Block:         %s%s%s\n", colorGray, formatNumber(data.MaxTxPerBlock), colorReset)
		}
		if data.AvgGasUsedPercent > 0 {
			fmt.Fprintf(w, "  Avg Gas Usage:        %s%.2f%%%s\n", colorBlue, data.AvgGasUsedPercent, colorReset)
		}
		if totalTxSizeBytes > 0 {
			fmt.Fprintf(w, "  Total Tx Bytes:       %s%s%s\n", colorBlue, formatNumber(int(totalTxSizeBytes)), colorReset)
		}
		if data.BlockTimeSeconds > 0 {
			fmt.Fprintf(w, "  Measured Block Time:  %s%.2fs%s\n", colorGray, data.BlockTimeSeconds, colorReset)
		}
		fmt.Fprintf(w, "\n  %s🚀 CHAIN THROUGHPUT:    %s%.2f TPS%s%s\n",
			colorBold, colorGreen, data.OverallTPS, colorReset, colorReset)
	}

	fmt.Fprintf(w, "\n%s%s%s\n\n", colorGray, strings.Repeat("═", 91), colorReset)
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
