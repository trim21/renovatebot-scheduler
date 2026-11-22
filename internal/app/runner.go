package app

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
	"github.com/trim21/renovatebot-scheduler/internal/runner/docker"
	"github.com/trim21/renovatebot-scheduler/internal/runner/system"
)

// maxLogBytes caps the captured log kept in memory and written to SQLite. The
// tail is kept because the useful part of a failed run is usually at the end.
const maxLogBytes = 16 << 20

// tailWriter keeps at most max bytes, dropping the oldest data once the limit
// is reached.
type tailWriter struct {
	mu        sync.Mutex
	max       int
	buf       []byte
	truncated bool
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	if over := len(w.buf) - w.max; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
		w.truncated = true
	}
	return len(p), nil
}

func (w *tailWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.truncated {
		return bytes.Clone(w.buf)
	}
	marker := []byte("# log truncated, showing the last bytes only\n")
	return append(marker, w.buf...)
}

// Executor runs Renovate and records the run in the store. The scheduler
// depends only on this interface, so the execution backend stays replaceable.
//
// A returned RunResult.Err means the run could not be executed at all (image
// pull failure, Docker socket unreachable, timeout). A non-zero ExitCode with a
// nil Err means Renovate ran and failed on its own.
type Executor interface {
	Run(ctx context.Context, req runner.RunRequest) runner.RunResult
}

type executor struct {
	backend runner.Runner
	store   *Store
	timeout time.Duration
}

func (e *executor) Run(ctx context.Context, req runner.RunRequest) runner.RunResult {
	runID := newRunID()
	req.RunID = runID

	started := time.Now()
	logWriter := &tailWriter{max: maxLogBytes}
	_, _ = fmt.Fprintf(logWriter, "# repo: %s\n# trigger: %s\n# attempt: %d\n# started: %s\n\n",
		req.Repo, req.Trigger, req.Attempt, started.UTC().Format(time.RFC3339))

	runCtx, cancel := context.WithTimeout(ctx, e.timeout)
	result := e.backend.Run(runCtx, req, logWriter)
	cancel()

	finished := time.Now()
	record := RunRecord{
		ID:         runID,
		Repo:       req.Repo,
		Trigger:    req.Trigger,
		Attempt:    req.Attempt,
		StartedAt:  started,
		FinishedAt: finished,
		ExitCode:   result.ExitCode,
		Error:      errorString(result.Err),
		Log:        logWriter.Bytes(),
	}
	if err := e.store.AppendRun(record); err != nil {
		logger.Error().Err(err).Str("repo", req.Repo).Str("run", runID).Msg("record run")
	}

	if result.Err != nil {
		logger.Warn().Err(result.Err).
			Str("repo", req.Repo).
			Str("run", runID).
			Int("exit_code", result.ExitCode).
			Dur("duration", finished.Sub(started)).
			Msg("renovate run failed")
	} else {
		logger.Info().
			Str("repo", req.Repo).
			Str("run", runID).
			Int("exit_code", result.ExitCode).
			Dur("duration", finished.Sub(started)).
			Msg("renovate run finished")
	}

	return result
}

// NewRunner builds the execution backend selected by RUNNER.
func NewRunner(cfg *Config, store *Store) (Executor, error) {
	switch cfg.Runner {
	case runner.KindDocker:
		dockerCfg, err := docker.FromEnv()
		if err != nil {
			return nil, err
		}
		dockerCfg.Endpoint = cfg.ForgejoURL
		dockerCfg.Token = cfg.RenovateToken

		backend, err := docker.New(dockerCfg)
		if err != nil {
			return nil, err
		}
		return &executor{backend: backend, store: store, timeout: cfg.RunTimeout}, nil
	case runner.KindSystem:
		systemCfg := system.FromEnv()
		systemCfg.Endpoint = cfg.ForgejoURL
		systemCfg.Token = cfg.RenovateToken
		return &executor{backend: system.New(systemCfg), store: store, timeout: cfg.RunTimeout}, nil
	default:
		return nil, fmt.Errorf("unsupported RUNNER %q, supported values: %q, %q",
			cfg.Runner, runner.KindDocker, runner.KindSystem)
	}
}
