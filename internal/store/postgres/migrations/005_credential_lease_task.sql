ALTER TABLE credentials
    ADD COLUMN lease_task_id text NOT NULL DEFAULT '';

CREATE INDEX credentials_lease_task_idx
    ON credentials (lease_task_id)
    WHERE lease_task_id <> '';

UPDATE credentials c
SET lease_task_id = j.task_id
FROM jobs j
WHERE c.leased_by = j.id
  AND c.leased_by <> '';
