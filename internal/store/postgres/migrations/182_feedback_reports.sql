-- component-feedback-reports, feature-production-feedback-listening FR-1,
-- req-production-feedback REQ-4 and REQ-11, req-environments AC-2.4, and
-- DEC-149(1). One record for each report, its links with their evidence, its
-- notes, and the answer of each line read. Nothing is stored for each
-- occurrence. No dispatch, claim, gate, review, merge, lineage, next-action,
-- or work-order query reads these tables (component-feedback-reports
-- invariant 1). Every table is keyed to its workspace and cascades with it
-- (component-persistence invariants 21 and 34). An empty reported value was
-- not stated. No backfill.

-- One row for each report. key_digest is the SHA-256 of the exact problem
-- key, so no collation decides which keys are equal. first_seen is the
-- earliest first-seen time of every entry, and first_entry_seen is the first
-- entry's own first-seen time, which no later entry changes
-- (component-feedback-reports invariants 6, 11, and 13). summary is redacted
-- and summary_redactions counts its removals by pattern class (invariant 5).
CREATE TABLE IF NOT EXISTS feedback_reports (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    id text NOT NULL,
    environment_id text NOT NULL,
    key_digest text NOT NULL CHECK (char_length(key_digest) = 64),
    problem_key text NOT NULL CHECK (char_length(problem_key) BETWEEN 1 AND 200),
    kind text NOT NULL CHECK (kind IN ('error', 'alert', 'message')),
    summary text NOT NULL CHECK (char_length(summary) >= 1),
    summary_redactions jsonb NOT NULL,
    service text NOT NULL CHECK (char_length(service) <= 100),
    occurrence_count bigint NOT NULL CHECK (occurrence_count >= 1),
    first_seen timestamptz NOT NULL,
    last_seen timestamptz NOT NULL,
    first_entry_seen timestamptz NOT NULL,
    file text NOT NULL CHECK (char_length(file) <= 500),
    line integer NULL CHECK (line BETWEEN 1 AND 1000000),
    first_repository text NOT NULL,
    first_commit text NOT NULL CHECK (char_length(first_commit) <= 40),
    first_version text NOT NULL CHECK (char_length(first_version) <= 200),
    latest_repository text NOT NULL,
    latest_commit text NOT NULL CHECK (char_length(latest_commit) <= 40),
    latest_version text NOT NULL CHECK (char_length(latest_version) <= 200),
    state text NOT NULL CHECK (state IN ('open', 'closed')),
    in_inbox boolean NOT NULL,
    source text NOT NULL CHECK (source = 'report'),
    source_label text NOT NULL,
    actor_id text NOT NULL,
    created_at timestamptz NOT NULL,
    closed_at timestamptz NULL,
    closed_by text NULL,
    close_reason text NULL,
    reopened_at timestamptz NULL,
    reopen_source text NULL CHECK (reopen_source = 'report'),
    reopen_source_label text NULL,
    reopen_actor_id text NULL,
    linking_ended_at timestamptz NULL,
    fix_task_id text NULL,
    fix_task_set_by text NULL,
    fix_task_set_at timestamptz NULL,
    PRIMARY KEY (workspace_id, id),
    UNIQUE (workspace_id, environment_id, key_digest),
    FOREIGN KEY (workspace_id, environment_id) REFERENCES environments(workspace_id, id) ON DELETE CASCADE,
    CHECK (first_seen <= last_seen),
    CHECK ((file = '') = (line IS NULL)),
    CHECK ((state = 'closed') = (closed_at IS NOT NULL)),
    CHECK (num_nonnulls(closed_at, closed_by, close_reason) IN (0, 3)),
    CHECK (num_nonnulls(reopened_at, reopen_source, reopen_source_label, reopen_actor_id) IN (0, 4)),
    CHECK (num_nonnulls(fix_task_id, fix_task_set_by, fix_task_set_at) IN (0, 3)),
    CHECK (state = 'open' OR NOT in_inbox)
);

-- The open-report count of an environment (component-feedback-reports
-- invariant 8) and the open lists read this.
CREATE INDEX IF NOT EXISTS feedback_reports_environment_state_idx
    ON feedback_reports (workspace_id, environment_id, state);

-- One row for each link of a report to a task or a crate, with its evidence.
-- A removed link stays, so no rule links that task again
-- (component-feedback-reports invariants 11, 16, and 17).
CREATE TABLE IF NOT EXISTS feedback_report_links (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    id text NOT NULL,
    report_id text NOT NULL,
    task_id text NULL,
    crate_id text NULL,
    state text NOT NULL CHECK (state IN ('possible', 'confirmed', 'removed')),
    evidence jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    created_by text NOT NULL,
    confirmed_by text NULL,
    confirmed_at timestamptz NULL,
    removed_by text NULL,
    removed_at timestamptz NULL,
    PRIMARY KEY (workspace_id, id),
    FOREIGN KEY (workspace_id, report_id) REFERENCES feedback_reports(workspace_id, id) ON DELETE CASCADE,
    CHECK (num_nonnulls(task_id, crate_id) = 1),
    CHECK (num_nonnulls(confirmed_by, confirmed_at) IN (0, 2)),
    CHECK (num_nonnulls(removed_by, removed_at) IN (0, 2))
);

CREATE UNIQUE INDEX IF NOT EXISTS feedback_report_links_task_idx
    ON feedback_report_links (workspace_id, report_id, task_id) WHERE task_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS feedback_report_links_crate_idx
    ON feedback_report_links (workspace_id, report_id, crate_id) WHERE crate_id IS NOT NULL;

-- One row for each note of a report (component-feedback-reports invariants
-- 12, 14, and 15). detail holds what the note names.
CREATE TABLE IF NOT EXISTS feedback_report_notes (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    id text NOT NULL,
    report_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('too_many_tasks', 'no_task_deploy', 'deploy_not_compared', 'file_not_found', 'commit_unresolved', 'not_checked')),
    detail jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, id),
    FOREIGN KEY (workspace_id, report_id) REFERENCES feedback_reports(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS feedback_report_notes_report_idx
    ON feedback_report_notes (workspace_id, report_id);

-- The answer of the line read of a report: which commit last changed the
-- line of the file as of the resolved commit. An answered read is never asked
-- again (component-feedback-reports invariants 12 and 15).
CREATE TABLE IF NOT EXISTS feedback_report_line_reads (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    report_id text NOT NULL,
    repository text NOT NULL,
    resolved_commit text NOT NULL CHECK (char_length(resolved_commit) <= 40),
    file text NOT NULL CHECK (char_length(file) BETWEEN 1 AND 500),
    line integer NOT NULL CHECK (line BETWEEN 1 AND 1000000),
    answer jsonb NOT NULL,
    answered_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, report_id),
    FOREIGN KEY (workspace_id, report_id) REFERENCES feedback_reports(workspace_id, id) ON DELETE CASCADE
);

-- A report change appends one taskless workspace event
-- (component-feedback-reports invariant 20). This keeps every kind migration
-- 177 admits and admits the seven report kinds.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_scope_check;
ALTER TABLE events ADD CONSTRAINT events_scope_check CHECK (
  task_id IS NOT NULL OR (kind IN ('artifact.metadata_repaired',
    'workspace.github_app_connected','workspace.github_app_replaced','workspace.github_app_disconnected','workspace.github_app_installation_recorded',
    'workspace.github_webhook_secret_set',
    'config.updated','workspace.created','workspace.membership_granted','workspace.membership_revoked','workspace.invitation_resent','identity.legacy_token_rotated',
    'worker.pairing_issued','worker.enrolled','worker.revoked','worker.heartbeat',
    'requirement.created','requirement.version_proposed','requirement.version_confirmed','requirement.version_retired','requirement.version_dismissed','requirement.staleness_acknowledged',
    'requirement.archived','requirement.restored','requirement.title_changed',
    'planning_session.created','planning_session.message_appended','planning_session.finalized','planning_session.abandoned','planning_session.repo_pinned',
    'planning_bundle.finalized','planning_bundle.revised','planning_bundle.approved','planning_bundle.rejected',
    'reference_document.created','reference_document.superseded','reference_document.deleted','reference_document.consulted',
    'system_design.created','system_design.version_proposed','system_design.version_confirmed','system_design.version_dismissed','system_design.consulted','system_design.drift_detected','system_design.drift_resolved',
    'system_design.archived','system_design.restored','system_design.title_changed',
    'decision.proposed','decision.confirmed','decision.dismissed',
    'decision.supersession_sweep_opened','decision.supersession_sweep_reopened','decision.supersession_sweep_auto_cleared','decision.supersession_sweep_dismissed',
    'migration.feature_node_dropped','migration.requirement_reference_repaired','lineage.vocabulary_repaired',
    'lineage.historical_fabrication_recorded','lineage.pull_request_identity_repaired','lineage.rebuilt',
    'crate.created','crate.updated','crate.closed','crate.reopened',
    'environment.created','environment.updated','environment.archived','environment.restored',
    'environment.key_issued','environment.key_revoked','environment.suggestion_ignored',
    'feedback_report.opened','feedback_report.reopened','feedback_report.linked','feedback_report.link_confirmed',
    'feedback_report.link_removed','feedback_report.closed','feedback_report.fix_task_set'
  ) AND job_id IS NULL)
) NOT VALID;
