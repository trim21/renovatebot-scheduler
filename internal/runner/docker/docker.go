package docker

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
)

// Runner runs every Renovate invocation in its own container through the
// Docker Engine API. The scheduler itself is expected to run in a container
// with the Docker socket mounted.
type Runner struct {
	cfg    Config
	client *client.Client
}

func New(cfg Config) (*Runner, error) {
	cli, err := client.New(clientOptions(cfg.Host)...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Runner{cfg: cfg, client: cli}, nil
}

func clientOptions(host string) []client.Opt {
	if host == "" {
		return []client.Opt{client.FromEnv}
	}
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	return []client.Opt{client.WithHost(host)}
}

func (r *Runner) Run(ctx context.Context, req runner.RunRequest, log io.Writer) runner.RunResult {
	if err := r.ensureImage(ctx, log); err != nil {
		return runner.RunResult{ExitCode: -1, Err: err}
	}

	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     containerConfig(r.cfg, req),
		HostConfig: hostConfig(r.cfg.Mounts),
		Name:       containerName(req),
	})
	if err != nil {
		return runner.RunResult{ExitCode: -1, Err: fmt.Errorf("create container: %w", err)}
	}

	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		r.remove(created.ID, log)
		return runner.RunResult{ExitCode: -1, Err: fmt.Errorf("start container: %w", err)}
	}

	exitCode := -1
	wait := r.client.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-wait.Error:
		if err != nil {
			r.remove(created.ID, log)
			return runner.RunResult{ExitCode: -1, Err: fmt.Errorf("wait for container: %w", err)}
		}
	case resp := <-wait.Result:
		exitCode = int(resp.StatusCode)
	case <-ctx.Done():
		r.remove(created.ID, log)
		return runner.RunResult{ExitCode: -1, Err: fmt.Errorf("wait for container: %w", ctx.Err())}
	}

	r.copyLogs(created.ID, log)
	r.remove(created.ID, log)

	return runner.RunResult{ExitCode: exitCode}
}

func containerConfig(cfg Config, req runner.RunRequest) *container.Config {
	return &container.Config{
		Image: cfg.Image,
		Cmd:   []string{req.Repo},
		Env: []string{
			"RENOVATE_PLATFORM=forgejo",
			"RENOVATE_ENDPOINT=" + cfg.Endpoint,
			"RENOVATE_TOKEN=" + cfg.Token,
			"LOG_LEVEL=" + cfg.LogLevel,
		},
		Labels: map[string]string{
			"renovate-scheduler.repo": req.Repo,
			"renovate-scheduler.run":  req.RunID,
		},
	}
}

func hostConfig(mounts []Mount) *container.HostConfig {
	return &container.HostConfig{
		Mounts: toDockerMounts(mounts),
		// Logs are read back through the API, so the daemon does not need to
		// keep a second copy in its own log driver.
		LogConfig: container.LogConfig{Type: "none"},
	}
}

func toDockerMounts(mounts []Mount) []mount.Mount {
	out := make([]mount.Mount, 0, len(mounts))
	for _, m := range mounts {
		mountType := mount.TypeVolume
		if m.Type == MountBind {
			mountType = mount.TypeBind
		}
		out = append(out, mount.Mount{
			Type:     mountType,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

func containerName(req runner.RunRequest) string {
	sum := sha1.Sum([]byte(req.Repo))
	return fmt.Sprintf("renovate-%s-%s", hex.EncodeToString(sum[:])[:10], strings.ToLower(req.RunID))
}

func (r *Runner) ensureImage(ctx context.Context, log io.Writer) error {
	switch r.cfg.PullPolicy {
	case PullNever:
		return nil
	case PullMissing:
		if _, err := r.client.ImageInspect(ctx, r.cfg.Image); err == nil {
			return nil
		}
	}

	_, _ = fmt.Fprintf(log, "# pulling image %s\n", r.cfg.Image)
	resp, err := r.client.ImagePull(ctx, r.cfg.Image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", r.cfg.Image, err)
	}
	defer func() { _ = resp.Close() }()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull image %s: %w", r.cfg.Image, err)
	}
	return nil
}

// copyLogs reads the container output after it stopped. It uses its own context
// because the run context may already be cancelled by a timeout.
func (r *Runner) copyLogs(id string, w io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := r.client.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "# read container logs: %v\n", err)
		return
	}
	defer func() { _ = stream.Close() }()

	if _, err := stdcopy.StdCopy(w, w, stream); err != nil {
		_, _ = fmt.Fprintf(w, "# demultiplex container logs: %v\n", err)
	}
}

func (r *Runner) remove(id string, w io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := r.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
		_, _ = fmt.Fprintf(w, "# remove container: %v\n", err)
	}
}
