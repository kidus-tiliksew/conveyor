-- DEC-23 / req-execution-configuration v4 REQ-8: remove live server execution
-- registry/setup/template state while preserving workspace pipeline policy.
WITH captured AS (
    SELECT id,
           config_yaml,
           coalesce((regexp_match(config_yaml, '(?m)^stage_timeouts:\n(?:    [^\n]*\n)*    spec: ([^\n]+)'))[1], (regexp_match(config_yaml, '(?m)^    spec:\n(?:        [^\n]*\n)*        timeout: ([^\n]+)'))[1], '30m') AS spec_timeout,
           coalesce((regexp_match(config_yaml, '(?m)^stage_timeouts:\n(?:    [^\n]*\n)*    implement: ([^\n]+)'))[1], (regexp_match(config_yaml, '(?m)^    implementation:\n(?:        [^\n]*\n)*        timeout: ([^\n]+)'))[1], '4h') AS implement_timeout,
           coalesce((regexp_match(config_yaml, '(?m)^stage_timeouts:\n(?:    [^\n]*\n)*    review: ([^\n]+)'))[1], (regexp_match(config_yaml, '(?m)^    review:\n(?:        [^\n]*\n)*        timeout: ([^\n]+)'))[1], '1h') AS review_timeout,
           greatest(1, array_length(regexp_split_to_array(coalesce((regexp_match(config_yaml, '(?m)^review:\n((?:[ \t]+[^\n]*(?:\n|$))*)'))[1], ''), '(?m)^        - '), 1) - 1) AS review_seats
    FROM workspaces
), stripped AS (
    SELECT id, spec_timeout, implement_timeout, review_timeout, review_seats,
           regexp_replace(
             regexp_replace(
               regexp_replace(
               regexp_replace(
                 regexp_replace(
                   regexp_replace(
                     regexp_replace(
                     regexp_replace(config_yaml, '(?m)^execution_settings:\n(?:[ \t]+[^\n]*(?:\n|$))*', '', 'g'),
                       '(?m)^routing:\n(?:[ \t]+[^\n]*(?:\n|$))*', '', 'g'),
                     '(?m)^harnesses:\n(?:[ \t]+[^\n]*(?:\n|$))*', '', 'g'),
                   '(?m)^review:\n(?:[ \t]+[^\n]*(?:\n|$))*', '', 'g'),
                 '(?m)^setups:\n(?:[ \t]+[^\n]*(?:\n|$))*', '', 'g'),
               '(?m)^default_setup:.*\n?', '', 'g'),
             '(?m)^planning_models:\n(?:[ \t]+[^\n]*(?:\n|$))*', '', 'g'),
           '(?m)^stage_timeouts:\n(?:[ \t]+[^\n]*(?:\n|$))*', '', 'g') AS policy_yaml
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
