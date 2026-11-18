-- component-persistence, Read migration invariants. The Go hook adds the
-- approved equality and paging keys only to a recorded fresh bootstrap.
-- Existing installations record this version without automatic ALTERs;
-- operators prepare them with docs/sql/hot-reads/prepare-singlestore.sql.
-- The bootstrap-origin record survives SingleStore's implicit DDL commits.
SELECT 1;
