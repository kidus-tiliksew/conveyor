-- Blueprint SUB identity is stable across spec versions (spec §4.1).
DO $$
DECLARE
    duplicate record;
BEGIN
    SELECT workspace_id, parent_task_id, origin_sub_id, count(*) AS child_count
      INTO duplicate
      FROM tasks
     WHERE parent_task_id IS NOT NULL
       AND origin_sub_id <> ''
       AND state NOT IN ('merged', 'closed')
     GROUP BY workspace_id, parent_task_id, origin_sub_id
    HAVING count(*) > 1
     ORDER BY workspace_id, parent_task_id, origin_sub_id
     LIMIT 1;

    IF FOUND THEN
        RAISE EXCEPTION
            'cannot tighten tasks_live_blueprint_origin_idx: duplicate live blueprint children for workspace %, parent %, SUB % (% rows)',
            duplicate.workspace_id, duplicate.parent_task_id,
            duplicate.origin_sub_id, duplicate.child_count
            USING ERRCODE = '23505';
    END IF;
END
$$;

DROP INDEX tasks_live_blueprint_origin_idx;
CREATE UNIQUE INDEX tasks_live_blueprint_origin_idx
    ON tasks (workspace_id, parent_task_id, origin_sub_id)
    WHERE parent_task_id IS NOT NULL
      AND origin_sub_id <> ''
      AND state NOT IN ('merged', 'closed');
