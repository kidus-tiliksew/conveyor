-- Operator-run only AFTER exact copy validation and before ANY resumed writes.
-- Replace `your_database` with the actual database name in the SQL editor.
-- Keep the database and table qualified: a bare ON name means a database.
-- Explicit copied IDs do not advance every aggregator-local allocator.
AGGREGATOR SYNC AUTO_INCREMENT ON `your_database`.`events_columnstore` ALL;
