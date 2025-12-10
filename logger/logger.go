package logger

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/lmittmann/tint"
)

var (
	// DefaultLogger is the default logger instance
	DefaultLogger *slog.Logger
	// currentLevel stores the current log level for use in New()
	currentLevel = slog.LevelInfo
)

func init() {
	// Initialize the default logger
	DefaultLogger = slog.New(
		tint.NewHandler(os.Stderr, &tint.Options{
			Level: slog.LevelInfo,
		}),
	)
	slog.SetDefault(DefaultLogger)
}

// SetLevel reinitializes the default logger with the specified level
func SetLevel(level string) error {
	lvl, err := ParseLevel(level)
	if err != nil {
		return err
	}
	currentLevel = lvl
	DefaultLogger = slog.New(
		tint.NewHandler(os.Stderr, &tint.Options{
			Level: lvl,
		}),
	)
	slog.SetDefault(DefaultLogger)
	return nil
}

// ParseLevel converts a string level to slog.Level
func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level: %s (valid: debug, info, warn, error)", level)
	}
}

// New creates a new logger with the given options
func New(opts *tint.Options) *slog.Logger {
	if opts == nil {
		opts = &tint.Options{
			Level: currentLevel,
		}
	}
	return slog.New(tint.NewHandler(os.Stderr, opts))
}
