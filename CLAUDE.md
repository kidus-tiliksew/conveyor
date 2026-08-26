# Conveyor — agent notes

The factory's confirmed document corpus is the design authority: Requirements,
System Design documents, and DEC-n decisions served through the workspace API
and UI. Authority changes through propose-confirm revisions; decision
supersession replaces the retired amendment process (DEC-12). Product overview
and Roadmap reference documents are informative rather than normative. Current
and historical phase status lives in the "Roadmap" reference document.

## Conventions

- Backend is Go everywhere: `net/http` + chi for the API and cobra for the CLI.
  Persistence is pgx + sqlc, and River is the Postgres-backed queue. Do not
  introduce another persistence or queue dependency.
- The whole `internal/store/postgres/db/` package is maintained by hand
  (`design-database`). `sqlc generate` cannot parse migration 035's
  `{{task_states}}` template before rendering, so PostgreSQL integration tests
  are the validation boundary for column and query changes.
- `cmd/conveyor-shim` and the sandbox execution plane are retired and deleted.
  Do not reintroduce them without confirmed corpus authority.
- Traceability comments cite confirmed REQ-n/AC-n.m, DEC-n, and governing
  System Design document IDs. The former specification-section citation
  convention is retired.
- `TODO(phase1)` was the blocking-gap marker; none may remain on the closed
  Phase 1 baseline. `TODO(phase1-followup)` marks accepted deferred work.
- Build/test: `make build`, `make vet`, `make fmt-check`, `make test`, and
  `make test-integration`. `make test` is the ordinary local aggregate: Go
  tests plus web typecheck, Biome lint/format checking, and Playwright. CI
  reports the complete gate on every pull request; blocking merges requires
  operator-enabled branch protection, which is unavailable on the current
  GitHub plan. Its PostgreSQL service uses the CI-only
  `make test-integration-ci` entrypoint.

## Worktrees and branches

Task branch names are assignments, not pre-created refs. Immediately after
reading a work order, the implementing agent uses `conveyor checkout <task-id>`
to resolve a dedicated sibling worktree and performs every edit, test, commit,
and push there. Conveyor does not mutate the primary checkout or reset task
history. Workspace context is explicit across REST, CLI, MCP, dispatch, and
reconciliation; omission is compatible only for a singleton workspace.

## Local planning & filing

Planning documents (requirements, System Design, decisions, overviews) and task
filing can be driven from an operator-side agent session, the headless twin of
in-product planning. The canonical playbooks are
[docs/playbooks/conveyor-planning.md](docs/playbooks/conveyor-planning.md) and
[docs/playbooks/conveyor-task-filing.md](docs/playbooks/conveyor-task-filing.md);
`.claude/skills/` wraps them for Claude Code, and `AGENTS.md` is a symlink to
this file so Codex and other AGENTS.md-convention tools read the same guidance.
Every push is a proposal — operators confirm; agents never perform
operator-only acts.

## Scope bars

- Memory-store scope is defined by DEC-9.
- Task priority, phase, assignment, and queue-order scope is defined by DEC-18.
