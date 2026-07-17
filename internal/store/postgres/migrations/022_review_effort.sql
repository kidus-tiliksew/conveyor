-- Preserve the configured review-seat effort through asynchronous review
-- publication so operator-facing audit labels survive River retries/restarts
-- (spec §21.19 change 4).
ALTER TABLE review_publications
    ADD COLUMN required_effort text NOT NULL DEFAULT ''
        CHECK (required_effort IN ('', 'low', 'medium', 'high'));
