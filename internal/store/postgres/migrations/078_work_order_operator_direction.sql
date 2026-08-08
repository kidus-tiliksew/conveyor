ALTER TABLE work_orders
    ADD COLUMN operator_direction text NOT NULL DEFAULT ''
    CHECK (char_length(operator_direction) <= 4096);
