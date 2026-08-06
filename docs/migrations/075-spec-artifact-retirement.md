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

The applied backfill selected each task's latest job with `ORDER BY
j.started_at DESC`. PostgreSQL sorts NULL values first for a descending order,
so a never-started job could have hidden the actual spec-stage job. Migration
075 is immutable history; future migrations that reuse this lookup must order
with `j.started_at DESC NULLS LAST, j.id DESC`.

Audit an applied database for awaiting tasks whose captured status may differ
from the NULL-safe latest-job lookup:

```sql
SELECT t.workspace_id,
       t.id AS task_id,
       latest_job.stage AS null_safe_latest_stage,
       legacy.spec_version AS captured_spec_version
FROM tasks t
LEFT JOIN LATERAL (
  SELECT j.stage
  FROM jobs j
  WHERE j.task_id = t.id
  ORDER BY j.started_at DESC NULLS LAST, j.id DESC
  LIMIT 1
) latest_job ON true
LEFT JOIN legacy_spec_gate_versions legacy
  ON legacy.workspace_id = t.workspace_id
 AND legacy.task_id = t.id
WHERE t.state = 'awaiting_human'
  AND (latest_job.stage = 'spec') IS DISTINCT FROM (legacy.task_id IS NOT NULL)
ORDER BY t.workspace_id, t.id;
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
