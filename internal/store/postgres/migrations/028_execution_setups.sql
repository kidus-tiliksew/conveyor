ALTER TABLE tasks
    ADD COLUMN setup_name text NOT NULL DEFAULT '',
    ADD COLUMN setup_contract jsonb NOT NULL DEFAULT '{}'::jsonb;
