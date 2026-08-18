ALTER TABLE work_orders
    ADD COLUMN continuation_session_id text NOT NULL DEFAULT ''
        CHECK (char_length(continuation_session_id) <= 512),
    ADD COLUMN continuation_attempt_id text NOT NULL DEFAULT ''
        CHECK (char_length(continuation_attempt_id) <= 128),
    ADD COLUMN continuation_harness text NOT NULL DEFAULT ''
        CHECK (char_length(continuation_harness) <= 128),
    ADD COLUMN continuation_launch_environment text NOT NULL DEFAULT ''
        CHECK (char_length(continuation_launch_environment) <= 512);
