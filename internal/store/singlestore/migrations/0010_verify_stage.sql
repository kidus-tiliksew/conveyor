-- DEC-43, feature-verification-kit-execution VK-2. SingleStore's work-order
-- stage vocabulary is enforced by core.ValidWorkOrderStage at the Go write
-- boundary. No rows or schema fields need backfilling.
SELECT 1;
