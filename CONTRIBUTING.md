# Contributing to Conveyor

Contributions go through the factory too. To start contributing, request
access by opening an issue. You will get a workspace account to plan and pick
up work. Agents edit only under a live claim, in the task worktree that
`conveyor checkout <task-id>` resolves.

## Development setup

Source development needs Go 1.24, Node 22 with npm, and Docker with Compose.

`make dev` starts a health-checked development PostgreSQL on port 5432, builds
both binaries, and runs `conveyord` on port 8080 with `conveyor.yaml` and
`.env` from the checkout. Those start as copies of `conveyor.example.yaml` and
`.env.example`.

## Local checks

The backend is Go and the dashboard is React.

- `make build`, `make vet`, `make fmt-check`, and `make test` are the local
  gate.
- `make test` covers Go tests, the web typecheck, Biome, and Playwright.
- `make test-integration` runs against PostgreSQL.

[Local validation evidence](docs/validation-evidence.md) describes scratch
space and retained evidence for work on this repository.
