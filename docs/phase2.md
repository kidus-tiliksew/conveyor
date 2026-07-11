# Phase 2 operations and validation

**Status: complete as of July 11, 2026.** The spec §19 deliverables and the
multi-harness + human-gate proof are implemented and validated. Phase 3 has not
started.

Phase 2 separates the control and execution planes around one Postgres
dependency. `conveyord` owns HTTP, GitHub ingestion, durable task mutations,
and the embedded dashboard. `conveyor-runner` owns Docker, worktrees, secret
resolution, harness credentials, and River job execution. The store inserts a
River job in the same transaction as the event/projection that queues a task.

## Start the two processes

```sh
export CONVEYOR_DATABASE_URL='postgres://conveyor:...@localhost/conveyor?sslmode=disable'
export CONVEYOR_API_TOKEN="$(openssl rand -hex 32)"

bin/conveyord -config conveyor.yaml -addr 127.0.0.1:8080 -poll-github 60s
# Separate terminal or runner host:
bin/conveyor --config conveyor.yaml runner start --local
```

The activity and review UI is served at `http://127.0.0.1:8080/`. Review
mutations require the same API token in the dashboard header. The token is held
in browser session storage, not embedded into the UI bundle.

The memory backend remains available only as an explicit compatibility mode
for tests and local development. It runs the Phase 1 co-process queue through
an explicit static router, so job metadata follows the production dispatch
contract without pretending to provide durable leases.

## Credential and vendor policy model

Credential config stores references only: a runner-local credential directory
or a `secretref://` URL. Values remain in SOPS and are resolved only by the
runner at sandbox boot. Personal and team subscription credentials are scoped
to `routing.owner_id`; organization-owned API capacity may be shared. A leased
credential cannot be selected concurrently, and abandoned leases expire.

Subscription capacity is fail-closed. The exact `(vendor, harness, auth_mode)`
record must have `subscription_headless: allowed`, or `restricted` plus the
explicit `routing.allow_restricted` opt-in. `unknown` and `disallowed` never
route. API credentials remain eligible because the registry field governs
subscription headless use. Harness rate-limit signals release the lease into a
five-minute cooldown rather than attempting circumvention.

Claude Code on macOS stores interactive credentials in Keychain, so a
`~/.claude` directory is not portable into Linux containers. Use Claude Code's
`setup-token`, store the result as `CLAUDE_CODE_OAUTH_TOKEN` through
`conveyor secrets set`, and configure its `secretref://` as shown in
`conveyor.example.yaml`. API users use the same path with `ANTHROPIC_API_KEY`.
Codex subscription auth stages only `auth.json`; neither harness inherits the
operator's interactive settings.

## Transcript boundary

The runner writes only injected environment-variable names and the read-only
credential-file path to the control directory, never their values. The shim
reads environment values plus every nested string in the mounted credential
JSON and builds in-memory exact and common-encoding matchers. It also detects
known credential formats, private keys, and high-entropy secret assignments.
Every normalized event is redacted before reaching stdout or the attempt event
log; shim diagnostics use the same redacting writer.

The attempt-scoped `events-<job-id>.jsonl` is authoritative. Docker's live log
stream is best effort and a stream failure no longer fails a successful job
when the artifact contains a valid terminal event. Persisted transcript
metadata contains only the URI and counts by detector class. The dispatcher
also derives token/cost totals and a bounded redacted job summary for the
activity timeline.
The current sandbox-writable integrity boundary is documented explicitly in
[known limitations](known-limitations.md#5-the-sandbox-can-write-its-own-authoritative-transcript-file).

## Dashboard query model

`GET /v1/activity` returns task summaries and activity markers in two store
queries regardless of task count. Full jobs, events, and interventions load
only for `GET /v1/tasks/{id}/activity`. The SPA does not poll the full activity
feed every five seconds; selected-task SSE updates are coalesced before query
invalidation. SSE reads events strictly after the last delivered event ID, and
the supporting Postgres indexes are installed by migration 004.

## Database upgrades and recovery

Migrations are embedded, versioned, serialized by a Postgres advisory lock,
and recorded in `conveyor_schema_migrations`. River's own bundled migrations
run after Conveyor's control-plane schema. Upgrades are forward-only: back up
before replacing binaries, then start one control-plane or runner process to
apply pending versions.

Example backup and restore:

```sh
pg_dump --format=custom --file=conveyor-$(date +%Y%m%d).dump "$CONVEYOR_DATABASE_URL"
createdb conveyor_restore
pg_restore --clean --if-exists --no-owner \
  --dbname=postgres://localhost/conveyor_restore conveyor-YYYYMMDD.dump
```

Events and interventions are append-only at the database layer. Tasks in the
GitHub `claiming` state are reconciled idempotently on every poll before they
can transition to `queued`, eliminating the Phase 1 crash window between a
label claim and durable dispatch.

## Evidence runs on July 10-11, 2026

- Live Postgres 17 container: migrations, persistence across reopen,
  append-only triggers, workspace isolation, transactional task/River insert,
  rollback behavior, claim-before-enqueue, credential lease exclusion/release.
- Live River worker: workspace-specific queue consumed a transactionally
  inserted task and finalized its River row.
- Separate `conveyord` and `conveyor-runner` processes connected to the same
  Postgres database; the control plane served an empty workspace-scoped
  activity feed while the runner held the durable workspace queue.
- Real browser: deep-linked dashboard rendered the human gate without console
  errors, enabled actions only after token entry, submitted a reason-coded
  redirect, moved the task back to Triage, and rendered the bounce history.
- Claude Code adapter fixtures cover init/session capture, assistant tool use,
  token usage, tool results, final result/cost, and permission-rule mapping.
- A Claude Max OAuth token generated by `claude setup-token` was stored through
  `conveyor secrets set` in a mode-0600 SOPS file. The runner resolved it into
  a short-lived env file, injected only `CLAUDE_CODE_OAUTH_TOKEN`, removed the
  staging file after container creation, and persisted no credential value.
- Live task `260711-123f60` routed through credential `local-claude` to Claude
  Code 2.1.206 in a Tier A container. It produced commit `b817cea` (`docs:
  validate Claude OAuth path`) as `Conveyor <agent@conveyor.local>`, a complete
  26-event transcript and handoff snapshot, then reached `awaiting_human`.
- That run exposed project-scoped Claude resume: launching `--resume` outside
  the main worktree returned `No conversation found`. The adapter now retains
  the main run worktree. Follow-up task `260711-52f3af` produced commit
  `447a500` and resumed the identical session ID for handoff: 22 events, seven
  `handoff_resume` events, zero fallback events, a valid snapshot, and terminal
  `done` before `awaiting_human`.
- The same live pass exposed River's default unique-state set suppressing an
  intentional redispatch while a completed row remained. Dispatch uniqueness
  now covers active/retryable states only; a live Postgres integration test
  proves a completed task can enqueue a second River job without permitting
  concurrent duplicates.
- Closure regression pass: activity-index reads were reduced from per-task
  history fan-out to two summary queries; SSE was changed to incremental event
  reads; review mutation uses the latest-job lookup; running-job JSON omits a
  zero `ended_at`; memory/Postgres relationship semantics were aligned; and a
  live Postgres 17 run validated migrations 001-005, activity markers,
  incremental events, latest-job lookup, and River dispatch behavior.
- Both harness adapters now share version/process-stream lifecycle plumbing;
  runner and shim share one credential-layout contract; mounted credential
  files fail closed; JSON payload fallback, redaction statistics, intervention
  validation, queue hashing, SPA route fallback, and UI action/group contracts
  each have a single implementation owner.
- Final recovery/accounting pass: human redirect comments are included in the
  successor prompt; event and log artifacts are attempt-scoped; rate-limit
  detection uses terminal structured errors; successful jobs clear throttling;
  resumed cumulative cost is converted to an incremental delta; credential
  JSON values join the redaction set; abandoned task credential leases are
  rescued on retry; and queued tasks missing River rows reconcile at startup,
  every minute, or through idempotent redispatch.
