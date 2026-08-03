# Phase 8 plan: the desired-state document model (phases 8.1–8.4)

Accepted by §21.58; spec §19 is authoritative for scope and ordering.
The thesis: durable documents are the truth — requirements for intent,
System Design for mechanism — and tasks are the reconciliation work
that moves code toward them, each carrying its own execution plan.
The per-change spec artifact retires; every function it performed
relocates (§21.58 change 2's table is the checklist). Entered only
after Phase 6 closes. Sequence 8.1 → 8.2 → 8.3 → 8.4; 8.1 and 8.2
are additive (nothing retires until 8.3).

## Phase 8.1 — Requirements v2 & product overview

*Proves: requirements are independently verifiable state, and the
informative tier feeds planning without pretending to be normative.*

1. **REQ→AC structure**: REQ entries gain optional user-story framing
   (`As a…, I want…, so that…`) and nested acceptance criteria
   (`AC-n.m`, "when X, the system shall Y"). Additive over the
   existing statement corpus — statement-only documents stay valid.
   Fence schema, high-water-mark discipline, and citation machinery
   extend to nested IDs; review contracts reference requirement ACs.
2. **Product overview uploads**: markdown-only `reference_document`
   artifacts; flexible operator-named set (suggested starter titles,
   no enforced taxonomy); re-upload supersedes with rendered diffs;
   document nodes + planning-session `consulted` edges in the graph;
   top planning-context slot under existing budgets; §18 posture.
   Citations and staleness never target uploads.
3. **Promotion**: a planning action turning an informative passage
   into a normative REQ/AC with a section-anchored `derived_from`
   link; the agent proposes promotions when uploads contain
   enforceable claims.

**Exit criterion:** a requirement drafted with user stories and nested
ACs is confirmed and cited; an uploaded overview document is consulted
by a session, superseded by re-upload with a visible diff, and one of
its claims is promoted into a requirement with the anchor link.

## Phase 8.2 — System Design corpus & decision records

*Proves: mechanism truth lives in maintained documents that agents
read, propose against, and are held to.*

1. **System Design documents**: factory-resident, versioned markdown
   in operator-named categories (SDD, architecture, LLD, database
   design, API contracts, …). Propose→confirm like requirements —
   agents and operators propose, operator confirms; no freehand
   editor. Rendered in a System Design surface beside Requirements.
2. **Freshness enforcement**: merges touching a component without
   touching its governing documents raise a drift signal (link-aware,
   same family as requirement staleness); review may demand document
   updates in the same change (role-prompt + validation, the REQ-n
   citation pattern applied to `governs` links).
3. **Decision records**: `DEC-n`, append-only with supersession,
   machinery-proposed from planning and implementation deliberation
   (the transcripts already contain the alternatives-rejected
   material), human-confirmed, citable in code and documents.
4. Graph: `governs` (design doc → component/task), `consulted`,
   `derived_from`, `supersedes` edges; document nodes labeled and
   traversable.

**Exit criterion:** a System Design document proposed by a planning
session is confirmed; a merge touching its component without a doc
update raises the drift signal; a decision extracted from a session is
confirmed and cited from code.

## Phase 8.3 — Task-centric delivery

*Proves: the factory runs without the per-change spec artifact and
nothing the spec did is lost. The riskiest slice — everything before
it is additive; this one retires machinery.*

1. **Context attachment at intake** (all surfaces): tasks reference
   the requirements they serve (`serves` at task level) and the
   System Design documents that govern them, alongside artifacts.
2. **Execution plans on the existing stage machinery**: the spec
   stage survives re-contented — a dispatched, stage-typed work order
   before implementation when the §13.1 first toggle (renamed plan
   approval) or routing calls for it; the claiming agent (not
   necessarily the implementer) reads attached context and submits a
   markdown plan — approach, files, ordering, risks, done-criteria.
   Versioned; gate approval, redirect-to-revision, auto-approval with
   the gate off, and gate-off direct-to-implement routing are all
   byte-compatible with today's spec-gate semantics. `submit_plan`
   succeeds `submit_spec` (wire compatibility handled here). Plans
   are task-scoped lineage artifacts.
3. **Planning bundles**: finalize proposes document revisions + a
   task set with dependencies; operator bundle approval creates the
   work. Blueprint-goal sessions and decomposition materialization
   retire; the 6.1 dependency machinery is the substrate, unchanged.
4. **Retirements**: §4.1 fence formats, blueprint-goal finalize,
   materialization path. The stage itself is retained. Historical
   blueprint tasks/events/nodes preserved read-only. Migration for
   in-flight tasks defined before landing.
5. Review judges: cited requirement ACs + plan done-criteria + diff;
   citation contracts updated; a task citing nothing must carry
   done-criteria in its plan (validated).

**Exit criterion:** a real feature flows intent → requirement →
bundle (design deltas + task set) → per-task exec plans through the
plan stage (one gated, approved after a redirect round) →
implementation → review against ACs and plan done-criteria → merge —
with no §4.1 fences produced anywhere and the full chain queryable in
the graph.

## Phase 8.4 — Tasks view & simplification

*Proves: operating the new model is legible.*

1. **Tasks view**: list-first management — filters, dependency and
   blocking columns, child rollups, attached-context links, plan
   status. The view imports no barred fields: no priority, no
   assignee, no declared phases (§6.3, §21.31, §21.46).
2. Staleness and lineage simplification to the task-level chain;
   Blueprints surface becomes a historical lens or folds into Tasks;
   board relationship decided by use (retain as pipeline lens).
3. Cleanup: retired code paths removed, docs/CLAUDE.md updated,
   §21.40-style body restatement of §§4, 9, 13 if drift between body
   and practice warrants it.

**Exit criterion:** the Tasks view is the daily operating surface for
a multi-task delivery with dependencies, plans, and context visible;
no retired machinery remains reachable.

---

## Deferred, explicitly

- Priority, assignee, and declared phase fields (§6.3 —
  amendment-gated, twice reaffirmed).
- Freehand editing of any normative tier; PDF/Word ingestion.
- Repo-resident System Design (reconsider after operating the
  factory-resident corpus).
- Agent-minted child tasks from implement sessions (fan-out stays in
  planning).
- Branch stacking (§8.3, still deferred).

## Standing notes

- Both §13.1 gates survive: bundle/plan approval (relocated first
  toggle) and merge approval. Neither thins without an amendment.
- The §21.4 boundary holds: planning and documents are
  Conveyor-owned; implementation and review stay delegated over MCP.
- One queue, derived state, workers claim; blocked stays a derived
  predicate.
- Events append-only; every new edge kind ships with projector,
  conformance case, and vocabulary-agreement coverage on day one —
  the 6.3 lesson, now a rule.
