# Conveyor — agent notes

The authoritative design is [conveyor-spec.md](conveyor-spec.md) (v1.2, accepted).
When code and spec disagree, the spec wins; spec changes go by amendment
with a version bump (§21), never silent edits.

## Conventions

- Backend is Go everywhere (spec §17.0): `net/http` + chi, cobra for the
  CLI. Phase 2 adds pgx + sqlc + River — do not introduce other
  persistence or queue dependencies.
- `cmd/conveyor-shim` must stay stdlib-only: it ships as a static binary
  inside every sandbox image.
- Comments citing the spec use `(spec §N)` — keep these accurate; they
  are the traceability layer between code and design.
- `TODO(phase1)` was the blocking-gap marker; none may remain on the closed
  Phase 1 baseline. `TODO(phase1-followup)` marks accepted deferred work.
- Build/test: `make build`, `make test`, `make vet`.

## Phase discipline

Phases 1–2 are complete and validated; preserve their contracts. The roadmap
was re-phased for the Beta milestone (spec §19 v1.2, rationale in §21.2;
working breakdown in [docs/beta-plan.md](docs/beta-plan.md)). Pre-Beta is
exactly two phases:

- **Phase 3** — full pipeline: multi-stage orchestration, triage/spec/
  code-review agents, §4.1 spec format, proto-pack role prompts, per-repo
  images, PR-comment redirects, budget breaker + timeouts.
- **Phase 4** — UI rewrite (shadcn/ui, full §13.3) → **Beta: Conveyor
  develops Conveyor**.

Phase 3 and its live dogfood exit run are complete. Phase 4 has not been
activated. Do NOT build post-Beta or deferred surfaces:
command-policy shim approval cards, environment inference/repair, monitor
agent (Phase 5); memory store / pgvector (Phase 6); transcript mining /
self-improvement / eval rig (Phase 7); verification agent, K8sRunner,
multi-repo worktree sets, aggregate cost dashboard (Phase 8,
demand-triggered); enterprise SSO/SCIM/RBAC/HA (Phase 9, demand-triggered).
