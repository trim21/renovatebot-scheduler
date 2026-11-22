package app

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

// logger is set once during startup, before any background goroutine starts.
var logger = zerolog.Nop()

func newLogger() zerolog.Logger {
	return zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.RFC3339,
		NoColor:    true,
	}).With().Timestamp().Logger()
}
