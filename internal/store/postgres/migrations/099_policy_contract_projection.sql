-- DEC-23 / req-execution-configuration v4 AC-7.2: project every live frozen
-- task contract to pipeline policy only. The events ledger is append-only and
-- deliberately untouched; migration application is audited by schema version.
UPDATE tasks AS task
SET setup_name = '',
    setup_contract = jsonb_build_object(
		'max_bounces', coalesce(
			nullif(task.setup_contract->>'max_bounces', '')::integer,
			nullif((regexp_match(workspace.config_yaml, '(?m)^max_bounces: ([^\n]+)'))[1], '')::integer,
			10
		),
        'stage_timeouts', jsonb_build_object(
            'spec', coalesce(setup_contract #>> '{execution_settings,spec,timeout}', setup_contract #>> '{stage_timeouts,spec}', ''),
            'implement', coalesce(setup_contract #>> '{execution_settings,implementation,timeout}', setup_contract #>> '{stage_timeouts,implement}', ''),
            'review', coalesce(setup_contract #>> '{execution_settings,review,timeout}', setup_contract #>> '{stage_timeouts,review}', '')
        ),
        'review', jsonb_build_object(
            'seats', coalesce(
                (SELECT jsonb_agg('{}'::jsonb) FROM jsonb_array_elements(coalesce(setup_contract #> '{review,seats}', '[]'::jsonb))),
                '[]'::jsonb
            )
        ),
        'refresh_review', coalesce(setup_contract->>'refresh_review', 'delta')
    )
FROM workspaces AS workspace
WHERE workspace.id = task.workspace_id;
