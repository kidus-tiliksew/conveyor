-- Keep the dashboard's high-frequency event projections on the small subset
-- of event kinds each projection consumes. The workspace-leading keys serve
-- the attention CTEs directly; task/time keys also serve the task-joined
-- activity and review-lifecycle projections.
CREATE INDEX events_forge_polling_idx
    ON events (workspace_id, task_id, at DESC, id DESC)
    WHERE kind IN (
        'github_issue.publication_failed',
        'github_issue.publication_published',
        'review.publication_failed',
        'review.publication_published',
        'merge.failed',
        'merge.confirmed',
        'merge.reconciled'
    );

CREATE INDEX events_review_lifecycle_polling_idx
    ON events (workspace_id, task_id, at DESC, id DESC)
    WHERE kind IN (
        'work_order.claimed',
        'work_order.lease_renewed',
        'work_order.released',
        'review.completed',
        'review.accepted',
        'task.setup.changed'
    );

CREATE INDEX events_review_round_polling_idx
    ON events (workspace_id, task_id, at DESC, id DESC)
    WHERE kind = 'review.round_completed';

CREATE INDEX events_merge_conflict_polling_idx
    ON events (workspace_id, task_id, at DESC, id DESC)
    WHERE kind IN (
        'merge.blocked',
        'merge.conflict_cleared',
        'merge.conflict_fix_dispatched',
        'merge.conflict_recovery_blocked',
        'merge.conflict_dispatch_exhausted'
    );
