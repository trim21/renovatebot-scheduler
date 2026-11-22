package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
)

// containerConfigPath is where ExtraConfig is written inside the container.
const containerConfigPath = "/tmp/renovate-config.json"

// defaultOwner is used when the image declares a USER that cannot be read as a
// numeric uid, which is the common case for distribution images.
const defaultOwner = "1000:1000"

// logDrainTimeout bounds how long Run waits for the log stream to finish after
// the container exited, so a stuck stream cannot block the run.
const logDrainTimeout = 10 * time.Second

// Runner runs every Renovate invocation in its own container through the
// Docker Engine API. The scheduler itself is expected to run in a container
// with the Docker socket mounted.
type Runner struct {
	cfg    Config
	client *client.Client

	permMu   sync.Mutex
	permDone map[string]bool

	ownerMu sync.Mutex
	owner   string
}

func New(cfg Config) (*Runner, error) {
	cli, err := client.New(clientOptions(cfg.Host)...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Runner{cfg: cfg, client: cli, permDone: make(map[string]bool)}, nil
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

// Prepare pulls the image if needed and fixes mount ownership, so the first
// webhook-triggered run does not pay for either. It is safe to call more than
// once: ownership is tracked per mount and the image check is cheap.
func (r *Runner) Prepare(ctx context.Context, log io.Writer) error {
	if err := r.ensureImage(ctx, log); err != nil {
		return err
	}
	return r.ensureMountOwnership(ctx, log)
}

func (r *Runner) Run(ctx context.Context, req runner.RunRequest, log io.Writer) runner.RunResult {
	if err := r.ensureImage(ctx, log); err != nil {
		return runner.RunResult{ExitCode: -1, Err: err}
	}

	if err := r.ensureMountOwnership(ctx, log); err != nil {
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

	if len(r.cfg.ExtraConfig) > 0 {
		if err := r.copyConfig(ctx, created.ID); err != nil {
			r.remove(created.ID, log)
			return runner.RunResult{ExitCode: -1, Err: err}
		}
		_, _ = fmt.Fprintf(log, "# renovate config: %d bytes\n", len(r.cfg.ExtraConfig))
	}

	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		r.remove(created.ID, log)
		return runner.RunResult{ExitCode: -1, Err: fmt.Errorf("start container: %w", err)}
	}

	// Stream logs while the container runs. Reading them only after the wait
	// would keep the writer empty until the run finished, so a caller that
	// persists output incrementally would see nothing until the very end.
	logsDone := make(chan error, 1)
	go func() {
		logsDone <- r.streamLogs(ctx, created.ID, log)
	}()

	exitCode := -1
	var waitErr error
	wait := r.client.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-wait.Error:
		if err != nil {
			waitErr = fmt.Errorf("wait for container: %w", err)
		}
	case resp := <-wait.Result:
		exitCode = int(resp.StatusCode)
	case <-ctx.Done():
		waitErr = fmt.Errorf("wait for container: %w", ctx.Err())
	}

	if waitErr != nil {
		// The container may still be running after a timeout, so stop it before
		// draining the log stream.
		r.remove(created.ID, log)
	}

	select {
	case err := <-logsDone:
		// A canceled context is expected when the run timed out; the timeout
		// itself is already reported through waitErr.
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			_, _ = fmt.Fprintf(log, "# stream container logs: %v\n", err)
		}
	case <-time.After(logDrainTimeout):
		_, _ = fmt.Fprintf(log, "# stream container logs: timed out draining output\n")
	}

	if waitErr != nil {
		return runner.RunResult{ExitCode: -1, Err: waitErr}
	}

	r.remove(created.ID, log)

	return runner.RunResult{ExitCode: exitCode}
}

// streamLogs copies the container's log stream into w until the container exits
// or the context is canceled.
func (r *Runner) streamLogs(ctx context.Context, id string, w io.Writer) error {
	stream, err := r.client.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return fmt.Errorf("read container logs: %w", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stdcopy.StdCopy(w, w, stream); err != nil {
		return fmt.Errorf("demultiplex container logs: %w", err)
	}
	return nil
}

func containerConfig(cfg Config, req runner.RunRequest) *container.Config {
	env := []string{
		"RENOVATE_PLATFORM=forgejo",
		"RENOVATE_ENDPOINT=" + cfg.Endpoint,
		"RENOVATE_TOKEN=" + cfg.Token,
		"LOG_LEVEL=" + cfg.LogLevel,
	}
	if len(cfg.ExtraConfig) > 0 {
		env = append(env, "RENOVATE_CONFIG_FILE="+containerConfigPath)
	}

	return &container.Config{
		Image: cfg.Image,
		Cmd:   []string{req.Repo},
		Env:   env,
		Labels: map[string]string{
			"renovate-scheduler.repo": req.Repo,
			"renovate-scheduler.run":  req.RunID,
		},
	}
}

// copyConfig writes ExtraConfig into the created container. The file has to
// exist before start, so the config travels with the container instead of
// requiring a host path that the scheduler container may not share.
func (r *Runner) copyConfig(ctx context.Context, containerID string) error {
	archive, err := configArchive(r.cfg.ExtraConfig)
	if err != nil {
		return err
	}

	_, err = r.client.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{
		DestinationPath: path.Dir(containerConfigPath),
		Content:         bytes.NewReader(archive),
	})
	if err != nil {
		return fmt.Errorf("copy renovate config into container: %w", err)
	}
	return nil
}

func configArchive(content []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)

	header := &tar.Header{
		Name: path.Base(containerConfigPath),
		// CopyToContainer writes as root; the Renovate image runs as uid 1000,
		// so a 0600 file would be unreadable and Renovate reports it as
		// "Error parsing config file". World-readable is fine: the file lives
		// only inside the short-lived container.
		Mode: 0o644,
		Size: int64(len(content)),
	}
	if err := writer.WriteHeader(header); err != nil {
		return nil, fmt.Errorf("build config archive: %w", err)
	}
	if _, err := writer.Write(content); err != nil {
		return nil, fmt.Errorf("build config archive: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("build config archive: %w", err)
	}
	return buf.Bytes(), nil
}

func hostConfig(mounts []Mount) *container.HostConfig {
	return &container.HostConfig{
		Mounts: toDockerMounts(mounts),
		// ContainerLogs reads whatever the logging driver stored, so the driver
		// has to be one that supports reading. "none" stores nothing and fails
		// the read; pinning json-file keeps the run log working regardless of
		// the daemon default. The container is removed right after the run, so
		// the daemon copy does not accumulate.
		LogConfig: container.LogConfig{Type: "json-file"},
	}
}

func toDockerMounts(mounts []Mount) []mount.Mount {
	out := make([]mount.Mount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, mount.Mount{
			Type:     toMountType(m.Type),
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

// ensureMountOwnership makes writable mounts usable by the Renovate user.
// Docker creates a missing bind source, and a fresh volume's root directory, as
// root, while the Renovate image runs as UID/GID 1000 — that mismatch is what
// produces EACCES on /tmp/renovate/cache. Each mount is fixed once per process.
func (r *Runner) ensureMountOwnership(ctx context.Context, log io.Writer) error {
	for _, m := range r.cfg.Mounts {
		if m.ReadOnly || r.mountOwnershipFixed(m.Source) {
			continue
		}

		if err := r.chownMount(ctx, m); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(log, "# fixed permissions of %s\n", m.Source)

		r.permMu.Lock()
		r.permDone[m.Source] = true
		r.permMu.Unlock()
	}
	return nil
}

func (r *Runner) mountOwnershipFixed(source string) bool {
	r.permMu.Lock()
	defer r.permMu.Unlock()
	return r.permDone[source]
}

// chownMount runs a one-off root container that hands the mount to the Renovate
// user. It reuses the Renovate image so no extra image has to be pulled, and
// runs without networking.
func (r *Runner) chownMount(ctx context.Context, m Mount) error {
	owner, err := r.mountOwner(ctx)
	if err != nil {
		return err
	}

	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      r.cfg.Image,
			User:       "0",
			Entrypoint: []string{"chown"},
			Cmd:        []string{"-R", owner, "/mnt"},
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{
				Type:   toMountType(m.Type),
				Source: m.Source,
				Target: "/mnt",
			}},
			NetworkMode: container.NetworkMode("none"),
		},
	})
	if err != nil {
		return fmt.Errorf("create permission fixer for %s: %w", m.Source, err)
	}
	defer r.remove(created.ID, io.Discard)

	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start permission fixer for %s: %w", m.Source, err)
	}

	wait := r.client.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-wait.Error:
		if err != nil {
			return fmt.Errorf("wait for permission fixer for %s: %w", m.Source, err)
		}
	case resp := <-wait.Result:
		if resp.StatusCode != 0 {
			return fmt.Errorf("chown %s: exit code %d", m.Source, resp.StatusCode)
		}
	case <-ctx.Done():
		return fmt.Errorf("chown %s: %w", m.Source, ctx.Err())
	}
	return nil
}

// mountOwner is the uid:gid writable mounts are handed to. It follows the
// image's USER by default, because the official Renovate image runs as a
// non-1000 uid and a 1000-owned directory is not writable for it.
func (r *Runner) mountOwner(ctx context.Context) (string, error) {
	r.ownerMu.Lock()
	defer r.ownerMu.Unlock()

	if r.owner != "" {
		return r.owner, nil
	}
	owner, err := r.resolveOwner(ctx)
	if err != nil {
		return "", err
	}
	r.owner = owner
	return owner, nil
}

func (r *Runner) resolveOwner(ctx context.Context) (string, error) {
	if r.cfg.UID > 0 {
		gid := r.cfg.GID
		if gid <= 0 {
			gid = r.cfg.UID
		}
		return fmt.Sprintf("%d:%d", r.cfg.UID, gid), nil
	}

	inspect, err := r.client.ImageInspect(ctx, r.cfg.Image)
	if err != nil {
		return "", fmt.Errorf("inspect image %s to resolve its user: %w", r.cfg.Image, err)
	}
	user := ""
	if inspect.Config != nil {
		user = inspect.Config.User
	}
	uid, gid, ok := parseImageUser(user)
	if !ok {
		return defaultOwner, nil
	}
	return fmt.Sprintf("%d:%d", uid, gid), nil
}

// parseImageUser extracts the numeric user from a Docker image's USER value,
// which may be "1000", "1000:1000" or a user name. Names cannot be resolved
// without running the image, so they are rejected.
func parseImageUser(user string) (int, int, bool) {
	user = strings.TrimSpace(user)
	if user == "" {
		return 0, 0, false
	}

	uidPart, gidPart, hasGID := strings.Cut(user, ":")
	uid, err := strconv.Atoi(uidPart)
	if err != nil {
		return 0, 0, false
	}
	if hasGID {
		if gid, err := strconv.Atoi(gidPart); err == nil {
			return uid, gid, true
		}
	}
	return uid, uid, true
}

func toMountType(t MountType) mount.Type {
	if t == MountBind {
		return mount.TypeBind
	}
	return mount.TypeVolume
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

func (r *Runner) remove(id string, w io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := r.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
		_, _ = fmt.Fprintf(w, "# remove container: %v\n", err)
	}
}
