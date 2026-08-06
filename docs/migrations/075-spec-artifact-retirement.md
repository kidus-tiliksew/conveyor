# Migration 075 — spec artifact retirement

Migration `075_legacy_spec_gate_snapshot.sql` captures the finite set of
unapproved legacy spec versions at the plan gate when the retirement flip is
applied. It never accepts later inserts. An approval may complete and
materialize an exact captured version; redirect is terminal for that version,
and the next dispatched stage accepts only an execution plan. Historical
specs, blueprint tasks, events, children, and lineage are not rewritten.

The migration table is the authoritative flip-time enumeration:

```sql
SELECT workspace_id, task_id, spec_version
FROM legacy_spec_gate_versions
ORDER BY workspace_id, task_id, spec_version;
```

Before landing, the worker-visible MCP census for workspace `demo` contained
no active spec-stage work order. The durable migration query above remains the
required source for the exact gate-resident set because awaiting human gates
are intentionally absent from `list_work_orders`.

## §21.58 relocation verification

| Retired spec-artifact function | Landed successor |
| --- | --- |
| Acceptance criteria | Confirmed requirement `REQ-n` statements and nested `AC-n.m` criteria attached to tasks |
| Approach | Confirmed System Design revisions proposed before delivery |
| Decomposition and ordering | Operator-approved planning bundles creating tasks over the unchanged dependency machinery |
| Change rationale | Task description, attached desired-state context, and durable planning transcript |
| Per-change plan | Versioned Markdown execution plan submitted through `submit_plan` on the existing stage/gate machinery |
| Requirement association | Task-level `requirement_ids` / bundle task context and task-level `serves` lineage |
| Blueprint presentation | Read-only Blueprint history lens; no creation affordance |
