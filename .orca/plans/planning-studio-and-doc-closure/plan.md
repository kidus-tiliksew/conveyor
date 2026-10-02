# Planning studio and documentation-closure gate

## Status

Draft, 2026-10-02; committed on a review branch for core-developer review before any factory push.
Owner settled every tradeoff in this file on 2026-10-02; grill round 1 (2026-10-02) settled the implementation questions, recorded under "Grill decisions".
After the core developer's review and the owner's approval, this package is pushed to the Conveyor factory (`https://conveyor.kidus.sh`, workspace `demo`, repo `conveyor`, base `main`); after that every change is a Conveyor revision.
Source drafts being generalized: `flux-saas-runtime/docs/exec-plans/active/developer-tooling/plan-studio/plan.md` and `.../conveyor-knowledge-closure/plan.md` (D2, D7-D12, D16-D21).
FunnelFlux survives here only as the pilot example; no FunnelFlux path or skill name is a Conveyor dependency.

## Goal

A human plans a feature as three fast loops — grill the idea, decompose it into corpus-shaped documents, review and approve each item on one local page — and pushes the approved package to Conveyor once.
The same release gives any Conveyor repository an optional documentation-closure gate: durable docs describe merged behavior, and a task that changes behavior updates them in its own pull request or states why none are affected.
Success for Feature A: an owner who has never read the `conveyor-plan` playbook can start from a bare feature idea, answer grill rounds, and approve a whole package item by item on a local page.
Success for Feature B: a pull request that changes behavior without the declared docs edit and without a stated reason cannot reach an approved review verdict.

## Current state

Verified against `/home/orca/_dev/funnelflux-pro/conveyor` on 2026-10-02.

- The planning skill is the embedded `conveyor-plan` asset: `cmd/conveyor/skills_assets/conveyor-plan/SKILL.md` (18 lines) plus the canonical playbook `cmd/conveyor/skills_assets/conveyor-plan/conveyor-planning.md`, byte-identical to `docs/playbooks/conveyor-planning.md`.
- The playbook defines the push formats but no local draft, no grill, no review page, and no local checks; it assumes the agent drafts in chat and pushes directly.
- Skills install is harness-neutral: `cmd/conveyor/skills.go:153-158` maps claude, codex, cursor, and opencode to their native roots; `cmd/conveyor/skills.go:26` embeds `skills_assets/*/*` and `cmd/conveyor/skills.go:22-25` describes the release-carried snapshot.
- `conveyor repo init` installs guidance and skills from that snapshot (`cmd/conveyor/repo_init.go:35-52`).
- Repo-tracked verification kits are the precedent for a repo-declared gate: `.conveyor/kits/manifest.yaml` beside `.conveyor/kits/**` (`.conveyor/kits/manifest.yaml:1-20`), parsed by `internal/verification` and validated offline by `cmd/conveyor/kit_validate.go:57-117` with no network.
- The server already reads a repo-tracked manifest at an exact revision through the workspace GitHub App: `internal/trigger/github/verification.go` (`ResolveBranchHead`, `DiscoverVerification`), capped by `verification.MaxManifestBytes` (`internal/verification/manifest.go:22`).
- A review verdict is `pipeline.Review` (`internal/pipeline/output.go:40-49`) parsed by `ParseReview` (`internal/pipeline/output.go:120-155`); the MCP tool `submit_review_verdict` decodes it at `internal/httpapi/mcp.go:491-508` with schema vars at `internal/httpapi/mcp.go:834-859`.
- Verdict validation that blocks an approval already exists: `store.ValidateDoneCriteriaCoverage` rejects `approve` with unresolved criteria (`internal/store/store.go:576-625`), `validateReviewCitations` and `validateGovernanceAssessment` do the same for citations and governance (`internal/dispatch/dispatch.go:1453-1500`, `internal/dispatch/dispatch.go:1504`), all wired in `applyReview` (`internal/dispatch/dispatch.go:1295-1340`).
- Review authority is pinned onto the work order: `WorkOrder.ServedRequirementSnapshot` and `WorkOrder.GovernanceSnapshot` (`internal/core/types.go:731-738`), surfaced to the reviewer through `workorder.Context` (`internal/workorder/service.go:52-83`).
- Both snapshots are resolved when a review or verify order is claimed (`internal/workorder/service.go:241-272`), not at task start; the implementer claim carries neither.
- `submit_for_review` already computes the authoritative changed paths for the pushed head against the PR base (`internal/workorder/service.go:1525-1546`, via `dispatch.ReviewBranchChangedPaths` at `internal/dispatch/dispatch.go:180-188`) and can read the PR description (`internal/workorder/service.go:930`).
- Conveyor regenerates only a marked lifecycle region of the PR body and preserves agent-authored Markdown on either side (`internal/trigger/github/github.go:1092-1116`).
- The implementer contract is `pack/roles/implement.md`; the reviewer contract is `pack/roles/review.md`.
- The confirmation capability is operator-only: `CapabilityConfirmDocuments` (`internal/core/authorization.go:24`) is granted to the operator role alone (`internal/core/authorization.go:61-72`); maintainer lacks it (`internal/core/authorization.go:52-60`); the UI gates on it (`web/src/pages/planning.tsx:21`).
- The web UI confirms per-version documents in `web/src/pages/requirements.tsx` and `system-design.tsx`; there is no per-item local review surface and no plan draft reader.
- The `demo` corpus (read 2026-10-02) holds 16 System Design documents, all `component-*` baselines plus the `feature-verification-kit-execution` overlay, following DEC-34's baseline-and-overlay convention; no `design-*` IDs exist.
- Current governed scope of the paths this work touches: `component-work-orders` (`internal/pipeline/output.go`, `internal/workorder/service.go`, `internal/dispatch/dispatch.go`, `internal/store/store.go`, `pack/roles/*.md`), `component-mcp-protocol` (`internal/httpapi/mcp.go`), `component-task-lifecycle` (`internal/core/types.go`), `component-git-delivery` (`internal/trigger/github/verification.go`), `component-runtime` (`cmd/conveyor/main.go`, `cmd/conveyor/skills_assets/**`), `component-verification-strategy` (`docs/playbooks/conveyor-work.md`), `component-persistence` (store migrations and `internal/store/postgres/db/**`).
  `cmd/conveyor/plan*.go`, `cmd/conveyor/docs*.go`, `docs/playbooks/conveyor-planning.md`, and `internal/docsconfig/**` are ungoverned today.
- No `plan` or `docs` cobra subcommand exists in `cmd/conveyor`, so the new verbs do not collide.

## Feature A: planning studio

### Delivery shape

The studio is an extension of the embedded `conveyor-plan` skill and `conveyor-planning.md`, not a new skill [DEC-P1].
The deterministic parts — draft checks, the local review page, the pre-push hook runner — ship as new CLI subcommands under `conveyor plan` in `cmd/conveyor/plan.go`.
The CLI is chosen because `conveyor-plan` must be usable from every skill destination in `cmd/conveyor/skills.go:151-158`, and a page server implemented in Go runs wherever the CLI runs [INFERENCE].
Alternatives rejected: a Lavish-served page (a FunnelFlux harness dependency, not harness-neutral), and hand-written HTML per round (layout drift and no stable item IDs).

### Draft layout

The draft lives in a git-ignored local folder that the operator's harness chooses as appropriate for the repository; Conveyor names no default path, and the commands take the draft directory as an argument [DEC-P10].

| File | Holds | Pushed as |
|---|---|---|
| `brief.md` | Goal, users, current versus target behavior, diagrams, out of scope | Reference document (informative) |
| `requirements/<doc>.md` | One capability per file, in the exact `conveyor:requirements` format | Requirement document |
| `decisions.yml` | Local IDs `D1`, `D2`; statement, context, rejected alternatives | `DEC-n` proposals |
| `designs/<id>.md` | Mechanism, diagrams, one `conveyor:governs` fence | System Design document |
| `tasks.yml` | Title, body, dependencies, governing document IDs, docs paths to update or a `docs: none` reason | Tasks |
| `notes.yml` | Per-item plain-language headline, example, and why | Not pushed |
| `review.json` | Per-item content hash, verdict, comments, round | Not pushed |

Normative files use Conveyor's exact formats, so what the owner approves is what is pushed; readability lives in `notes.yml` and `brief.md` [DEC-P1].

### Grill

Grill runs in the terminal: each question shows its options with the recommended one marked, and the owner picks or types. [DEC-P1]
A round with more than five questions, or one that needs a diagram, is served as one local browser form by `conveyor plan ask <draft-dir>` instead; one submit answers the round.
Every answer is written to `decisions.yml` immediately as a `DEC-n`-shaped entry (statement, context, rejected alternatives), so decisions need no rewriting later [DEC-P2].
Facts are looked up, never asked; an answer that resolves a dependency unblocks the next question in the same round.

### Decompose and local checks

`conveyor plan check <draft-dir>` runs the checks and prints failures by item ID with a non-zero exit on any failure.
Checks: the draft folder is git-ignored and no draft file is tracked (`git check-ignore`, `git ls-files`) [DEC-P10]; fence presence; acceptance criteria in "When X, the system shall Y" form; `conveyor:governs` globs in the `*`, `?`, `**` dialect and matching at least one tracked path; every `REQ` has an `AC` and every `AC` is covered by a task; every decision cites a requirement; every task names its docs paths or a `docs: none` reason; no design cites a decision absent from the draft (push order).
The checks are offline and deterministic, matching the offline doctrine of `cmd/conveyor/kit_validate.go:33-40`.

### Review page

`conveyor plan review <draft-dir> [--port N]` serves one page on `127.0.0.1` and blocks until the owner submits or closes it.
The page has an overview tab (goal, main diagram, "N of M approved"), one tab each for requirements, decisions, designs, and tasks, and a requirement-by-task traceability matrix.
Each item is a card with its ID, plain headline, exact normative text, example, related links, and Approve / Request change / Question controls with a comment box; "Approve all unchanged" exists per tab.
Approval is per item and bound to a content hash over the item's ID and normative fields only, normalized to LF line endings with trailing whitespace trimmed [DEC-P3]; `conveyor plan review` recomputes hashes on every render.
Normative fields per kind: a requirement's statement and acceptance criteria; a decision's statement, context, and rejected alternatives; a design's body including its `conveyor:governs` fence; a task's title, body, dependencies, governing document IDs, and docs paths or `docs: none` reason.
`notes.yml` text is never hashed, so improving a headline or example never invalidates an approval.
A change does not cascade: an approved card whose linked item changed keeps its approval and shows a "linked item changed" badge.
One "Send review" button writes `review.json` as `{round, items: [{id, hash, verdict, comment}]}` and returns; the agent reads `review.json` after the command exits.
Comments route back to the planner with the item ID and the file that holds the item, and the agent applies each to the right file [DEC-P3]; a card whose hash changed since approval returns to pending and shows a before/after view while untouched approvals stay approved.
A comment that changes the design tree returns the item to Grill rather than being applied silently.

### Push

When every item is approved, the push runs one layer at a time through `conveyor plan push <draft-dir> --layer requirements|decisions|designs|tasks [--dry-run]` in the order reference document with requirements, decisions, designs, then tasks [DEC-P4] [DEC-P11].
The command reads live confirmation state from the server and refuses a layer whose prerequisites are not yet confirmed (a decision citing an unconfirmed requirement, a design citing an unconfirmed decision, a task citing an unconfirmed document); `--dry-run` prints the proposal payloads without posting.
It never confirms: `CapabilityConfirmDocuments` is operator-only (`internal/core/authorization.go:24,61-72`), and the owner confirms each layer in the web UI at `web/src/pages/requirements.tsx` and `system-design.tsx` before running the next layer.
`conveyor plan check` stays offline; only `plan push` contacts the server.
After the last layer the draft is deleted; every later change is a Conveyor revision.

### Pre-push review hook

The optional pre-push review hook lives in a sibling repo-tracked file `.conveyor/planning.yaml`, not in `docs.yaml` [DEC-P5].
`docs.yaml` is read and pinned by the server for the review gate, so a planning-only concern stays out of a server-enforced contract; `.conveyor/planning.yaml` is read only by the local studio and is never pinned.
Absent file or absent `pre_push_review` key means the studio skips the hook.
The hook is an argv list executed without a shell, with a timeout and a `blocking` flag; FunnelFlux points it at its own `conveyor-brief-review`, and Conveyor ships no review skill [DEC-P5].
It runs once over the whole approved draft before the requirements layer; its pass is bound to the hash of every normative item, and it runs again only when an item changed since that pass [DEC-P5].
Example `.conveyor/planning.yaml`:

```yaml
schema_version: 1
pre_push_review:
  command: ["conveyor-brief-review"]
  timeout_seconds: 900
  blocking: true
```

### Playbook and skill changes

- `docs/playbooks/conveyor-planning.md` and its byte-identical embedded copy `cmd/conveyor/skills_assets/conveyor-plan/conveyor-planning.md` gain the Draft layout, Grill, Decompose, Review page, Push, and Pre-push hook sections above.
- `cmd/conveyor/skills_assets/conveyor-plan/SKILL.md` gains the draft-first non-negotiable: draft and review locally before any push, and never confirm.
- New `conveyor plan` subcommands: `check` (offline), `ask` and `review` (local pages sharing one renderer, with distinct item shapes and output files), and `push` (online, layer-gated) [DEC-P11].
- No new skill directory is added; the install snapshot in `cmd/conveyor/skills.go:22-26` grows only with the CLI and the two edited assets.

## Feature B: documentation-closure gate

### `.conveyor/docs.yaml`

A repository declares its durable-docs gate in repo-tracked `.conveyor/docs.yaml` beside `.conveyor/kits/manifest.yaml`; absent file means the gate is off [DEC-P6].
The schema and its parser live in a new shared package `internal/docsconfig`, so the CLI and the server apply one rule set [DEC-P7].
Example:

```yaml
schema_version: 1
docs:
  - path: docs/knowledge-base/**
    description: shipped-system knowledge base
  - path: docs/architecture/**
    description: architecture pages
rule:
  none_statement: "docs: none"
  reason_required: true
  text: >-
    Durable docs describe merged behavior only.
    A task that changes behavior updates the affected docs in the same pull
    request, or states "docs: none" with a reason.
```

Fields: `schema_version` (must be `1`); `docs` (one or more `{path, description}`, path a repo-relative glob in the `*`, `?`, `**` dialect); `rule.none_statement` (the exact literal a pull request uses to claim no docs are affected); `rule.reason_required` (whether a reason must follow the literal); `rule.text` (the human-readable rule, capped at 4 KiB).
An empty `docs` list is invalid, so a declaration always gates at least one path.

### Base-branch read and pinning

The policy is pinned once per task, when its first implement order is claimed; that claim is "task start" for this gate [DEC-P8].
At that claim Conveyor resolves the repository base branch head through the workspace GitHub App using the existing `ResolveBranchHead` path (`internal/trigger/github/verification.go:238`) and reads `.conveyor/docs.yaml` at that commit through the same contents call `DiscoverVerification` uses, capped at `verification.MaxManifestBytes` (1 MiB, `internal/verification/manifest.go:22`).
The result is stored as a set-once nullable task field, `core.DocumentationPolicy` (parsed policy, base commit SHA, content hash, or an explicit "off" record when the file is absent); it is never rewritten by later claims, bounces, or base movement.
Pinning on the task rather than on the review snapshot is required because the existing review snapshots are resolved at review claim (`internal/workorder/service.go:241-272`), after the implementer has worked and after the base may have moved.
The pin is the base-branch version, so a pull request cannot weaken its own gate.
A read failure (no App, revoked permission, transient transport) leaves the field unset and is retried at the next claim of any order on the task; while unset the gate is off [DEC-P8].
Each failure records a task event naming the cause, and the work-order context renders "documentation policy unavailable" so the reviewer and operator see that the gate is off; work is not blocked [DEC-P8].
A repository with no GitHub repository configured pins an explicit "off" with a task event, matching how `submit_for_review` already branches on `repo.GitHub` (`internal/workorder/service.go:1536`) [DEC-P8].
The pin is copied into every implement, review, and verify work-order context and rendered through `workorder.Context` (`internal/workorder/service.go:52-83`).
Adding the field needs a migration and the hand-maintained `internal/store/postgres/db/` query change, so `component-persistence` and `component-task-lifecycle` are revised.

### Verdict schema addition

`pipeline.Review` gains `DocumentationAssessment *core.DocumentationAssessment` (`internal/pipeline/output.go:40-49`), decoded from `submit_review_verdict` at `internal/httpapi/mcp.go:491-508` with a new schema var beside `governanceAssessment` (`internal/httpapi/mcp.go:848-859`).
Shape:

```json
{
  "documentation_assessment": {
    "applicable": true,
    "summary": "knowledge-base page updated in this pull request",
    "updated_paths": ["docs/knowledge-base/bot-and-preview-traffic-filtering.md"],
    "unresolved": [],
    "conflicts": []
  }
}
```

`applicable` is true exactly when a pinned policy exists (not "off") and declares at least one path.
`updated_paths` must equal the server-recorded docs-gate evidence for the reviewed head (below); a mismatch is a validation error, not a judgment call.
`unresolved` lists documentation findings the reviewer judges: behavior changed but the declared docs edit does not cover it, or a `docs: none` reason does not hold.
`conflicts` lists disjoint classification conflicts (for example a claimed path outside the policy globs).
`ParseReview` normalizes and validates the shape (sorted, disjoint lists, non-empty summary) the way it does for governance (`internal/pipeline/output.go:146-150`).

### Docs-gate evidence recorded at submission

Under a pinned policy, `submit_for_review` records docs-gate evidence for the head it accepts, reusing the changed paths it already computes against the PR base (`internal/workorder/service.go:1525-1546`) [DEC-P12].
The evidence holds the head SHA, `matched_paths` (changed paths intersected with the policy globs), and the `docs: none` statement with its reason when present.
The `docs: none` statement is matched as the exact, case-sensitive `rule.none_statement` literal at the start of a line in the agent-authored part of the PR body, outside Conveyor's generated lifecycle region (`internal/trigger/github/github.go:1092-1116`); when `rule.reason_required` is true the reason is the non-empty remainder of that same line.
Rejected: case-insensitive matching with multi-line reasons (no mechanical end to the reason).
Task bodies, reports, and commit messages never satisfy the statement.
A resubmission at a new head records fresh evidence; the validator reads the evidence for the reviewed head only.

### Server validation rule and error code

A new `store.ValidateDocumentationAssessment` mirrors `store.ValidateDoneCriteriaCoverage` (`internal/store/store.go:576-625`).
It rejects a verdict when the pinned policy is on and `documentation_assessment` is nil; when `applicable` disagrees with policy presence; when the summary is empty; when lists overlap or contain empty entries; when `updated_paths` differs from the recorded `matched_paths`; and, for `verdict == "approve"`, when `unresolved` or `conflicts` is non-empty, or when `matched_paths` is empty and no valid `docs: none` statement was recorded [DEC-P9] [DEC-P12].
The server therefore decides whether a declared doc changed or a reason was stated; the reviewer decides whether that edit or reason is adequate.
The block error is the sentinel `store.ErrDocumentationUnresolved` with message `review documentation_assessment blocks approve: unresolved findings in <lists>; correct the assessment or submit changes_requested`, mirroring the done-criteria wording; the precondition failure uses the same sentinel with `no declared docs path changed and no docs-none statement in the pull request`.
It is wired into `applyReview` beside the existing validators (`internal/dispatch/dispatch.go:1295-1340`) and therefore also reached from `workorder.Service.SubmitVerdict` (`internal/workorder/service.go:1941-2004`).
When no policy is pinned or the pin is "off", a supplied `applicable=false` assessment is accepted and a supplied `applicable=true` assessment is rejected, matching the `requirement_citations` applicability rule (`internal/dispatch/dispatch.go:1460-1461`).

### Work-order contract addition

`workorder.Context` gains `DocumentationPolicy *core.DocumentationPolicy` (`internal/workorder/service.go:52-83`), rendered as a `# Documentation policy` section naming the declared paths, the `docs: none` literal, and the pinned SHA and hash.
`pack/roles/implement.md` gains a durable-docs discipline item: update the declared docs in the same pull request as the behavior change, or state the reason, citing the pinned policy.
`pack/roles/review.md` gains the `documentation_assessment` requirement beside the existing `requirement_citations` requirement, with the rule that an approval must carry no unresolved finding.

### Skill and playbook changes

- `docs/playbooks/conveyor-work.md` and its embedded copy `cmd/conveyor/skills_assets/conveyor-work/conveyor-work.md` plus `SKILL.md` gain the closure step in the delivery sequence.
- `cmd/conveyor/skills_assets/conveyor-repo-init/AGENTS.md` mentions `.conveyor/docs.yaml` as an optional repo-tracked gate.
- `docs/document-corpus.md` and `docs/tasks.md` describe the gate and the pin.
- No multi-model review skill is shipped for either feature [DEC-P5].

### Offline validator

`conveyor docs validate [path]` reads `.conveyor/docs.yaml` from the working tree and prints a JSON receipt, mirroring `cmd/conveyor/kit_validate.go:33-40` and using the same `internal/docsconfig` parser as the server.
It resolves each declared glob against tracked files with `git ls-files` and exits non-zero on a glob that matches nothing, a malformed glob, an empty `docs` list, an oversized file, or an unsupported `schema_version`.
The server performs no tree listing when it pins; an empty-match glob is caught here and in the repository's own CI, and a repository adopting the gate seeds at least one declared doc first.
It never contacts the server; pin authority stays the caller's.

## Corpus changes

These are the Conveyor documents the push creates or revises; `DEC-n` numbers are placeholders because the server mints real numbers.

### New requirement documents

`req-planning-studio` (Feature A):

```conveyor:requirements
- id: REQ-1
  statement: The conveyor-plan skill shall run feature planning as three stages — grill, decompose, review — in the target repository before any Conveyor proposal exists.
  acceptance_criteria:
    - id: AC-1.1
      statement: When the owner starts planning, the studio shall keep the draft in a git-ignored local folder chosen for the repository and shall not commit it.
    - id: AC-1.2
      statement: When a grill round completes, the studio shall record each answer as a decision entry with statement, context, and rejected alternatives.
    - id: AC-1.3
      statement: When a grill round holds more than five questions or needs a diagram, the studio shall serve it as one local browser form instead of terminal prompts.
- id: REQ-2
  statement: The studio shall check the draft locally before presenting it for review.
  acceptance_criteria:
    - id: AC-2.1
      statement: When the draft contains a malformed acceptance criterion, an uncovered criterion, or a governs glob outside the allowed dialect, the check shall name the failing item and exit non-zero.
    - id: AC-2.2
      statement: When a task in the draft names no durable-doc update and no docs-none reason, the check shall report the task.
    - id: AC-2.3
      statement: When the draft folder is not git-ignored or holds a tracked file, the check shall fail and name the folder.
- id: REQ-3
  statement: The studio shall present the draft as one local review page with a per-item verdict bound to a content hash.
  acceptance_criteria:
    - id: AC-3.1
      statement: When the owner approves an item, the studio shall record the approval against that item's content hash.
    - id: AC-3.2
      statement: When an approved item's normative text changes, the studio shall return only that item to pending and show a before-and-after view.
    - id: AC-3.3
      statement: When the owner submits a comment, the studio shall deliver it to the planner with the item ID and the file that holds the item.
- id: REQ-4
  statement: The studio shall push an approved draft to Conveyor as proposals in a gated order and shall never confirm a document.
  acceptance_criteria:
    - id: AC-4.1
      statement: When the owner pushes a layer, the studio shall refuse it while any document it cites is unconfirmed and shall never confirm a document itself.
    - id: AC-4.2
      statement: When a pre-push review hook is declared in the repository, the studio shall run it once before the first layer, run it again only when an approved item changed since its last pass, and skip it when absent.
- id: REQ-5
  statement: The studio shall run wherever the Conveyor CLI runs, without a harness-specific page server.
  acceptance_criteria:
    - id: AC-5.1
      statement: When the CLI installs the conveyor-plan skill for Claude, Codex, Cursor, or OpenCode, the studio shall be usable from that installation.
```

`req-documentation-closure` (Feature B):

```conveyor:requirements
- id: REQ-1
  statement: A repository shall declare its durable documentation and closure rule in a repo-tracked .conveyor/docs.yaml.
  acceptance_criteria:
    - id: AC-1.1
      statement: When .conveyor/docs.yaml is absent, Conveyor shall apply no documentation gate to that repository.
    - id: AC-1.2
      statement: When a task's first implement order is claimed and .conveyor/docs.yaml exists on the base branch, Conveyor shall pin that version to the task and shall not change the pin for the life of the task.
- id: REQ-2
  statement: An implementation task shall update the durable docs affected by its behavior change in the same pull request, or state docs-none with a reason.
  acceptance_criteria:
    - id: AC-2.1
      statement: When the delivered pull request changes behavior and its documentation edit or docs-none reason does not cover the change, the reviewer shall record a documentation finding.
    - id: AC-2.2
      statement: When a pull request body states the configured docs-none literal at the start of a line, Conveyor shall accept it as the statement only in the agent-authored part of the body and, when a reason is required, only with a non-empty reason on that line.
- id: REQ-3
  statement: The server shall reject a review approval while a documentation finding is unresolved.
  acceptance_criteria:
    - id: AC-3.1
      statement: When a verdict sets verdict=approve with a non-empty documentation_assessment.unresolved or conflicts, the server shall reject the verdict and name the assessment.
    - id: AC-3.2
      statement: When a pinned documentation policy exists and the verdict omits documentation_assessment, the server shall reject the verdict.
    - id: AC-3.3
      statement: When a verdict approves a pull request under a pinned policy that changes no declared docs path and states no docs-none reason, the server shall reject the verdict.
- id: REQ-4
  statement: The CLI shall validate .conveyor/docs.yaml offline against the same rules the server applies.
  acceptance_criteria:
    - id: AC-4.1
      statement: When a declared docs path resolves to no tracked file, the validator shall report a diagnostic and exit non-zero.
```

### Decision proposals

- `DEC-P1` — The planning studio extends the embedded `conveyor-plan` skill and `conveyor-planning.md` and adds `conveyor plan` CLI subcommands; statement: one planning entry point per CLI release, harness-neutral by construction. Alternatives rejected: a new skill (second entry point, refresh drift); a Lavish-served page (harness-specific).
- `DEC-P2` — Grill answers are recorded immediately as `DEC-n`-shaped entries in `decisions.yml`. Alternatives rejected: capture at push time (loses rejected alternatives and their reasons).
- `DEC-P3` — Review approval is per item and bound to a hash of the item's ID and normative fields, normalized to LF with trailing whitespace trimmed; `notes.yml` is never hashed; a change does not cascade to linked items, which show a badge instead; comments route through `review.json` keyed by item ID and file. Alternatives rejected: whole-draft approval (one edit invalidates everything); hashing the whole card including notes (headline polish invalidates approvals); cascading invalidation to linked items (forces re-approval of unchanged text); in-page editing (no stable item identity).
- `DEC-P4` — Push is a gated proposal sequence and the agent never confirms; the owner confirms each layer in the existing web UI. Alternatives rejected: agent-side confirm (violates the operator-only `confirm_documents` capability, `internal/core/authorization.go:24,61-72`).
- `DEC-P5` — The optional pre-push review hook is declared in `.conveyor/planning.yaml`, runs an argv command with no shell, runs once before the requirements layer bound to the draft's normative hashes, re-runs only when an item changed, and is skipped when absent; Conveyor ships no review skill. Alternatives rejected: a key in `docs.yaml` (mixes a local planning concern into a server-pinned contract); running before every layer (repeats a slow review on unchanged content); shipping a review skill (out of scope, harness-dependent).
- `DEC-P6` — The documentation-closure gate is declared in repo-tracked `.conveyor/docs.yaml` beside `.conveyor/kits/manifest.yaml`; absent means off. Alternatives rejected: workspace/DB config (not reviewable with the docs it governs); a preset path (no cross-repo reuse).
- `DEC-P7` — One `internal/docsconfig` parser serves the CLI validator and the server. Alternatives rejected: two parsers (rule drift); CLI-only validation (skill text alone does not enforce; the settled decision requires server enforcement).
- `DEC-P8` — The governing policy is the base-branch `.conveyor/docs.yaml` read when the task's first implement order is claimed, stored as a set-once task field (an absent file or a repository without GitHub is pinned as an explicit "off"), and copied into every implement, review, and verify work-order context; a read failure leaves the field unset, records a task event, renders "documentation policy unavailable" in the work-order context, and is retried at the next claim. Context: the existing review snapshots are resolved at review claim (`internal/workorder/service.go:241-272`), too late for the implementer and after the base may move. Alternatives rejected: the reviewed head's copy (a PR could weaken its own gate); pinning on the review snapshot (implementer sees no policy, base may have moved); pinning at intake (base can drift for a long time before work starts); failing closed on a read error (an infrastructure fault blocks work); logging the failure only on the server (the operator cannot see the gate is off); reading from a server-side clone for non-GitHub repositories (a second read path for a rare configuration).
- `DEC-P9` — The `documentation_assessment` verdict field blocks `approve` through the same validator pattern as governance and done criteria. Alternatives rejected: reviewer skill text alone (lets an approval omit the check).
- `DEC-P10` — Conveyor names no draft location: the operator's harness keeps the draft in a git-ignored local folder appropriate for the repository, every `conveyor plan` command takes the draft directory as an argument, and `conveyor plan check` fails when that folder is not ignored or holds a tracked file. Context: the source drafts used one operator's harness folder, which is not a Conveyor concept. Alternatives rejected: a fixed harness-specific path in code and skills (couples Conveyor to one operator's tooling); committing an ignore rule into every consumer repository; no ignore check (one `git add -A` publishes the draft).
- `DEC-P11` — The `conveyor plan` verbs are `check` (offline), `ask` and `review` (separate local pages over one renderer), and `push --layer <layer> [--dry-run]` (online, refuses a layer whose cited documents are unconfirmed). Alternatives rejected: one `serve --mode` command (couples two page contracts and output files); a push gate inside the offline `check` (contradicts its offline contract); playbook-only REST pushes (no enforcement of the order); one long-running push that waits for confirmations (blocks across sessions).
- `DEC-P12` — Under a pinned policy, `submit_for_review` records docs-gate evidence for the accepted head (changed paths intersected with the policy globs, plus any `docs: none` statement); the server rejects `approve` when neither exists and requires `updated_paths` to equal the recorded matches, while the reviewer judges adequacy. Alternatives rejected: reviewer-marked updates only (an approval can claim an edit that never happened); server path match only (a trivial edit to any declared doc would satisfy the gate).

### System Design documents

Design documents follow DEC-34's baseline-and-overlay convention; current baseline versions were read from `demo` on 2026-10-02.

- New overlay `feature-planning-studio`, naming the baseline it changes (`component-runtime` v15) and governing the paths no baseline governs today:

```conveyor:governs
- repo: conveyor
  paths:
    - cmd/conveyor/plan*.go
    - docs/playbooks/conveyor-planning.md
```

- New overlay `feature-documentation-closure`, naming the baselines it changes (`component-work-orders` v26, `component-mcp-protocol` v14, `component-task-lifecycle` v4, `component-persistence` v25, `component-git-delivery` v13, `component-runtime` v15, `component-verification-strategy` v18) and governing the new ungoverned paths:

```conveyor:governs
- repo: conveyor
  paths:
    - internal/docsconfig/**
    - cmd/conveyor/docs*.go
```

- Baseline revisions: `component-runtime` (adds `cmd/conveyor/plan*.go` and `cmd/conveyor/docs*.go` to its CLI scope; the edited `conveyor-plan`, `conveyor-work`, and `conveyor-repo-init` skill assets), `component-work-orders` (verdict field, validator, docs-gate evidence, work-order context, role contracts), `component-mcp-protocol` (`submit_review_verdict` schema), `component-task-lifecycle` (`core.DocumentationPolicy` task field), `component-persistence` (migration and hand-maintained query), `component-git-delivery` (base-branch policy read), `component-verification-strategy` (`conveyor-work` playbook closure step).
- The push creates only the two overlays; each implementing task proposes the baseline revision for its own paths from its claim, so baselines describe merged behavior only, and existing review dispatch waits on task-authored proposals (`req-260810-70ce2f`).
- `feature-documentation-closure` also carries the mechanism rules folded out of the DEC list: the `docs: none` matcher and the empty-match validator rule.
- Requirements: two new documents, `req-planning-studio` and `req-documentation-closure`; no existing requirement is revised. `req-agent-skills` REQ-1 already covers embedded installation of the edited skills, and `req-verification-kits` is a precedent only.

## Task breakdown

Tasks are filed through `conveyor-file-tasks` after the requirements, decisions, and designs are confirmed.
`depends_on` expresses the real order; sibling tasks name the files they own.

| ID | Task | Depends on | Owns |
|---|---|---|---|
| T1 | `internal/docsconfig` package: `docs.yaml` schema, parse, validate, glob match, and the `docs: none` line matcher. | — | `internal/docsconfig/**` |
| T2 | Task policy pin: `core.DocumentationPolicy` set-once task field, migration and hand-maintained query, base-branch read at the first implement claim, retry on read failure, copy into implement/review/verify `workorder.Context` [DEC-P8]. | T1 | `internal/core/types.go`, `internal/store/**` (task field only), `internal/trigger/github/docs_policy*.go`, `internal/workorder/service.go` (claim and context only) |
| T3 | Docs-gate evidence at `submit_for_review`: matched paths from the existing changed-path computation plus the PR-body `docs: none` statement, recorded per head [DEC-P12]. | T2 | `internal/workorder/service.go` (submission only), `internal/store/**` (evidence record only) |
| T4 | `documentation_assessment` on `pipeline.Review`, MCP schema, `store.ValidateDocumentationAssessment` and `ErrDocumentationUnresolved` wired into `applyReview` [DEC-P9] [DEC-P12]. | T3 | `internal/pipeline/output.go`, `internal/httpapi/mcp.go`, `internal/store/store.go`, `internal/dispatch/dispatch.go` |
| T5 | Role contracts and skill text: implementer and reviewer docs discipline; `conveyor-work` playbook and skill; repo-init guidance. | T4 | `pack/roles/*.md`, `cmd/conveyor/skills_assets/conveyor-work/**`, `docs/playbooks/conveyor-work.md`, `cmd/conveyor/skills_assets/conveyor-repo-init/AGENTS.md` |
| T6 | Offline `conveyor docs validate` over `internal/docsconfig`. | T1 | `cmd/conveyor/docs*.go`, `cmd/conveyor/main.go` (registration only) |
| T7 | `conveyor plan check` (offline draft checks). | — | `cmd/conveyor/plan.go`, `cmd/conveyor/plan_check*.go` |
| T8 | `conveyor plan ask` and `conveyor plan review`: shared local page renderer, `review.json`, normalized item hashes [DEC-P3] [DEC-P11]. | T7 | `cmd/conveyor/plan_page*.go`, `cmd/conveyor/plan_ask*.go`, `cmd/conveyor/plan_review*.go` |
| T9 | `conveyor plan push --layer [--dry-run]`: live confirmation read, layer refusal, proposal posting, never confirm [DEC-P4] [DEC-P11]. | T7 | `cmd/conveyor/plan_push*.go` |
| T10 | Pre-push hook runner: `.conveyor/planning.yaml` parse and argv execution with timeout and blocking flag [DEC-P5]. | T9 | `cmd/conveyor/plan_hook*.go` |
| T11 | `conveyor-plan` playbook and skill text: harness-chosen git-ignored draft folder, grill, decompose, review, layered push, pre-push hook [DEC-P10]. | T8, T9, T10 | `docs/playbooks/conveyor-planning.md`, `cmd/conveyor/skills_assets/conveyor-plan/**` |
| T12 | Docs: `docs/document-corpus.md`, `docs/tasks.md`, `docs/concepts.md` describe the gate, the pin, and the validator. | T4, T6 | those files |

Done criteria and tests per task:

- T1 done when `internal/docsconfig` parses the example schema, rejects empty `docs`, an unsupported `schema_version`, a malformed glob, and an oversized file, and the matcher accepts the exact literal at line start with a same-line reason and rejects case variants, mid-line occurrences, and an empty reason when required; table-driven tests in `internal/docsconfig/*_test.go`.
- T2 done when the first implement claim pins the policy at a fixed base SHA, an absent file pins "off", a later claim after the base moves leaves the pin unchanged, and a read failure leaves it unset and is retried at the next claim; tests in `internal/workorder/service_test.go` and `internal/trigger/github` using the httptest contents fixture pattern in `internal/trigger/github/verification_test.go:99-104`, plus a PostgreSQL integration test for the column.
- T3 done when a submission under a pinned policy records matched paths for the head and a `docs: none` statement only from the agent-authored body region, and a resubmission at a new head records fresh evidence; tests in `internal/workorder/service_test.go` beside the existing `SubmissionChangedPaths` fixtures.
- T4 done when `approve` fails with `ErrDocumentationUnresolved` for non-empty `unresolved`, for `updated_paths` that differ from the evidence, and for no matched path and no statement, and a policy-pinned verdict with no assessment fails; tests mirror `internal/workorder/service_test.go:3440` (`blocks approve`) and `internal/pipeline/output_test.go`, plus an MCP decode test mirroring `internal/httpapi/mcp_test.go:2264`.
- T5 done when the implementer and reviewer prompts contain the policy section and the `documentation_assessment` instruction; `go test ./internal/pack` prompt assertions and a `conveyor repo init` regeneration check.
- T6 done when `conveyor docs validate` exits non-zero on a glob that matches no tracked file and prints a JSON receipt on success; CLI tests in `cmd/conveyor`, plus `make test-repository-install` (`Makefile:272-274`).
- T7 done when `conveyor plan check` names every failing item ID and exits non-zero on any failure, with no network access; CLI tests.
- T8 done when `plan ask` writes one answer per question, `plan review` writes `review.json` with one entry per item, editing one item's normative text resets only that item, and editing `notes.yml` resets nothing; CLI tests against the local server.
- T9 done when `--dry-run` prints payloads in layer order, a layer citing an unconfirmed document is refused, and no confirm endpoint is ever called; CLI tests against an httptest server.
- T10 done when a declared hook runs without a shell, a non-blocking failure does not stop the push, a timeout is enforced, and an absent file skips silently; CLI tests.
- T11 done when the embedded copy equals `docs/playbooks/conveyor-planning.md`, the skill names the draft-first rule, and no harness-specific path appears in either; the existing embedded-asset byte-equality check.
- T12 done when the three docs state the gate, the pin, and the offline validator consistently.

## Verification

- Feature A end to end: a throwaway draft in a temporary git-ignored folder runs `conveyor plan check` (fails on a broken draft, passes when fixed), `conveyor plan ask` for a six-question round, and `conveyor plan review`; approve all, edit one item, re-render, and confirm only that card returns to pending.
- Feature A push dry run: `conveyor plan push --layer decisions --dry-run` prints the proposal payloads and refuses while a cited requirement is unconfirmed.
- Feature B end to end: the pilot repository sets `.conveyor/docs.yaml`; an approve verdict on a behavior-changing PR with no declared docs edit and no `docs: none` line is rejected with the exact error; adding the docs edit or the statement and resubmitting lets a corrected verdict pass.
- Feature B offline: `conveyor docs validate` on the pilot `.conveyor/docs.yaml` passes, and fails when a declared glob matches nothing.
- Round trip: a "Request change" comment from the review page arrives in `review.json` with the item ID and the file, and the agent applies it to the right file.
- The FunnelFlux pilot (its own repo, not Conveyor) points `.conveyor/planning.yaml` at its `conveyor-brief-review` and sets `.conveyor/docs.yaml` at `docs/knowledge-base/**`, then proves the gate with a behavior-changing pull request.

## Risks

- Reviewer gaming: an agent can omit a finding or accept a weak `docs: none` reason.
  Mitigation: the server now verifies that a declared doc changed or a statement exists (`DEC-P12`); only adequacy remains the reviewer's judgment, reviewed by a different model from the implementer.
- Base-branch read failure: no GitHub App, revoked permission, or transient transport would otherwise block every task.
  Mitigation: fail open with a recorded diagnostic, as decided in `DEC-P8`.
- Skill-refresh overwrite of local playbook edits: the installed copies are CLI-owned and refreshed by `conveyor repo init`.
  This is intended; all edits land in Conveyor source and ship through a release.
- Hook non-determinism: a repo-declared hook can be slow or flaky.
  Mitigation: a timeout and a `blocking` flag; a skipped or failing non-blocking hook never blocks a push.
- Drift tripwire: touching paths governed by the seven baselines listed under System Design documents raises drift.
  Mitigation: each implementing task proposes the baseline revision for its own paths from its claim.

## Migration

- Feature B is off by default: absent `.conveyor/docs.yaml` applies no gate, so every existing repository is unaffected.
- A pinned policy turns the gate on for that repository only.
- Existing verdicts without `documentation_assessment` are unaffected because there is no pinned policy; once a policy is pinned, the assessment becomes required exactly as `requirement_citations` is required for a task with served requirements.
- The task pin defaults to NULL for existing tasks; NULL is the "no policy" state, and tasks already past their first implement claim are never pinned retroactively.

## Rollout

- One CLI release carries the new `conveyor plan` and `conveyor docs` subcommands, the edited skills, and the playbook.
- `conveyor repo init` in each repository refreshes the installed skills and guidance; a repository opts into Feature B by adding `.conveyor/docs.yaml` and into the pre-push hook by adding `.conveyor/planning.yaml`.
- FunnelFlux sets both files in its own repository as the pilot; no FunnelFlux change is a Conveyor task.

## Grill decisions

### Round 1 (2026-10-02)

Each answer is recorded in DEC shape in "Decision proposals"; this list maps questions to records.

- Q1 pin location and time: set-once task field pinned at the first implement claim → `DEC-P8`. The draft's review-snapshot recommendation was rejected after verifying review snapshots resolve at review claim.
- Q2 large grill rounds: separate `conveyor plan ask` sharing the review page renderer → `DEC-P11`.
- Q3 hash scope: ID plus normative fields, normalized, no cascade → `DEC-P3`.
- Q4 changed-docs detection: server precondition from submission evidence plus reviewer judgment → `DEC-P12`.
- Q5 empty-match globs: `conveyor docs validate` error only → `feature-documentation-closure` design text (folded per R2-6).
- Q6 `docs: none` matching: exact literal, line start, agent-authored PR body, same-line reason → `feature-documentation-closure` design text (folded per R2-6).
- Q7 push command: online `conveyor plan push --layer`, `check` stays offline → `DEC-P11`.
- Q8 corpus targets: DEC-34 overlays plus baseline revisions; the draft's `design-*` and `req-260811-0ee057` IDs do not exist in `demo` → "System Design documents".
- Q9 draft location: owner direction, "Store plans locally in a Git-ignored folder that's appropriate for the repository in your local harness"; no harness-specific path in Conveyor code or skills → `DEC-P10`.

Corrections found while verifying the draft: `CapabilityConfirmDocuments` is operator-only, not maintainer; `.orca/` is not git-ignored in this repository; several line ranges were off by a few lines and are fixed in place.

### Round 2 (2026-10-02)

- R2-1 baseline revision timing: each implementing task proposes its own baseline revision in its PR; the push creates only the two overlays → "System Design documents".
- R2-2 draft ignore check: `conveyor plan check` fails when the draft folder is not ignored or holds a tracked file → `DEC-P10`, AC-2.3.
- R2-3 read-failure visibility: task event plus "documentation policy unavailable" in the work-order context; work not blocked → `DEC-P8`.
- R2-4 repository without GitHub: pins explicit "off" with a task event → `DEC-P8`.
- R2-5 hook timing: once before the requirements layer, bound to the normative hashes, re-run only on change → `DEC-P5`, AC-4.2.
- R2-6 DEC count: former `DEC-P13` (docs-none matcher) and `DEC-P14` (empty-match validator rule) become design text; 12 DEC proposals remain.

## Running log

- 2026-10-02: Drafted from the owner's settled decisions and the two FunnelFlux source plans; verified every Conveyor citation against the checkout at `/home/orca/_dev/funnelflux-pro/conveyor`.
- 2026-10-02: Grill round 1 answered; citations re-verified (two factual corrections), `demo` corpus read for design and requirement IDs; round 1 recorded in `DEC-P3`, `DEC-P8`, and `DEC-P10` to `DEC-P12`, with tasks re-split to T1-T12.
- 2026-10-02: Grill round 2 answered and recorded; owner directed that the package go on a review branch and pull request for the core developer instead of staying uncommitted.
