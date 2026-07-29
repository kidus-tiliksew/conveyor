# Phase 6 plan: planning & the knowledge graph (phases 6.1–6.3)

Accepted by §21.46; spec §19 is authoritative for scope and ordering.
The thesis: the factory's compounding value is that task N is cheaper
than task 1. That requires owning planning (so rationale enters the
corpus instead of evaporating in external chat windows) and keeping the
structure the pipeline currently drops — the spec agent's
`decomposition` output is validated, stored, and never read back, and
the curated features tree accretes without driving anything. The
lineage chain is **requirement → blueprint → code → evidence**; no
epic entity.
Sequence 6.1 → 6.2 → 6.3; 6.1 pays off with the existing MCP spec flow
before any chat surface exists.

## Phase 6.1 — Blueprint materialization & dependency-gated claiming

*Proves: one approved spec fans out into ordered, claimable work
(§4.1, §6.3, §8.3). No planning UI required — the existing spec work
order flow already produces decompositions.*

1. **Dependency edges:** `task_dependencies (task_id, depends_on_task_id)`
   with FK + index, acyclic at write time; wire the dormant
   `tasks.parent_task_id` (FK, index) as the blueprint-parent link with
   `(spec_version, sub_id)` origin columns. Migration + sqlc + machine
   module and CHECK templates move together (§21.37 discipline).
2. **Materialization on approval:** a non-empty `decomposition`
   materializes each `SUB-n` into a child task in the approval
   transaction — body from the SUB summary + blueprint reference,
   repo from the SUB, frozen gates and setup inherited, `NextStage:
   implement` (no re-triage), `depends_on` → edges. DAG-validated at
   approval (cycles fail closed — closes the known
   `validateDecomposition` gap). Idempotent per spec version:
   re-approval of a revised blueprint materializes only SUB IDs without
   a live child.
3. **Claim gating:** blocked is a derived predicate (an unmerged
   dependency exists), enforced in `ListClaimable` and the claim
   enforcement layer alongside hold and the self-review guard — never a
   stored state. Scoped by §21.47: the gate applies to **implementation
   orders only, at claim time only** — spec orders stay claimable, and
   an already-claimed order is never rejected mid-flight for blocking.
   Orders queue openly with blocking task IDs visible on every surface
   including worker-facing MCP listings; original queue entry
   preserved, and the queue-timeout clock is **suspended while
   blocked** (§21.47). A dependency that terminates unmerged makes the
   dependent **unsatisfiable**: `task.dependency_unsatisfiable` event,
   distinct rendering, needs-operator tray entry, and an audited
   operator unlink (`task.dependency_removed`; UI/CLI/REST, not MCP) or
   cancel as the only resolutions. Cross-repo edges are legal
   (workspace-scoped, acyclic — intake's same-repo restriction drops,
   §21.47).
4. **Unblock nudge:** on a dependency reaching `merged`, re-enqueue
   each dependent's dispatch so a waiting worker sees the order within
   one poll rather than at `work_order_queue_timeout`.
5. **Parent anchor:** the parent takes no implement order; audited
   control-plane transition to `closed` when the last child reaches a
   terminal state (§3.3 gains exactly this edge).
6. **First lineage edges:** materialization writes blueprint-version →
   child and task → dependency links (the §16 `links` table lands here
   in minimal form; 6.3 generalizes).
7. **UI:** blocked chips naming blocking tasks on card and detail;
   child-progress rollup (n of m merged) on parent tasks. Board stays
   read-only.

**Exit criterion:** a blueprint with at least three SUBs and a
dependency chain materializes on approval; children are claimable only
in dependency order; merging a dependency makes its dependent claimable
within one poll; the parent closes itself when the last child is
terminal; a decomposition-free spec behaves byte-identically to today.

## Phase 6.2 — Planning sessions & requirement documents

*Proves: the factory owns planning; intent goes from conversation to a
confirmed requirement, and to the first of the many blueprints that may
serve it over its life, without leaving the
product (§4.2, §9, §13.3, §17.3).*

1. **Streaming transport:** the AI SDK UI-message protocol over SSE
   served by `conveyord` (`net/http`, no Node sidecar, no new tier) —
   text deltas, tool-call parts, message parts.
2. **Requirement documents:** `requirements` / `requirement_versions`
   (§4.2 item 1) — generated from stated intent by the planning agent,
   revised by chat or drift amendment, each version
   operator-confirmed (audited, no gate); prose + one
   `conveyor:requirements` block with stable `REQ-n` statement IDs;
   flat corpus, `relates_to` links only.
3. **Planning agent:** in-process on the factory credential (like
   triage), tool loop over: requirement corpus + approved-spec reads,
   artifact reads, lineage-link reads, and draft / revise / finalize
   for both artifact types. Finalizing a blueprint creates the parent
   task + spec version through
   unchanged §4.1 validation and the unchanged §13.1 spec gate — and
   proposes the `serves` link when drafted in a requirement's context;
   the session grants no approval authority.
4. **Durable sessions:** `planning_sessions` rows (status, message log,
   finalized requirement/parent-task link); on finalize the transcript
   archives as an artifact linked to what it produced — rationale
   becomes lineage.
5. **Features-tree migration:** nodes with accumulated approved specs
   seed requirement documents (accumulated text = first version), empty
   nodes drop, `tasks.feature_id` assignments convert to history links,
   `triage.feature_suggested` retires in favor of a proposed
   requirement link. Artifact attachment re-homes to requirements and
   tasks.
6. **UI:** the Requirements view replaces the tree — flat list of
   living documents with confirmed version, staleness badge, serving
   blueprints, and lineage — plus session list + chat built on the
   stock shadcn chat family (MessageScroller, Message, Bubble,
   Attachment, Marker); tool activity rendered as markers; attachments
   flow into the ordinary artifact machinery. No epic surface: the
   blueprint parent task carries the child rollup.
7. **Headless twin unchanged:** MCP `create_task` + `submit_spec`
   reaches the identical blueprint contract; no planning-only MCP
   surface in this phase.

**Exit criterion:** a real feature is planned entirely in-product —
intent stated in chat → generated requirement confirmed → blueprint
drafted in its context and approved → materialized children shipped
through the unchanged pipeline — with the `serves` link recorded, the
planning transcripts attached as artifacts, the features tree migrated
and its API/UI removed, and no external chat tool involved.

## Phase 6.3 — Lineage links & graph context assembly

*Proves: the factory can answer "why does this code exist" and "what
context does this work order need" by traversal, not archaeology
(§4.2 item 4, §15.1, §16).*

1. **`links` generalized:** polymorphic
   `(src_type, src_id, dst_type, dst_id, kind, created_by_event)`;
   edges written only by pipeline machinery at stage transitions —
   planning session → requirement/blueprint, requirement → blueprint
   (`serves`), requirement → requirement (`relates_to`), blueprint →
   child, task → dependency,
   task → PR/commit range (at submit and merge), evidence → verdict,
   spec/requirement version → superseded predecessor. Rebuildable
   projection of
   `events`; a backfill projector derives historical edges where the
   event corpus supports them.
2. **`REQ-n` citations:** the `(spec §N)` convention generalized to
   requirement-statement IDs cited in code comments by implementing
   agents; review checks citations against the governing spec and
   served requirements (role-prompt + validation, not a parser).
   Derived file/symbol maps are recomputed cache, never asserted.
3. **Context assembly:** work-order and planning-agent context becomes
   a link traversal under an explicit depth/size budget — served
   requirement, parent blueprint section, sibling outcomes, adjacent
   evidence — replacing feature-scoped artifact injection (retired
   with the tree in 6.2; requirement/task attachment is the successor).
4. **Surfacing:** lineage rendered on task detail and requirement
   documents (chain: session → requirement → blueprints → children →
   PRs → evidence); per-requirement staleness driven by link-aware
   merge/drift queries.

**Exit criterion:** for a merged Phase-6 task, the full chain —
planning session → requirement → blueprint version → child task →
work order → PR
and commits → evidence → verdict — is queryable end-to-end from the
API, and a newly dispatched child's work-order context demonstrably
includes link-derived material (sibling outcome or parent rationale)
that feature-scoped injection alone would not have selected.

---

## Deferred, explicitly

- **Branch stacking** (§8.3): v1 dependencies are ordering gates;
  dependents branch from a base that already contains their merged
  dependencies. Stacked branches + rebase tasks return only by
  amendment.
- **Memory MCP tools** (`get_memories`/`store_memory`): Phase 7 —
  recall over the lineage graph, pgvector as secondary index (§15.1).
- **Task priority:** still no priority field (§6.3); dependency order
  and FIFO are the only ordering inputs.
- **Cross-repo dependency edges:** Phase 9 with multi-repo
  coordination (§7.2); Phase 6 edges are workspace-local tasks only.
- **Automatic corpus/requirements edits:** reverse sync keeps filing
  reconciliation tasks (§4.2); drift proposes requirement versions,
  humans confirm — nothing in Phase 6 edits confirmed requirements
  without a human.
- **Requirement hierarchy / epic entity:** the corpus stays flat
  (`relates_to` links only) and there is no epic object or page — the
  blueprint parent task is the mechanical carrier (§21.46 change 7).

## Standing notes

- One queue, hold semantics, and the §21.31 contract are untouched;
  blocked is a predicate in the same enforcement layer as hold.
- The §21.4 boundary holds: planning and triage are Conveyor-owned;
  implementation and review stay delegated over MCP.
- `events` stays append-only and authoritative; every link is a
  projection with `created_by_event` provenance — no free-standing
  volunteered edges, no graph database, embeddings never the primary
  structure.
