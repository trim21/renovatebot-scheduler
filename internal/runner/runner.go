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

// Kind selects the execution backend through the RUNNER environment variable.
type Kind string

const (
	KindDocker Kind = "docker"
	KindSystem Kind = "system"
)
