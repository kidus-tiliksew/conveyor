-- An issue publication still open when an operator turns workspace issue
-- publication off ends skipped, with outcome skipped, and makes no forge
-- write (req-delivery-and-forge AC-8.4; component-git-delivery).
ALTER TABLE github_lifecycles DROP CONSTRAINT IF EXISTS github_lifecycles_state_check;
ALTER TABLE github_lifecycles
    ADD CONSTRAINT github_lifecycles_state_check
    CHECK (state IN ('queued', 'retrying', 'published', 'failed', 'skipped'));

ALTER TABLE github_lifecycles DROP CONSTRAINT IF EXISTS github_lifecycles_outcome_check;
ALTER TABLE github_lifecycles
    ADD CONSTRAINT github_lifecycles_outcome_check
    CHECK (outcome IN ('', 'created', 'reused', 'skipped'));
