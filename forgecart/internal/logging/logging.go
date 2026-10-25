// Package logging builds the JSON logger every ForgeCart process uses. The level comes from
// FORGECART_LOG_LEVEL (debug, info, warn or error; default info). It exists because at full load a
// worker writes a line per job and the dispatcher one per event: about a megabyte a second, which
// would fill a small disk during a long run. warn keeps only problems.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// LevelEnv names the variable.
const LevelEnv = "FORGECART_LOG_LEVEL"

// New returns a JSON logger writing to w at the level in the environment.
func New(w io.Writer) (*slog.Logger, error) {
	level, err := parse(os.Getenv(LevelEnv))
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})), nil
}

func parse(v string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%s must be debug, info, warn or error", LevelEnv)
}
