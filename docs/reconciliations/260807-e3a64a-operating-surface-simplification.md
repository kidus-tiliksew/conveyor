# Reconciliation: operating-surface simplification

## Signal

- Task: `260807-e3a64a`
- Out-of-pipeline commit: `e6297884daa501add1fde4239beba3b9e683032d`
- Source: <https://github.com/kidus-tiliksew/conveyor/commit/e6297884daa501add1fde4239beba3b9e683032d>
- Recorded intent: spec §21.61, v2.21, dated August 7, 2026
- Resolution: pending operator confirmation and an audited reconciliation outcome

The commit changes only `conveyor-spec.md`. It labels v2.21 accepted, adds the
§13.3 operating-surfaces paragraph, adds the §21.61 amendment, and updates the
closing summary. Its stated intent is presentation-only: document-centric
Requirements and System Design surfaces, one attention surface per document,
an on-demand grouped lineage explorer, parked Planning and Blueprint-history
navigation, and consolidated Tasks and Board behavior. It explicitly leaves
gates, lifecycle, graph persistence and reads, authority, propose-to-confirm,
and deep-link behavior unchanged.

The direct commit and its acceptance wording are the monitor signal, not by
themselves authority to revise confirmed requirements, System Design, or the
durable repository-drift outcome. This record therefore preserves the exact
signal and checks the implementation gap without modifying those authorities.

## Verified presentation gaps

The current UI still presents behavior that §21.61 parks or replaces:

- `web/src/components/app-shell.tsx` renders both **Blueprint history** and
  **Planning** in primary navigation. The existing `useBlueprints` machinery
  and canonical blueprint routes can remain while the navigation entry is
  parked.
- `web/src/pages/requirements.tsx` still starts planning sessions, renders the
  always-present requirement assistant, and repeats pending-confirmation,
  drift, staleness, and pending-version state across the corpus, version area,
  and detail cards instead of one actionable per-document attention surface.
  It also renders lineage through the existing graph card rather than an
  on-demand grouped explorer.
- `web/src/pages/system-design.tsx` still starts and embeds planning chat and
  renders design drift, pending versions, and pending confirmation in separate
  regions rather than one per-document attention surface.
- `web/src/components/lineage/lineage-graph-card.tsx` presents node/link counts
  and an edge-by-edge graph trace. `web/src/components/task/task-sheet.tsx` and
  `web/src/pages/task-full.tsx` render that card inline. §21.61 instead calls
  for a corner affordance that opens grouped related entities in a right panel
  over the existing §16 read API, with no graph visualization.
- `web/src/pages/tasks.tsx` fetches the complete projection and applies only
  client-side text, state, and repository filters. It has no creation-in-view,
  server pagination, updated-at/requirement/design filters, or permalinked
  right detail panel.
- `web/src/components/board/board.tsx` has only client-side text search, keeps
  task creation on the Board, and does not provide the shared updated-at,
  requirement, and design filter family or the recent-activity default.

These paths identify the verified delivery surface; they are not authorization
to implement it in this reconciliation task. Navigation-only edits would also
be incomplete because the gap spans document, lineage, task, and board
presentation.

## Operator checkpoint

Before implementation or drift resolution proceeds, the operator must:

1. Confirm whether commit `e6297884daa501add1fde4239beba3b9e683032d` is the
   intentional v2.21 desired state.
2. Select the audited repository-drift and governance outcome for the direct
   push.
3. If the intent is retained, confirm or create the governing requirement and
   System Design documents and approve a dependency-ordered set of ordinary
   Conveyor delivery tasks.

This work order supplies no served requirement or UI-governing System Design
document that authorizes inventing those links. Its pinned work-order lifecycle
design governs backend work-order and MCP paths, not the presentation paths
listed above. This record does not select an outcome, resolve the drift, or
silently amend any approved authority document.

## Follow-on boundary

Operator-approved follow-on tasks may remove the two parked navigation entries,
consolidate each document's attention signals, replace inline graph
presentation with the grouped lineage explorer, and implement the shared
Tasks/Board creation, pagination, detail-panel, and filter behavior. They must
preserve propose-to-confirm, approval gates, lifecycle state, lineage
persistence and §16 read semantics, blueprint records and deep links, and the
§6.3 prohibition on priority, assignee, and declared-phase fields. Each task
must run and record the repository's applicable validation gates.
