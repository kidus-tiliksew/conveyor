-- Operator-run only AFTER exact copy validation and before ANY resumed writes.
-- Render __DATABASE__ with scripts/render-events-allocator.py before execution.
-- A bare identifier after ON means DATABASE, not the selected database's table.
-- Explicit copied IDs do not advance every aggregator-local allocator.
AGGREGATOR SYNC AUTO_INCREMENT ON __DATABASE__.`events_columnstore` ALL;
