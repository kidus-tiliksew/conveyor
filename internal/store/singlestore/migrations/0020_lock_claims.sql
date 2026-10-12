-- Task 261012-832d9c. migrateLockClaims adds conveyor_locks.holder
-- VARCHAR(255) NOT NULL DEFAULT '' behind an information_schema check because
-- SingleStore DDL commits implicitly and does not support ADD COLUMN IF NOT
-- EXISTS. Unique write rules record their claim on the locked row. Rows written
-- before this migration have no claim and stay covered by the rules' counts.
SELECT 1;
