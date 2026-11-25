# Production feedback

A watcher is a program beside a workspace's software. It sends a signal that
tells Conveyor which version each service runs in one
[environment](environments.md). Conveyor records each new version as a deploy.

People read these records. No queue, claim, gate, review, merge, or work order
reads them. A signal creates no task, changes no task, and raises no notice
(`component-production-feedback` invariant 1; DEC-149).

`req-production-feedback` and `component-production-feedback` hold the rules.

## The signal route

`POST /v1/feedback/signals` is outside the workspace group. Its only credential
is an [environment key](environments.md#keys) in one `Authorization: Bearer`
header. A personal access token, an OAuth token, and a session cookie are
refused. The key decides the workspace and the environment. No header, query, or
body field can choose them.

```sh
curl -X POST https://conveyor.example.com/v1/feedback/signals \
  -H "Authorization: Bearer $ENVIRONMENT_KEY" \
  -H "Content-Type: application/json" \
  --data '{
    "next_signal_seconds": 60,
    "services": [
      {
        "name": "api",
        "repository": "app",
        "versions": [
          {"commit": "3f9c2ab", "version": "2026.11.25"}
        ]
      },
      {
        "name": "worker",
        "versions": [{"version": "1.4.0"}, {"version": "1.5.0"}]
      }
    ]
  }'
```

The answer is 202 with the server's time of arrival and nothing else:

```json
{"accepted_at": "2026-11-25T12:00:00.123456Z"}
```

Everything the signal causes is dated at that time. No time in the body is read.

### Body

The body is one JSON object of at most 256 KiB.

| Field | Rule |
| --- | --- |
| `next_signal_seconds` | Required. A whole number from 10 to 86,400: the seconds until the watcher expects to send again |
| `services` | Required. 1 to 200 entries |
| `services[].name` | Required. 1 to 100 characters. Differs from each other name of the signal |
| `services[].repository` | Optional. The name of a repository in the workspace configuration |
| `services[].versions` | Required. 1 to 10 entries. Each states `commit`, `version`, or both |
| `services[].versions[].commit` | 7 to 40 hexadecimal characters |
| `services[].versions[].version` | 1 to 200 characters |

Leave out a field that has no value. An explicit `null` or an empty string is
refused, and so is an unknown field or a field that appears twice.

Names and versions are kept and compared exactly as sent, so case and spaces
count. Two entries are the same version only when the repository, the commit,
and the version are all equal. An entry that appears twice counts once.

`name` and `version` refuse the NUL character and text that looks like a
credential. Such text is never stored, and no refusal repeats a submitted value.

A signal that breaks a rule is refused whole and records nothing.

### Refusals

| Status | Cause |
| --- | --- |
| 400 `invalid_signal` | `field` names the field by its path, such as `services[0].versions[1].commit`, and `message` states the rule. `body` names the whole body |
| 401 | The credential is missing, is not an environment key, is revoked, or belongs to an archived environment. The answer does not say which |
| 413 | The body is over 256 KiB |
| 429 | The key sent an accepted signal less than 5 seconds earlier, or another signal of the key is in progress. `Retry-After` holds whole seconds |
| 500 | The store failed. The answer holds no detail |
| 503 | The server holds the most keys it can, and each one is inside its 5 seconds. Try again |

A refused or failed signal does not start the 5 seconds and does not change the
key's `last_used_at`.

The 5-second rule is kept in the memory of each `conveyord` process for at most
4,096 keys. A restart forgets it, and two processes each apply their own.

## Deploys and running versions

For each service that a signal names:

- A version that the service did not run before records a deploy.
- A version that the service ran before and that the signal no longer names is
  recorded as stopped.
- A version that the signal names again keeps its deploy and its deploy time.
- A version that comes back after it stopped records a new deploy.

A service that the signal does not name keeps its running versions. The first
signal that names a service records its versions as deploys.

Each deploy and each stop holds the arrival time, the source `signal` with the
key's label, and the system as the actor. Everything one signal causes is
recorded in one transaction.

When a signal reaches the store after a later arrival, it changes no service
that the later arrival named. It also does not replace the newest signal.

A version is stored as it was sent and is unresolved. No forge is asked.

## Listening

An environment is listening from an accepted signal until twice that signal's
`next_signal_seconds` has passed with no later one. Each row of
`GET /v1/environments` holds the state, computed at the read:

| Field | Rule |
| --- | --- |
| `listening` | `true` or `false` |
| `last_signal_at` | The arrival of the newest accepted signal, or `null` before the first |

`last_signal_at` stays when the environment is no longer listening. No timer
runs.
