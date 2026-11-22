// Package runner defines the execution backend contract for Renovate runs.
//
// A backend only has to execute Renovate for one repository and write its
// output to the provided writer. Persistence of run history and log files is
// the caller's concern, so backends stay free of storage dependencies.
package runner

import (
	"context"
	"io"
)

type RunRequest struct {
	// RunID identifies the run; backends may use it for container or job names.
	RunID   string
	Repo    string
	Trigger string
	Attempt int
}

type RunResult struct {
	ExitCode int
	Err      error
}

// Runner executes Renovate for a single repository. Implementations must be
// safe for concurrent use.
type Runner interface {
	Run(ctx context.Context, req RunRequest, log io.Writer) RunResult
}

// Preparer is implemented by runners that need one-time setup before the first
// run, such as pulling the image or fixing mount ownership. Prepare is
// best-effort: the runner must still work if it is never called.
type Preparer interface {
	Prepare(ctx context.Context, log io.Writer) error
}

// Kind selects the execution backend through the RUNNER environment variable.
type Kind string

const (
	KindDocker Kind = "docker"
	KindSystem Kind = "system"
)
