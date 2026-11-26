-- component-production-feedback invariant 10, feature-production-feedback-live
-- FB-4, and req-production-feedback AC-1.5. A GitHub deployment report names
-- its service after the configured repository, and the workspace
-- configuration bounds no repository name's length. The service of a deploy
-- therefore has no upper bound here, and the column is text. A watcher signal
-- keeps its own 100-character service rule, which the store checks before any
-- write (invariant 3). No row changes, and no backfill.
ALTER TABLE feedback_deploys DROP CONSTRAINT IF EXISTS feedback_deploys_service_check;
ALTER TABLE feedback_deploys ADD CONSTRAINT feedback_deploys_service_check CHECK (char_length(service) >= 1);
