-- component-feedback-reports invariant 15, feature-production-feedback-listening
-- FR-2, and approved plan version 2 of task 261127-6d47db. The time of a
-- report's last linking attempt. The background linking work claims one
-- attempt for one report no more than once a minute: under the report's row
-- lock it compares the exact attempt time with this stored time, and it writes
-- the new time only when the claim succeeds. NULL means that no attempt was
-- claimed yet. No backfill: every stored report starts with no attempt.
ALTER TABLE feedback_reports ADD COLUMN IF NOT EXISTS linking_last_attempt_at timestamptz NULL;

-- Approved plan version 3. The stored progress of a report's linking, so that
-- a fresh worker continues from the store alone (component-feedback-reports
-- invariants 12 and 15). line_progress holds the answer of each candidate
-- repository's commit read (a full commit or a GitHub-proven absence) with
-- the digest of the one resolution input they were read with, only for the
-- current candidate repositories, and then the resolved repository and
-- commit or the definitive unresolved reason. timing_result holds the timing rule's answer once it ended: its
-- links by task and deploy ID and its notes by deploy ID. Each definitive
-- result is written once under the report row lock and never replaced;
-- interim answers for another input or a repository that is no longer a
-- candidate are dropped; and a retryable failure writes nothing. Neither column holds report text, and no
-- route or served read exposes either. NULL means that nothing was reached.
ALTER TABLE feedback_reports ADD COLUMN IF NOT EXISTS line_progress jsonb NULL;
ALTER TABLE feedback_reports ADD COLUMN IF NOT EXISTS timing_result jsonb NULL;
