ALTER TABLE work_orders
    ADD COLUMN checkpoint jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT work_orders_checkpoint_object CHECK (jsonb_typeof(checkpoint) = 'object');
