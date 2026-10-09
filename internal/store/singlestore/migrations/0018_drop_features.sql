-- Task 261007-9d50e0 (component-persistence, component-artifacts,
-- component-lineage). migrateFeatureRetirement first detaches residual
-- feature-owned attachment links into workspace-unattached links and drops
-- artifact_links.feature_id and tasks.feature_id behind information_schema
-- checks. The retired table then drops, and IF EXISTS keeps a retry after this
-- implicit DDL commit safe. Recorded events and links are unchanged.
DROP TABLE IF EXISTS features;
