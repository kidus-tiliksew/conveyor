# Conveyor — agent notes

The factory's confirmed document corpus is the design authority: Requirements,
System Design documents, and DEC-n decisions served through the workspace API
and UI. Authority changes only through propose-confirm revisions; decision
supersession replaces the retired amendment process (DEC-48). Product overview
and Roadmap reference documents are informative and never authority. Current
and historical phase status lives in the "Roadmap" reference document.

## Conventions

- Backend is Go everywhere: `net/http` + chi for the API and cobra for the CLI.
  Persistence is pgx + sqlc for the relational schema; the durable queue is
  the event log (`internal/eventlog`, driver `pglog`) through
  `internal/queue/logqueue`. Do not introduce another persistence or queue
  dependency.
- The whole `internal/store/postgres/db/` package is maintained by hand
  (`component-persistence`). `sqlc generate` cannot parse migration 035's
  `{{task_states}}` template before rendering, so PostgreSQL integration tests
  are the validation boundary for column and query changes.
- `cmd/conveyor-shim` and the sandbox execution plane are retired and deleted.
  Do not reintroduce them without confirmed corpus authority.
- Traceability comments cite confirmed REQ-n/AC-n.m, DEC-n, and governing
  System Design document IDs. The former specification-section citation
  convention is retired.
- `TODO(phase1)` was the blocking-gap marker; none may remain on the closed
  Phase 1 baseline. `TODO(phase1-followup)` marks accepted deferred work.
- Build/test: `make build`, `make vet`, `make fmt-check`, `make test`,
  `make validate`, `make test-integration`, and
  `make test-integration-singlestore-ci` are available locally for focused
  iteration and CI-failure diagnosis. `make test` is the ordinary local
  aggregate: Go tests plus web typecheck, Biome lint/format checking, and
  Playwright. Review evidence for these heavy gates is a green run of every
  `.github/workflows/ci.yml` job on the task pull request at the exact reviewed
  head (DEC-72); its PostgreSQL service uses the CI-only
  `make test-integration-ci` entrypoint. Do not run `make validate`,
  `make test-integration`, or `make test-integration-singlestore-ci` locally
  to satisfy review, and record the CI run URL, attempt, head SHA, and every
  job's conclusion, including failed attempts. With the merge gate off, Conveyor
  reads the approved head's check status before an automatic merge and holds
  it on a failed or pending check (DEC-108). With the merge gate on, the
  operator decides, and no code path reads check status.
- Run local validation in this repository only through the Make targets
  above. Never run raw `docker compose down` commands here: validation
  resources are removed by ID after their labels and project are rechecked
  (`component-validation-tooling`).
- Local validation evidence (task scratch space, records for local-only
  acceptance gates such as `make test-validation-docker`, retained diagnostic
  runs, and validation resource recovery) follows
  [docs/validation-evidence.md](docs/validation-evidence.md).
- UI changes get a visual check wherever this repository's Playwright tests
  run. When a change under `web/` alters what a page shows, render each
  changed screen with a Playwright page that mocks the API, at 375 px wide and
  at a desktop width. Save the screenshots in the task scratch root's `tmp`
  child, `$CONVEYOR_TASK_CACHE/tmp`, which
  [docs/validation-evidence.md](docs/validation-evidence.md) defines ("Keep
  scratch data outside checkouts"), and open and inspect each one before
  submitting. Attach them to the task or upload them as a CI artifact, and
  link them from the pull request body; the uploaded copy is the record. Do
  not pass a screenshot as a retained `--reference` to
  `scripts/validation_evidence.py cleanup`, so the cleanup at the end of the
  claim removes it with the rest of the `tmp` child. Never commit screenshots.

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
`.claude/skills/` wraps them for Claude Code. This file, a regular `AGENTS.md`
with no `CLAUDE.md` beside it, is the repository's only guidance file; Claude
Code, Codex, and other AGENTS.md-convention tools read it.
Every push is a proposal — operators confirm; agents never perform
operator-only acts.

## Filing tasks for this repository

These rules extend the filing playbook's body house style for this repository:

- Standing boundary: new edge kinds ship with projector + two-store
  conformance + vocabulary lockstep + rebuild proof on day one. List it under
  a task's `## Boundaries` when the task touches edge kinds.
- Source boundaries: for web-only work, state that regenerating
  `internal/httpapi/dashboard` is expected build output of the web change
  under DEC-16. Never impose a blanket "no changes under `internal/**`"
  boundary; exclude unrelated internal source explicitly without excluding the
  generated dashboard bundle.

## Scope bars

- Memory-store scope is defined by DEC-9.
- Task priority, phase, assignment, queue-order, and hold-retirement scope is defined by DEC-122.

This repository maintains the Conveyor skill sources and keeps its guidance
in a regular `AGENTS.md` with no `CLAUDE.md`. Refresh its guidance directly in
the task worktree with
`conveyor --server 'https://conveyor.kidus.sh' --workspace 'conveyor' repo init --guidance-only`.
This preserves the source skill wrappers and creates no `CLAUDE.md`. Deliver
the updated guidance through the task's review lifecycle.
<!-- conveyor:repo-init owner=v1 version=v0.59.1-13-gb153effa -->
## Conveyor factory work

Repository: `conveyor`. Base branch: `main`.
Server: `https://conveyor.kidus.sh`. Workspace: `conveyor`.
Select a native MCP connection whose endpoint matches this server and pass workspace `conveyor` on every call. MCP registration names vary by machine; registering MCP does not set CLI defaults.
CLI example: `conveyor --server 'https://conveyor.kidus.sh' --workspace 'conveyor' task list`.
Refresh this owned section and the project-scoped skills through ordinary task delivery with `conveyor --server 'https://conveyor.kidus.sh' --workspace 'conveyor' repo init`. To preserve maintained source skill wrappers, refresh only guidance with `conveyor --server 'https://conveyor.kidus.sh' --workspace 'conveyor' repo init --guidance-only`.
On connection failure, report the failed endpoint and missing context. Do not infer a replacement host from localhost defaults, SSH configuration, or release instructions.

The confirmed document corpus is the design authority: Requirements, System Design documents, and DEC-n decisions.
Changes are filed as tasks through Conveyor.
An agent edits only under a live claim in a task worktree resolved by `conveyor checkout <task-id>` inside the machine's configured worktree location, never on the base branch, and never in a separate clone made to get around uncommitted changes in the primary checkout.
Follow the `conveyor-plan` skill for planning, `conveyor-file-tasks` for filing tasks, and `conveyor-work` for task work.
This section and the project-scoped skills are versioned with the CLI. Re-run `conveyor repo init` after an upgrade to refresh both, or use `conveyor repo init --guidance-only` to preserve maintained source skill wrappers.
<!-- /conveyor:repo-init -->
