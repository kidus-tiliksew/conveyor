-- The lineaged_merge observation kind landed in Go (PR #279 relabeled the
-- factory's own merges away from external_pr_merge) without extending the
-- kind CHECKs minted in 040, so every own-merge design-drift evaluation
-- fails at the observation insert. Extend both tables; the Go/DB kind
-- vocabulary is guarded by TestMonitorKindVocabularyMatchesConstraints.
ALTER TABLE monitor_observations DROP CONSTRAINT monitor_observations_kind_check;
ALTER TABLE monitor_observations ADD CONSTRAINT monitor_observations_kind_check CHECK (
    kind IN ('post_merge_failure','direct_push','external_pr_merge','lineaged_merge','revert'));
ALTER TABLE repository_drift DROP CONSTRAINT repository_drift_kind_check;
ALTER TABLE repository_drift ADD CONSTRAINT repository_drift_kind_check CHECK (
    kind IN ('direct_push','external_pr_merge','lineaged_merge','revert'));
