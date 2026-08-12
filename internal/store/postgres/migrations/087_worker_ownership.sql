ALTER TABLE worker_pairings
    ADD COLUMN owner_user_id text REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE workers
    ADD COLUMN owner_user_id text REFERENCES users(id) ON DELETE SET NULL;
