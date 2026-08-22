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
