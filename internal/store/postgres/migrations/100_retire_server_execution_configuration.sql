-- DEC-23 / req-execution-configuration v4 REQ-8: remove live server execution
-- registry/setup/template state while preserving workspace pipeline policy.
WITH captured AS (
    SELECT id,
           config_yaml,
           coalesce((regexp_match(config_yaml, '(?ms)^    spec:\n.*?^        timeout: ([^\n]+)'))[1], '30m') AS spec_timeout,
           coalesce((regexp_match(config_yaml, '(?ms)^    implementation:\n.*?^        timeout: ([^\n]+)'))[1], '4h') AS implement_timeout,
           coalesce((regexp_match(config_yaml, '(?ms)^    review:\n.*?^        timeout: ([^\n]+)'))[1], '1h') AS review_timeout,
           greatest(1, array_length(regexp_split_to_array(coalesce((regexp_match(config_yaml, '(?ms)^review:\n((?:^[ \t].*\n?)*)'))[1], ''), '(?m)^        - '), 1) - 1) AS review_seats
    FROM workspaces
), stripped AS (
    SELECT id, spec_timeout, implement_timeout, review_timeout, review_seats,
           regexp_replace(
             regexp_replace(
               regexp_replace(
                 regexp_replace(
                   regexp_replace(
                     regexp_replace(
                       regexp_replace(config_yaml, '(?ms)^execution_settings:\n(?:^[ \t].*\n?)*', '', 'g'),
                       '(?ms)^routing:\n(?:^[ \t].*\n?)*', '', 'g'),
                     '(?ms)^harnesses:\n(?:^[ \t].*\n?)*', '', 'g'),
                   '(?ms)^review:\n(?:^[ \t].*\n?)*', '', 'g'),
                 '(?ms)^setups:\n(?:^[ \t].*\n?)*', '', 'g'),
               '(?m)^default_setup:.*\n?', '', 'g'),
             '(?ms)^planning_models:\n(?:^[ \t].*\n?)*', '', 'g') AS policy_yaml
    FROM captured
)
UPDATE workspaces workspace
SET config_yaml = regexp_replace(stripped.policy_yaml, '(?m)^    first_activity_timeout:.*\n?', '', 'g') ||
    E'stage_timeouts:\n    spec: ' || stripped.spec_timeout ||
    E'\n    implement: ' || stripped.implement_timeout ||
    E'\n    review: ' || stripped.review_timeout ||
    E'\nreview:\n    seats:\n' || repeat(E'        - {}\n', stripped.review_seats)
FROM stripped
WHERE workspace.id = stripped.id;

DROP TABLE IF EXISTS harness_model_failures;
