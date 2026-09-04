# Installation

Conveyor ships as two binaries. `conveyord` is the server: REST API, dashboard, event log, and the Postgres-backed queue. `conveyor` is the CLI
that operators and agents use to authenticate, file tasks, check out worktrees,
and run work.

## Prerequisites

A deployed factory needs:

- PostgreSQL 15 or newer. The server checks the version at startup and refuses
  older ones.
- Git.
- An authenticated `gh` CLI, or `GH_TOKEN` for headless use.
- An API key for an OpenAI-compatible model endpoint (for example OpenRouter).
  The server uses it for in-process triage and spec stages.
- The agent CLIs you plan to run (Claude Code, Codex, Grok, or your own
  wrapper).

The release installer needs `curl`, `tar`, and a SHA-256 tool (`sha256sum` or
`shasum`). It does not need `sudo`.

## Install a release

Install the latest `conveyor` and `conveyord` into `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/install.sh | sh
```

To install a reviewed version, pin both the installer and the release:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/v1.2.3/install.sh | sh -s -- v1.2.3
```

The installer supports Linux and macOS on amd64 and arm64. It downloads the
selected GitHub release, verifies the archive against the published
`checksums.txt` before replacing either binary, and rolls both back if
anything fails. Set `CONVEYOR_INSTALL_DIR` to choose another destination.

When it finishes it prints the next step: `conveyor init`. The
[solo](getting-started-solo.md) and
[multiplayer](getting-started-multiplayer.md) guides pick up from there.

## Upgrade

Rerun the installer with the new version, then restart `conveyord`. Startup
applies pending embedded database migrations. The binary refuses to start
against a database that a newer release already migrated, so upgrade the
binary before the database ever gets ahead of it.

## Run the container image

Release tags are also published as
`ghcr.io/kidus-tiliksew/conveyor:<version>`. Stable releases additionally
publish `ghcr.io/kidus-tiliksew/conveyor:latest`; prereleases do not move that
tag. The image contains both Conveyor binaries plus the `git` and `gh` runtime
tools, but it contains no credentials or configuration.

Provide a `conveyor.yaml`, persist the default cache directory, and pass a
container-reachable listen address:

```sh
docker run --rm \
  -p 8080:8080 \
  -v "$PWD/conveyor.yaml:/etc/conveyor/conveyor.yaml:ro" \
  -v conveyor-cache:/home/conveyor/.conveyor/cache \
  -e CONVEYOR_API_TOKEN \
  -e CONVEYOR_DATABASE_URL \
  -e CONVEYOR_LLM_API_KEY \
  -e CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY \
  -e GH_TOKEN \
  ghcr.io/kidus-tiliksew/conveyor:v1.2.3 \
  -config /etc/conveyor/conveyor.yaml -addr 0.0.0.0:8080
```

`CONVEYOR_API_TOKEN`, `CONVEYOR_DATABASE_URL`, `CONVEYOR_LLM_API_KEY`, and
`CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY` are required process environment. Supply
`GH_TOKEN` when the GitHub monitor is enabled. Secret values should come from
your container platform's secret facility; do not add them to the image or
`conveyor.yaml`. PostgreSQL must be reachable from the container.

Once the process starts, `/healthz` is available on the published port. Image
upgrades use the same protocol as binary upgrades: replace the image and
restart it. `conveyord` applies embedded database migrations during startup;
there is no separate container migration command.

The default shutdown budget is 25 seconds. On Kubernetes, set
`terminationGracePeriodSeconds` to at least the selected budget plus five
seconds (30 seconds for the default). Cloud Run has a fixed ten-second
termination window, so start `conveyord` with `-shutdown-timeout` or
`CONVEYOR_SHUTDOWN_TIMEOUT` set below ten seconds, such as `8s`. An explicit
flag takes precedence over the environment value.

## Build from source

Source development also needs Go 1.24, Node 22 with npm, and Docker with
Compose (for the development Postgres).

```sh
git clone https://github.com/kidus-tiliksew/conveyor
cd conveyor
cp conveyor.example.yaml conveyor.yaml
cp .env.example .env
make dev
```

Set `CONVEYOR_LLM_API_KEY` in `.env` and generate the operator token with
`openssl rand -hex 32`. `make dev` starts a health-checked Postgres on port
5432, builds the project, and starts `conveyord` on port 8080.

`make build` alone writes `bin/conveyor` and `bin/conveyord` without starting
anything. The full local test aggregate is `make test`; integration tests
against Postgres run with `make test-integration`.

## What gets installed where

| Thing | Path |
|---|---|
| Binaries | `~/.local/bin` (or `CONVEYOR_INSTALL_DIR`) |
| CLI credentials and per-server defaults | `<user-config-dir>/conveyor/credentials.json` |
| Per-user execution config | `<user-config-dir>/conveyor/conveyor.yaml` |
| Worker enrollment credentials | `<user-config-dir>/conveyor/workers/<workspace>.json` |
| Task worktrees | `~/.conveyor/worktrees` (override with `worktree_root`) |

`<user-config-dir>` is `~/Library/Application Support` on macOS and
`~/.config` on Linux. Credential files are created with mode 0600 in 0700
directories.
