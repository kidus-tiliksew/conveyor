-- In-process jobs no longer estimate USD cost. A nullable amount preserves an
-- explicit worker-reported value without presenting missing telemetry as zero
-- (spec §21.28).
ALTER TABLE jobs
    ALTER COLUMN cost_usd DROP DEFAULT,
    ALTER COLUMN cost_usd DROP NOT NULL;
