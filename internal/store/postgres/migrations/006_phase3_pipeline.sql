CREATE TABLE task_specs (
    task_id          text NOT NULL REFERENCES tasks(id),
    version          integer NOT NULL,
    content          text NOT NULL,
    acceptance_count integer NOT NULL,
    acceptance       jsonb NOT NULL DEFAULT '[]'::jsonb,
    decomposition    jsonb NOT NULL DEFAULT '[]'::jsonb,
    approved         boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    approved_at      timestamptz,
    PRIMARY KEY (task_id, version)
);

CREATE INDEX task_specs_latest_idx ON task_specs (task_id, version DESC);
