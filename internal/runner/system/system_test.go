package system

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
)

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "renovate")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunPassesArgumentsAndEnv(t *testing.T) {
	script := writeScript(t, `echo "repo=$1"
echo "endpoint=$RENOVATE_ENDPOINT"
echo "token=$RENOVATE_TOKEN"
echo "platform=$RENOVATE_PLATFORM"
echo "level=$LOG_LEVEL"
exit 3
`)

	r := New(Config{
		Bin:      script,
		Endpoint: "https://git.example.com",
		Token:    "secret",
		LogLevel: "debug",
	})

	var log bytes.Buffer
	result := r.Run(context.Background(), runner.RunRequest{Repo: "owner/repo"}, &log)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("unexpected exit code: %d", result.ExitCode)
	}
	for _, want := range []string{
		"repo=owner/repo",
		"endpoint=https://git.example.com",
		"token=secret",
		"platform=forgejo",
		"level=debug",
	} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log does not contain %q:\n%s", want, log.String())
		}
	}
}

func TestRunPassesExtraConfig(t *testing.T) {
	script := writeScript(t, `echo "config=$RENOVATE_CONFIG_FILE"
cat "$RENOVATE_CONFIG_FILE"
`)

	config := []byte(`{"extends":["config:recommended"]}`)
	r := New(Config{Bin: script, ExtraConfig: config})

	var log bytes.Buffer
	result := r.Run(context.Background(), runner.RunRequest{Repo: "owner/repo"}, &log)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	output := log.String()
	if !strings.Contains(output, string(config)) {
		t.Fatalf("log does not contain the config:\n%s", output)
	}

	// The temporary file must be gone once the run finished.
	for _, line := range strings.Split(output, "\n") {
		if path, ok := strings.CutPrefix(line, "config="); ok && path != "" {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("temporary config %s was not removed: %v", path, err)
			}
		}
	}
}

func TestBuildEnvDropsOverriddenKeys(t *testing.T) {
	t.Setenv("RENOVATE_TOKEN", "inherited")
	t.Setenv("KEEP_ME", "yes")

	env := buildEnv(map[string]string{"RENOVATE_TOKEN": "injected"})

	var tokens int
	for _, entry := range env {
		if strings.HasPrefix(entry, "RENOVATE_TOKEN=") {
			tokens++
			if entry != "RENOVATE_TOKEN=injected" {
				t.Fatalf("unexpected token entry: %q", entry)
			}
		}
	}
	if tokens != 1 {
		t.Fatalf("expected exactly one RENOVATE_TOKEN entry, got %d", tokens)
	}
	if !slices.Contains(env, "KEEP_ME=yes") {
		t.Fatal("unrelated environment variables were dropped")
	}
}

func TestRunReportsMissingBinary(t *testing.T) {
	r := New(Config{Bin: filepath.Join(t.TempDir(), "does-not-exist")})
	result := r.Run(context.Background(), runner.RunRequest{Repo: "owner/repo"}, &bytes.Buffer{})

	if result.Err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if result.ExitCode != -1 {
		t.Fatalf("unexpected exit code: %d", result.ExitCode)
	}
}

func TestRunKillsOnTimeout(t *testing.T) {
	script := writeScript(t, "sleep 30\n")

	r := New(Config{Bin: script})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := r.Run(ctx, runner.RunRequest{Repo: "owner/repo"}, &bytes.Buffer{})

	if result.Err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(result.Err.Error(), "context deadline exceeded") {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("run did not stop promptly: %s", elapsed)
	}
}
