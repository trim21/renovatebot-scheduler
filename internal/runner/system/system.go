// Package system runs Renovate as a child process of the scheduler.
//
// It exists for deployments where the scheduler binary is copied into the
// Renovate image itself, so a run costs a process instead of a container.
package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
)

// Config holds the process runner settings. Endpoint and Token are injected by
// the caller; the rest can come from the environment through FromEnv.
type Config struct {
	Bin      string
	Endpoint string
	Token    string
	LogLevel string
	// ExtraConfig is written to a temporary file and referenced through
	// RENOVATE_CONFIG_FILE.
	ExtraConfig []byte
}

func FromEnv() Config {
	return Config{
		Bin:      getenv("RENOVATE_BIN", "renovate"),
		LogLevel: getenv("RENOVATE_LOG_LEVEL", "info"),
	}
}

type SystemRunner struct {
	cfg Config
}

func New(cfg Config) *SystemRunner {
	return &SystemRunner{cfg: cfg}
}

func (r *SystemRunner) Run(ctx context.Context, req runner.RunRequest, log io.Writer) runner.RunResult {
	_, _ = fmt.Fprintf(log, "# command: %s %s\n", r.cfg.Bin, req.Repo)

	overrides := map[string]string{
		"RENOVATE_PLATFORM": "forgejo",
		"RENOVATE_ENDPOINT": r.cfg.Endpoint,
		"RENOVATE_TOKEN":    r.cfg.Token,
		"LOG_LEVEL":         r.cfg.LogLevel,
	}

	if len(r.cfg.ExtraConfig) > 0 {
		path, cleanup, err := writeTempConfig(r.cfg.ExtraConfig)
		if err != nil {
			return runner.RunResult{ExitCode: -1, Err: err}
		}
		defer cleanup()
		overrides["RENOVATE_CONFIG_FILE"] = path
	}

	cmd := exec.CommandContext(ctx, r.cfg.Bin, req.Repo)
	cmd.Env = buildEnv(overrides)
	cmd.Stdout = log
	cmd.Stderr = log

	// Renovate spawns git and package manager children. Putting the run in its
	// own process group lets a timeout kill the whole tree instead of orphaning
	// the children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if err == nil {
		return runner.RunResult{ExitCode: 0}
	}
	if ctx.Err() != nil {
		return runner.RunResult{ExitCode: -1, Err: fmt.Errorf("run renovate: %w", ctx.Err())}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return runner.RunResult{ExitCode: exitErr.ExitCode()}
	}
	return runner.RunResult{ExitCode: -1, Err: fmt.Errorf("run renovate: %w", err)}
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// buildEnv returns the scheduler environment with overrides applied. Keys are
// dropped from the inherited environment first so the child never sees the
// same variable twice.
func buildEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[key]; ok {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func writeTempConfig(content []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "renovate-config-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("create renovate config: %w", err)
	}
	cleanup := func() { _ = os.Remove(file.Name()) }

	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write renovate config: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write renovate config: %w", err)
	}
	return file.Name(), cleanup, nil
}
