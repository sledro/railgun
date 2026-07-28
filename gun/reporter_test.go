package gun

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestOverallTPSFromWindow(t *testing.T) {
	tests := []struct {
		name           string
		totalConfirmed int
		timespan       float64
		blockTime      float64
		wallTime       time.Duration
		want           float64
	}{
		{
			name:           "nothing confirmed",
			totalConfirmed: 0,
			timespan:       10,
			blockTime:      1,
			want:           0,
		},
		{
			// One block on a one-second chain: the block's own window is the
			// whole measurement period.
			name:           "single block one second chain",
			totalConfirmed: 60,
			timespan:       0,
			blockTime:      1,
			want:           60,
		},
		{
			// The same block count on a twelve-second chain is 12x slower.
			// The old code assumed a 1s block here and reported 60.
			name:           "single block twelve second chain",
			totalConfirmed: 60,
			timespan:       0,
			blockTime:      12,
			want:           5,
		},
		{
			// 5 blocks, 1s apart: timespan 4s covers the first four windows,
			// blockTime adds the last one, so 500 txs over 5s.
			name:           "multiple blocks one second apart",
			totalConfirmed: 500,
			timespan:       4,
			blockTime:      1,
			want:           100,
		},
		{
			name:           "multiple blocks twelve seconds apart",
			totalConfirmed: 500,
			timespan:       48,
			blockTime:      12,
			want:           500.0 / 60.0,
		},
		{
			// Sub-second blocks share a timestamp and no interval can be
			// derived, so wall clock is the only usable denominator.
			name:           "zero window falls back to wall clock",
			totalConfirmed: 100,
			timespan:       0,
			blockTime:      0,
			wallTime:       2 * time.Second,
			want:           50,
		},
		{
			name:           "zero window and no wall time yields zero",
			totalConfirmed: 100,
			timespan:       0,
			blockTime:      0,
			wallTime:       0,
			want:           0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := overallTPSFromWindow(tt.totalConfirmed, tt.timespan, tt.blockTime, tt.wallTime)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("overallTPSFromWindow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGasUsedPercent(t *testing.T) {
	tests := []struct {
		name   string
		report BlockReport
		want   float64
	}{
		{"half full", BlockReport{GasUsed: 15_000_000, GasLimit: 30_000_000}, 50},
		{"empty block", BlockReport{GasUsed: 0, GasLimit: 30_000_000}, 0},
		{"full block", BlockReport{GasUsed: 30_000_000, GasLimit: 30_000_000}, 100},
		// Guards against a NaN leaking into the report table.
		{"zero gas limit", BlockReport{GasUsed: 0, GasLimit: 0}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gasUsedPercent(tt.report)
			if math.IsNaN(got) {
				t.Fatal("gasUsedPercent returned NaN")
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("gasUsedPercent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatNumber(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
	}

	for _, tt := range tests {
		if got := formatNumber(tt.in); got != tt.want {
			t.Errorf("formatNumber(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCenterText(t *testing.T) {
	// The report's box drawing depends on the result being exactly `width` wide.
	for _, width := range []int{10, 25, 89} {
		for _, text := range []string{"", "a", "RAILGUN BENCHMARK REPORT"} {
			got := centerText(text, width)
			if len(text) >= width {
				if got != text {
					t.Errorf("centerText(%q, %d) = %q, want the input unchanged", text, width, got)
				}
				continue
			}
			if len(got) != width {
				t.Errorf("centerText(%q, %d) produced width %d, want %d", text, width, len(got), width)
			}
		}
	}
}

func sampleReportData() *ReportData {
	return &ReportData{
		TotalSubmitted:   100,
		Accepted:         100,
		Confirmed:        100,
		ElapsedTime:      2 * time.Second,
		GenerationTime:   10 * time.Millisecond,
		SubmissionTime:   500 * time.Millisecond,
		SubmissionRate:   200,
		OverallTPS:       50,
		BlockTimeSeconds: 2,
		AvgTxPerBlock:    50,
		MinTxPerBlock:    50,
		MaxTxPerBlock:    50,
		BlockReports: []BlockReport{
			{BlockNumber: 10, TxCount: 50, TotalTxCount: 50, GasUsed: 1_050_000, GasLimit: 30_000_000, Timestamp: time.Unix(1700000000, 0)},
			{BlockNumber: 11, TxCount: 50, TotalTxCount: 50, GasUsed: 1_050_000, GasLimit: 30_000_000, Timestamp: time.Unix(1700000002, 0)},
		},
	}
}

func TestUniqueReportPathAvoidsCollisions(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Date(2026, 7, 28, 20, 26, 57, 0, time.UTC)

	first, err := uniqueReportPath(dir, stamp)
	if err != nil {
		t.Fatalf("uniqueReportPath: %v", err)
	}
	if got, want := filepath.Base(first), "railgun-2026-07-28_20-26-57.txt"; got != want {
		t.Errorf("first report name = %q, want %q", got, want)
	}

	// Once the first exists, a run in the same second must not reuse the name.
	if err := os.WriteFile(first, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := uniqueReportPath(dir, stamp)
	if err != nil {
		t.Fatalf("uniqueReportPath: %v", err)
	}
	if second == first {
		t.Fatal("second report reused the first path, which would overwrite it")
	}
	if got, want := filepath.Base(second), "railgun-2026-07-28_20-26-57-2.txt"; got != want {
		t.Errorf("second report name = %q, want %q", got, want)
	}
}

func TestSaveReportWritesPlainText(t *testing.T) {
	// A nested path also exercises directory creation.
	dir := filepath.Join(t.TempDir(), "nested", "reports")
	r := &Reporter{log: discardLogger()}

	meta := &RunMetadata{
		StartedAt:    time.Date(2026, 7, 28, 20, 26, 57, 0, time.UTC),
		RPCURL:       "http://127.0.0.1:8545",
		ChainID:      31337,
		TxCount:      100,
		BatchSize:    20,
		BatchDelay:   10,
		Concurrency:  4,
		Accounts:     1,
		GasTipCapWei: 1_000_000_000,
		GasFeeCapWei: 20_000_000_000,
	}

	path, err := r.SaveReport(dir, sampleReportData(), meta)
	if err != nil {
		t.Fatalf("SaveReport: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved report: %v", err)
	}
	content := string(raw)

	// A saved report must be readable in any editor, so no escape sequences.
	if strings.Contains(content, "\033") {
		t.Error("saved report contains ANSI escape sequences")
	}

	// The preamble is what makes a timestamped file identifiable later.
	for _, want := range []string{
		"http://127.0.0.1:8545",
		"31337",
		"Chain ID",
		"Batch Delay",
		"1 gwei",
		"20 gwei",
		"RAILGUN BENCHMARK REPORT",
		"CHAIN THROUGHPUT",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("saved report is missing %q", want)
		}
	}
}

func TestRenderReportColorAndPlainAlignIdentically(t *testing.T) {
	data := sampleReportData()

	var colored, plain strings.Builder
	renderReport(&colored, data, colorPalette, nil)
	renderReport(&plain, data, plainPalette, nil)

	if !strings.Contains(colored.String(), "\033") {
		t.Error("colour rendering produced no escape sequences")
	}
	if strings.Contains(plain.String(), "\033") {
		t.Error("plain rendering produced escape sequences")
	}

	// Stripping the escapes from the coloured output must reproduce the plain
	// output exactly: colour must never affect column widths.
	stripped := ansiPattern.ReplaceAllString(colored.String(), "")
	if stripped != plain.String() {
		t.Error("plain output differs from the coloured output with escapes stripped")
	}
}

var ansiPattern = regexp.MustCompile("\033\\[[0-9;]*m")
