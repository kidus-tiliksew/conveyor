-- req-verification-evidence REQ-2/AC-2.1 and component-verification-evidence.
-- migrateVerificationChunkExpiryIndex creates the ordered
-- verification_upload_chunks_expiry (workspace_id, expires_at, id) index with
-- a restart-safe information_schema check before this file is recorded.
SELECT 1;
