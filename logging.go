package main

import (
	"log/slog"
	"os"
)

// logger is the process-wide structured logger. JSON output so Render's log
// pipeline (and any future log aggregator) can parse fields directly.
var logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
