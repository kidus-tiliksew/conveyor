-- component-production-feedback invariant 10, feature-production-feedback-live
-- FB-4, and req-production-feedback AC-1.5. A GitHub deployment report names
-- its service after the configured repository, and a configured repository
-- name has no 100-character limit. The service of a deploy therefore takes the
-- bound that the SingleStore deploy table already has, 1 to 255 characters. A
-- watcher signal keeps its own 100-character service rule, which the store
-- checks before any write (invariant 3). No row changes, and no backfill.
ALTER TABLE feedback_deploys DROP CONSTRAINT IF EXISTS feedback_deploys_service_check;
ALTER TABLE feedback_deploys ADD CONSTRAINT feedback_deploys_service_check CHECK (char_length(service) BETWEEN 1 AND 255);
