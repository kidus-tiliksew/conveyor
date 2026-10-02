# Session brief: grill the planning-studio and doc-closure draft

Written 2026-10-02 by the flux-saas-runtime session that produced the draft.

## Task

Grill `plan.md` in this directory with the owner (Kidus), then, once he approves, push it to this repository's own factory.
Use the `grill-me` approach: each round asks every open question at once, each with a recommended answer and the rejected alternatives.
Record each answer in `plan.md` immediately, in DEC shape (statement, context, rejected alternatives).
Follow `docs/playbooks/conveyor-planning.md` for the push shape (requirement documents, DEC proposals, System Design revisions, tasks with dependencies).

## Push target

- Server `https://conveyor.kidus.sh`, workspace `demo`, repository `conveyor`, base branch `main`.
- The `conveyor` CLI already has credentials for this server; the Conveyor MCP tools in the flux-saas-runtime session point at a different server (`conveyor.funnelflux.com`), so use the CLI here.
- Push nothing before the owner approves the whole package.
- Agents never confirm documents; the owner confirms each layer in the Conveyor web UI.

## Settled by the owner on 2026-10-02 (do not reopen)

- Plan-studio extends the embedded `conveyor-plan` skill and its playbook; it is not a separate skill.
- Drafts live uncommitted in `.orca/plans/<slug>/`, are pushed once, then deleted; every later change is a Conveyor revision.
- Grill rounds run in the terminal; a round with more than 5 questions or a diagram opens as one browser form.
- A local per-item review page (approval bound to a content hash) comes before the push; confirmation then happens in the web UI.
- Push order: requirements, then the DECs citing them, then designs, then tasks.
- Pre-push review is an optional repo-declared hook; Conveyor ships no multi-model review skill.
  FunnelFlux will point the hook at its own `conveyor-brief-review`.
- Documentation closure is configured by a repo-tracked `.conveyor/docs.yaml`; when the file is absent, the gate is off.
- The governing `docs.yaml` is the base-branch version at task start, pinned into the work order, so a PR cannot weaken its own gate.
- Review gets a new `documentation_assessment` verdict field; the server rejects `approve` while a documentation finding is unresolved.
- The rule: durable docs describe merged behavior only, updated in the same PR as the behavior change, or the PR states `docs: none` with a reason.

## Start with the draft's open questions

The draft ends with 6 implementation questions, each with a recommendation:
where the pinned policy is stored; a separate `conveyor plan ask` page versus the review page; what the content hash covers; how changed docs are detected; empty-match policy diagnostics; and `docs: none` matching rules.
Also challenge anything in the draft that looks unverified or marked `[INFERENCE]`, and verify file:line citations against the current `main` before relying on them.

## Source material (read-only, another repository)

- `/home/orca/_dev/funnelflux-pro/flux-saas-runtime/docs/exec-plans/active/developer-tooling/plan-studio/plan.md` (D1 to D11).
- `/home/orca/_dev/funnelflux-pro/flux-saas-runtime/docs/exec-plans/active/developer-tooling/conveyor-knowledge-closure/plan.md` (D11 to D21).
FunnelFlux is the pilot consumer; keep its specifics out of Conveyor's requirements.

## Out of scope

Committing or pushing git changes without the owner's word; editing the flux-saas-runtime repository.
