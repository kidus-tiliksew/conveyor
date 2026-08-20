-- Unified, operator-confirmed task-context proposal lifecycle (REQ-7/AC-7.1-7.4, DEC-25).
CREATE TABLE task_context_proposals (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    task_id text NOT NULL,
    target_kind text NOT NULL CHECK (target_kind IN ('requirement','system_design')),
    target_id text NOT NULL,
    target_title text NOT NULL,
    state text NOT NULL CHECK (state IN ('proposed','confirmed','dismissed')),
    source text NOT NULL CHECK (source IN ('planning','triage','operator')),
    justification text NOT NULL CHECK (btrim(justification) <> ''),
    created_by_event_id bigint NOT NULL,
    decision_event_id bigint,
    proposed_by text NOT NULL,
    decided_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, task_id, target_kind, target_id),
    FOREIGN KEY (workspace_id, task_id) REFERENCES tasks(workspace_id,id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, created_by_event_id) REFERENCES events(workspace_id,id),
    FOREIGN KEY (workspace_id, decision_event_id) REFERENCES events(workspace_id,id),
    CHECK ((state='proposed' AND decision_event_id IS NULL AND decided_by='') OR
           (state IN ('confirmed','dismissed') AND decision_event_id IS NOT NULL AND decided_by<>''))
);

INSERT INTO task_context_proposals (
    workspace_id,task_id,target_kind,target_id,target_title,state,source,justification,
    created_by_event_id,decision_event_id,proposed_by,decided_by,created_at,updated_at
)
SELECT l.workspace_id,l.blueprint_task_id,'requirement',l.requirement_id,r.title,l.state,l.source,
       'Migrated requirement context suggestion.',l.created_by_event_id,l.decision_event_id,
       l.proposed_by,l.decided_by,l.created_at,l.updated_at
FROM requirement_serves_links l
JOIN requirements r ON r.workspace_id=l.workspace_id AND r.id=l.requirement_id
ON CONFLICT DO NOTHING;

CREATE INDEX task_context_proposals_pending_idx
    ON task_context_proposals (workspace_id,state,task_id,target_kind,target_id);

DROP TABLE requirement_serves_links;
