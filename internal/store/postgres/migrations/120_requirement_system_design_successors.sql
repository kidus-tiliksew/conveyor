-- Migration 120 establishes superseded_by as the sole successor source while
-- retaining values from either predecessor column shape.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'requirements'
          AND column_name = 'superseding_document_ids'
    ) THEN
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = current_schema()
              AND table_name = 'requirements'
              AND column_name = 'superseded_by'
        ) THEN
            ALTER TABLE requirements RENAME COLUMN superseding_document_ids TO superseded_by;
        ELSE
            UPDATE requirements target
            SET superseded_by = (
                SELECT COALESCE(array_agg(item ORDER BY first_seen), '{}'::text[]) AS values
                FROM (
                    SELECT item, min(ordinality) AS first_seen
                    FROM unnest(
                        COALESCE(target.superseding_document_ids, '{}'::text[]) ||
                        COALESCE(target.superseded_by, '{}'::text[])
                    ) WITH ORDINALITY AS value(item, ordinality)
                    GROUP BY item
                ) ordered
            );
            ALTER TABLE requirements DROP COLUMN superseding_document_ids;
        END IF;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'system_designs'
          AND column_name = 'superseding_document_ids'
    ) THEN
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = current_schema()
              AND table_name = 'system_designs'
              AND column_name = 'superseded_by'
        ) THEN
            ALTER TABLE system_designs RENAME COLUMN superseding_document_ids TO superseded_by;
        ELSE
            UPDATE system_designs target
            SET superseded_by = (
                SELECT COALESCE(array_agg(item ORDER BY first_seen), '{}'::text[]) AS values
                FROM (
                    SELECT item, min(ordinality) AS first_seen
                    FROM unnest(
                        COALESCE(target.superseding_document_ids, '{}'::text[]) ||
                        COALESCE(target.superseded_by, '{}'::text[])
                    ) WITH ORDINALITY AS value(item, ordinality)
                    GROUP BY item
                ) ordered
            );
            ALTER TABLE system_designs DROP COLUMN superseding_document_ids;
        END IF;
    END IF;
END $$;

ALTER TABLE requirements
    ALTER COLUMN superseded_by SET DEFAULT '{}',
    ALTER COLUMN superseded_by SET NOT NULL;

ALTER TABLE system_designs
    ALTER COLUMN superseded_by SET DEFAULT '{}',
    ALTER COLUMN superseded_by SET NOT NULL;
