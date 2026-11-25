# Environments

An environment is a workspace record for one place the software runs, such as
staging or production. People read it. No queue, claim, gate, review, merge,
or work order reads it (`component-environments` invariant 1). A workspace
with no environment behaves as before.

This page lists the routes, the fields, the key format, and the events.
`req-environments` and `component-environments` hold the rules.

## Routes

All routes are in the workspace group, so they take a personal access token or
a dashboard session and the workspace context that
[Authentication](auth.md#workspace-context) describes. A read needs
`view_workspace`. A mutation needs `manage_workspace`.

| Route | Request | Answer |
| --- | --- | --- |
| `GET /v1/environments` | `archived`: `false` (default) or `true` | 200 with an array of environment rows, ordered by name without regard to case |
| `POST /v1/environments` | `name`, and optionally each other field of the next route | 201 with the environment row |
| `PATCH /v1/environments/{id}` | Any of `name`, `tracked_branches`, `github_environments`, `version_rule`, `timing_window_minutes`, and `archived` | 200 with the environment row |
| `POST /v1/environments/{id}/keys` | `label` | 201 with the key row and `value`, with `Cache-Control: no-store` |
| `DELETE /v1/environments/{id}/keys/{key_id}` | None | 204 |
| `GET /v1/environments/suggestions` | None | 200 with an array of `{github_environment, repository, last_reported_at}`, ordered by name |
| `POST /v1/environments/suggestions/ignore` | `github_environment` | 204 |

The two `GET` routes also take `limit` and `offset`, as every list route does.
`GET /v1/environments/suggestions` returns at most 100 rows when the request
sends no `limit`. A paged answer carries `X-Conveyor-Total`,
`X-Conveyor-Limit`, and `X-Conveyor-Offset`.
A body is one JSON object of at most 256 KiB. A body that is not one JSON object
answers 400 in plain text, and a larger body answers 413. A field that the route
does not take, or a field that appears twice, is refused.

`PATCH` leaves an omitted field unchanged. Only `version_rule` accepts `null`,
which clears it. A request that changes no field records no event.

A second `DELETE` of one key answers 204 and records nothing.

### Refusals

| Status and code | Cause |
| --- | --- |
| 400 `invalid_environment` | `field` names the field, and `message` states the rule |
| 404 `environment_not_found` | The environment, or its key, is missing or belongs to another workspace |
| 409 `environment_archived` | The environment is archived and the request edits a field other than `archived`, or issues a key |
| 409 `environment_key_limit` | The environment holds five keys that are not revoked |

Each refusal is JSON with `error` and `message`. A refused request records
nothing.

## Fields

| Field | Rule |
| --- | --- |
| `id` | `env-<yymmdd>-<6 hex>`, set by the server |
| `name` | 1 to 40 characters after trimming. Differs, without regard to case, from the name of every unarchived environment of the workspace |
| `tracked_branches` | An object from repository name to one branch name. Each repository is in the workspace configuration |
| `github_environments` | At most ten names of 1 to 255 characters. No other unarchived environment lists one of them, without regard to case. A repeated name is stored once |
| `version_rule` | `null`, or `kind` and `text` |
| `timing_window_minutes` | A whole number from 1 to 1,440. The default is 60 |
| `archived` | `true` or `false` |
| `created_by`, `created_at` | Set by the server |
| `keys` | The keys that are not revoked, oldest first |

Text fields refuse the NUL character. They also refuse text that the credential
redactor changes or that holds a credential shape, such as a token or an
environment key value, as given or after one percent-decoding. This rule covers
`name`, the repositories and branches of `tracked_branches`,
`github_environments`, the `version_rule` text, a key `label`, the name in an
ignore request, and the name and repository of a deployment report. The refusal names the field and never
repeats the text, so no record, read, or event holds it
(`req-security-boundaries` REQ-2 and REQ-3).

A version rule text is at most 200 characters:

- `commit_pattern`: the text holds exactly one `{commit}`, and `*` matches any
  run of characters. An example is `v*-{commit}`.
- `tag_template`: the text holds exactly one `{version}` and at most one
  `{service}`. An example is `{service}/v{version}`. The text names a valid Git
  tag when its placeholders are replaced, so a template with a space, or one such
  as `v..{version}`, is refused.

### Archive and restore

`archived: true` archives an environment and keeps its records readable. While
it is archived, each of its keys is refused, and a request that names any field
other than `archived` answers 409. A key can still be revoked.
`archived: false` restores it. A restore is refused when another unarchived
environment took its name or one of its GitHub environment names.

## Keys

An environment key belongs to the environment and not to a person. It carries
no user, role, capability, or session (DEC-148).

| Field | Rule |
| --- | --- |
| `id` | `envkey_<12 random bytes in URL-safe base64>` |
| `label` | 1 to 60 characters after trimming |
| `created_by`, `created_at` | Set by the server |
| `last_used_at` | `null` before the first use. Written at most once a minute |
| `value` | `cv_env_<key id>_<32 random bytes in URL-safe base64>` |

`value` is only in the answer to `POST /v1/environments/{id}/keys`. The server
stores the SHA-256 of the value and cannot return the value again. Logs and
events hold neither, and the credential redactor covers the shape.

A revoked key is refused at its next use. Revocation is final. A revoked key is
in no read.

The server refuses a bearer with the `cv_env_` prefix with 401 on every path
except `/v1/feedback/signals`, before routing and even beside a session cookie
(`req-security-boundaries` AC-1.5). That path is not mounted yet, so a key is
accepted nowhere today.

Other server components verify a presented value with
`EnvironmentStore.VerifyEnvironmentKey`. It returns the workspace ID, the
environment ID, the key ID, and the label. Every value that is not valid gets
the one refusal `ErrInvalidEnvironmentKey`. Any other error is a store failure.

## Suggestions

A suggestion is a GitHub environment name that GitHub reported in a repository
of the workspace, that no unarchived environment lists, and that no workspace
manager ignored. It holds the repository and the time of the newest report. It
creates no environment, and only its route serves it.

An environment that lists the name removes the suggestion and any ignore of the
name. An ignored name stays ignored until then. An archive brings back no
earlier report. A manager can ignore a name that GitHub has not reported yet,
but not a name that an unarchived environment lists.

`EnvironmentStore.RecordEnvironmentReport` records a suggestion. No delivery
calls it yet.

## Events

Each change appends one workspace event with the actor.

| Event | Payload |
| --- | --- |
| `environment.created` | `workspace_id`, `environment_id`, and `changes` with every field from `null` |
| `environment.updated` | The same, with the old and new value of each changed field |
| `environment.archived`, `environment.restored` | The same. A request that also edits fields records one event, with those changes in it |
| `environment.key_issued`, `environment.key_revoked` | `workspace_id`, `environment_id`, `key_id`, and `label` |
| `environment.suggestion_ignored` | `workspace_id`, `github_environment`, and `changes` with `ignored` |

A report that records a suggestion, and a key's last use, append no event.

## MCP

[`list_environments`](mcp.md#operator-investigation-read-only) returns each
environment row without `keys`, archived environments included. No MCP tool
creates or edits an environment or a key.
