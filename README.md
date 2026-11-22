# renovatebot-scheduler

A webhook-driven scheduler that runs [Renovate](https://github.com/renovatebot/renovate) against
[Forgejo](https://forgejo.org/) repositories.

Renovate on its own has no daemon: it runs once and exits. This service listens to Forgejo
webhooks, decides which events deserve a Renovate run, merges bursts of events per repository, and
executes Renovate in its own container (or as a child process) with a bounded concurrency.

It also provides a small web UI for signing in with Forgejo, enabling repositories (and registering
their webhooks automatically), triggering runs, and browsing run history with logs.

## How it works

```
Forgejo ──webhook──▶ /webhook/{token} ──▶ signature ──▶ delivery dedup ──▶ filter ──▶ per-repo queue
                                                                                          │
                                                                      debounce ──▶ Renovate run
                                                                                          │
                                                                      run record + log ──▶ SQLite
```

- **Per-repository webhook.** Enabling a repository creates a Forgejo webhook of type `forgejo` with
  a random secret; the same secret is embedded in the webhook URL and used for HMAC verification.
- **Event filtering.** Only default-branch pushes, PR opens/updates, and comments that mention the
  bot trigger a run (see [Trigger rules](#trigger-rules)).
- **No feedback loops.** Every event whose sender is the bot account is ignored, so Renovate's own
  pushes, pull requests and dashboard edits cannot trigger another run.
- **Delivery dedup.** Forgejo retries are dropped by `X-Forgejo-Delivery` within a 10-minute window.
- **Per-repository serialization.** Each repository has one worker: `debounce → running → idle`.
  Events arriving during the debounce window reset the timer; events arriving while a run is
  executing queue exactly one follow-up run, so nothing is lost.
- **Bounded parallelism.** `CONCURRENCY` caps how many Renovate processes run at the same time
  across different repositories.
- **Retries.** Failed runs are retried `MAX_RETRIES` times with linear `RETRY_BACKOFF` backoff. A
  non-zero exit code means Renovate failed; a run error means it could not execute at all.

## Trigger rules

| Event | Trigger |
| --- | --- |
| `push` to the repository's default branch | yes |
| `push` that deletes a branch | no |
| `push` to any other branch | no |
| `pull_request` / `pull_request_sync`, action `opened`, `reopened`, `synchronized` | yes |
| `pull_request` that is no longer open | no |
| `issue_comment`, action `created`, body mentions `@<BOT_USER>` | yes |
| `issues` | never (Renovate edits its own dashboard issue) |
| `ping` and everything else | no |
| any event sent by `BOT_USER` | never |

Comment commands are forwarded as-is; Renovate decides whether `@bot rebase`, `@bot retry` or
`@bot recreate` is meaningful.

## Scheduling

Three independent sources of runs, all going through the same per-repository queue:

- **Webhooks** — the table above.
- **Startup catch-up** — with `STARTUP_RUN=true` (the default) every enabled repository is queued
  once on startup. Renovate is idempotent, so this is safe after a restart or crash.
- **Periodic runs** — every minute the scheduler looks for repositories whose last run (`runs.started_at`)
  is older than `SCHEDULE_INTERVAL` and queues them with trigger `schedule`. Repositories that never
  ran are queued immediately on startup. Set `SCHEDULE_INTERVAL=0` to disable.

## Requirements

- A Forgejo instance reachable from this service, and this service reachable from Forgejo (the
  webhook URL is derived from `BASE_URL`).
- A dedicated bot account with a personal access token (`RENOVATE_TOKEN`) that can write to the
  repositories Renovate should manage.
- An OAuth2 application registered on Forgejo for the admin UI.
- Either access to the Docker socket (default `docker` runner) or a `renovate` binary in the same
  container (`system` runner).

## Quick start

1. Create a bot account (for example `renovate-bot`) and a personal access token for it. It needs
   read/write access to the repositories Renovate will manage.
2. On Forgejo go to **Settings → Applications → Create OAuth2 Application**:
   - Redirect URI: `https://renovate.example.com/oauth/callback` (exact match, no trailing slash)
   - Forgejo does not implement OAuth scopes: the token this service receives has the full rights of
     the user who signs in. It is kept in memory only and used solely to manage webhooks.
3. Copy `compose.yaml`, fill in `BASE_URL`, `FORGEJO_URL`, `OAUTH_CLIENT_ID`, `OAUTH_CLIENT_SECRET`,
   `RENOVATE_TOKEN` and `ALLOWED_USERS`.
4. Start it:

   ```sh
   DOCKER_SOCKET_GID="$(stat -c '%g' /var/run/docker.sock)" docker compose up -d
   ```

5. Open `BASE_URL`, sign in with a Forgejo account listed in `ALLOWED_USERS`, and enable the
   repositories to watch. Enabling requires admin permission on that repository, because a webhook
   has to be created.

The service exposes `GET /healthz` for liveness checks.

## Configuration

### Core

| Variable | Default | Description |
| --- | --- | --- |
| `ADDR` | `:8080` | Listen address. |
| `BASE_URL` | required | Public URL of this service, e.g. `https://renovate.example.com`. Must be reachable from Forgejo. `https://` enables secure cookies. A path prefix is not supported by the server itself; strip it in the reverse proxy while keeping it in `BASE_URL`. |
| `FORGEJO_URL` | required | Forgejo base URL, no trailing slash. |
| `OAUTH_CLIENT_ID` | required | OAuth2 application client ID. |
| `OAUTH_CLIENT_SECRET` | required | OAuth2 application client secret. |
| `RENOVATE_TOKEN` | required | PAT of the bot account Renovate runs as. |
| `BOT_USER` | resolved from `RENOVATE_TOKEN` | Bot account login. Events sent by this user never trigger runs. |
| `ALLOWED_USERS` | empty | Comma-separated logins allowed to sign in to the UI. When empty, only `BOT_USER` can sign in. |
| `DATA_DIR` | `data` | Directory for the SQLite database. |

### Scheduling

| Variable | Default | Description |
| --- | --- | --- |
| `DEBOUNCE` | `20s` | Quiet period after an event before a repository's run starts. |
| `RUN_TIMEOUT` | `30m` | Hard timeout for one Renovate run. |
| `SHUTDOWN_TIMEOUT` | `5m` | How long to wait for in-flight runs on shutdown. |
| `CONCURRENCY` | `2` | Maximum concurrent Renovate runs across all repositories. |
| `MAX_RETRIES` | `2` | Retries after a failed run. |
| `RETRY_BACKOFF` | `1m` | Backoff between retries, multiplied by the attempt number. |
| `STARTUP_RUN` | `true` | Queue every enabled repository once on startup. |
| `SCHEDULE_INTERVAL` | `24h` | Maximum time between runs of one repository. `0` disables periodic runs. |

### Runner

| Variable | Default | Description |
| --- | --- | --- |
| `RUNNER` | `docker` | `docker` or `system`. |

#### `RUNNER=docker`

Renovate runs in a short-lived container created through the Docker Engine API. The scheduler
container needs access to `/var/run/docker.sock`.

| Variable | Default | Description |
| --- | --- | --- |
| `DOCKER_HOST` | from environment | Docker socket or API endpoint. Empty means the SDK default (`unix:///var/run/docker.sock`). |
| `RENOVATE_IMAGE` | `ghcr.io/renovatebot/renovate:latest` | Renovate image. Pin a concrete version for reproducible runs. |
| `RENOVATE_PULL` | `missing` | `missing` (pull only when absent), `always`, or `never`. |
| `RENOVATE_MOUNTS` | `renovate-cache:/tmp/renovate/cache` | Comma-separated `source:target[:ro]` mounts. A source starting with `/` is a bind mount resolved on the Docker host; anything else is a named volume. `-` disables mounts. |
| `RENOVATE_UID` / `RENOVATE_GID` | image `USER` | Owner of writable mounts. Zero means "use the image's `USER`", which is what the official image requires (it runs as uid `12021`, not `1000`). |
| `RENOVATE_LOG_LEVEL` | `info` | `LOG_LEVEL` passed to Renovate. |

Writable mounts are handed to the Renovate user with a one-off root container (`chown -R`) before
the first run that uses them. Docker creates missing bind sources and named volumes as `root`, and
the container runs as a non-root user, so without this the first run fails with `EACCES` on
`/tmp/renovate/cache`. Bind mount sources are host paths; the directory has to be writable by the
Renovate uid on the host after the chown.

#### `RUNNER=system`

Renovate runs as a child process of the scheduler. Use this when the scheduler binary is baked into
the Renovate image itself, so a run costs a process instead of a container.

| Variable | Default | Description |
| --- | --- | --- |
| `RENOVATE_BIN` | `renovate` | Path to the Renovate executable. |
| `RENOVATE_LOG_LEVEL` | `info` | `LOG_LEVEL` passed to Renovate. |

The child runs in its own process group; on timeout the whole group is killed so git and other
subprocesses die with it.

### Renovate config

Both runners accept an optional global Renovate config, injected through `RENOVATE_CONFIG_FILE`
inside the run. Use either:

| Variable | Description |
| --- | --- |
| `RENOVATE_CONFIG` | Inline config content, written to a file inside the run. |
| `RENOVATE_CONFIG_FILE` | Path readable by the scheduler, read once at startup. |

Setting both is a startup error. With the Docker runner the file is copied into the container, so
no host path has to be shared with the Renovate container; with the system runner a temporary file
is created and removed after the run.

## Storage

Everything lives in `DATA_DIR/renovate.db` (SQLite, WAL mode):

- `repos` — enabled repositories with their webhook id, secret and who enabled them.
- `runs` — one row per run: trigger, attempt, timings, exit code, error, and the captured log.

A run row is written when the run starts and updated when it finishes, so in-progress runs are
visible in the UI; their log is flushed every 2 seconds and the detail page refreshes itself.
Logs are capped to the last 16 MiB per run. Runs left `running` by a crash are marked as
`interrupted by scheduler restart` on the next startup.

## Security notes

- The Forgejo OAuth token has no scope: it equals the full rights of the signing-in user. It is kept
  in memory only, never persisted, and only used to list repositories and manage webhooks.
- Renovate never runs with a user's OAuth token. It always uses `RENOVATE_TOKEN`, so pull requests
  come from the bot account and a leaked container environment cannot hand out a user's identity.
- Only users in `ALLOWED_USERS` can sign in, and enabling a repository requires admin permission on
  it.
- The Docker socket is equivalent to root on the host. Only give this service access if you accept
  that; the non-root container user is not a security boundary here.

## Development

```sh
go build ./...
go test -race ./...
```

Generate ent code after editing `internal/ent/schema`:

```sh
go run entgo.io/ent/cmd/ent generate ./internal/ent/schema
```

Two Dockerfiles are provided: `Dockerfile` compiles inside the build stage, and `Dockerfile.prebuilt`
copies a binary compiled on the CI runner. Pushing to `master` publishes
`ghcr.io/trim21/renovatebot-scheduler:master`; pushing a `v*` tag publishes versioned images and
`latest`.
