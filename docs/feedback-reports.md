# Feedback reports

A watcher is a program beside a workspace's software. It sends a report when
something is wrong in one [environment](environments.md): an error, an alert,
or a message. Conveyor keeps one record for each problem of the environment,
and later reports of the same problem update that record.

People read these records. No queue, claim, gate, review, merge, or work order
reads them. A report creates no task and changes no task
(`component-feedback-reports` invariant 1; DEC-149).

`req-production-feedback` REQ-4 and `component-feedback-reports` hold the
rules. This page describes the route that receives reports.

## The report route

`POST /v1/feedback/reports` is outside the workspace group. Its only credential
is an [environment key](environments.md#keys) in one `Authorization: Bearer`
header. A personal access token, an OAuth token, and a session cookie are
refused. The key decides the workspace and the environment. No header, query, or
body field can choose them. The server checks the key before it reads the body.

```sh
curl -X POST https://conveyor.example.com/v1/feedback/reports \
  -H "Authorization: Bearer $ENVIRONMENT_KEY" \
  -H "Content-Type: application/json" \
  --data '{
    "reports": [
      {
        "kind": "error",
        "key": "checkout.timeout",
        "summary": "Checkout request timed out after 30 seconds",
        "service": "api",
        "repository": "app",
        "commit": "3f9c2ab",
        "count": 12,
        "first_seen": "2026-11-27T11:58:02Z",
        "last_seen": "2026-11-27T11:59:40Z",
        "file": "internal/checkout/pay.go",
        "line": 214
      },
      {
        "kind": "alert",
        "key": "queue.depth",
        "summary": "Payment queue depth is above 10,000"
      }
    ]
  }'
```

The answer is 202 with the server's time of arrival and nothing else:

```json
{"accepted_at": "2026-11-27T12:00:00.123456Z"}
```

The answer holds no stored content. The server reads the arrival time before it
checks the key.

### Body

The body is one JSON object of at most 256 KiB with one field, `reports`.

| Field | Rule |
| --- | --- |
| `reports` | Required. 1 to 50 entries. The `key` of each entry differs from the `key` of every other entry |
| `reports[].kind` | Required. `error`, `alert`, or `message` |
| `reports[].key` | Required. 1 to 200 characters. It names the problem in the environment |
| `reports[].summary` | Required. 1 to 1,000 characters |
| `reports[].service` | Optional. 1 to 100 characters |
| `reports[].repository` | Optional. The name of a repository in the workspace configuration, matched exactly |
| `reports[].commit` | Optional. 7 to 40 hexadecimal characters |
| `reports[].version` | Optional. 1 to 200 characters |
| `reports[].count` | Optional. A whole number from 1 to 1,000,000,000. It is 1 when you leave it out |
| `reports[].first_seen` | Optional. An RFC 3339 time. It is the arrival time when you leave it out |
| `reports[].last_seen` | Optional. An RFC 3339 time. It is the arrival time when you leave it out |
| `reports[].file` | Optional. 1 to 500 characters. State it together with `line` |
| `reports[].line` | Optional. A whole number from 1 to 1,000,000. State it together with `file` |

Leave out a field that has no value. An explicit `null` or an empty string is
refused, and so is an unknown field or a field that appears twice.

`count` is the number of occurrences since the watcher's last report of the
problem, not a running total. Conveyor adds each `count` to the stored count.

`file` is a path from the root of the repository, such as
`internal/checkout/pay.go`, with `line` as the line number in that file.

`first_seen` and `last_seen` must be RFC 3339 date-times, such as
`2026-11-27T11:58:02.5Z` or `2026-11-27T13:58:02+02:00`. Conveyor refuses
anything else, including an offset hour above 23, an offset minute above 59,
and a day that the month does not have. A leap second is accepted only where
it ends UTC 23:59.

Conveyor compares the times exactly as you send them, every fractional digit
included. It stores them to the microsecond.

- A time later than the arrival time is taken as the arrival time.
- An entry whose `first_seen` is later than its `last_seen` is refused.
- Conveyor stores any UTC time from year 0000 to year 9999. A time with an
  offset can fall before 0000-01-01T00:00:00Z, for example
  `0000-01-01T00:00:00+01:00`. Conveyor refuses such a time on its field. A
  time after year 9999 is later than the arrival, so it is taken as the
  arrival.

Keys, services, versions, and files are kept and compared exactly as sent, so
case and spaces count. A request is refused whole when a `key`, `service`,
`repository`, `commit`, `version`, or `file` holds a credential or another
secret. The secrets checked include every secret that Conveyor stores. Conveyor
removes each secret from a `summary` before it stores it, and records how many
it removed of each kind.

### One record for each problem

The environment and the `key` identify a problem.

- The first entry under a key stores a new, open report.
- A later entry under the key adds its `count`. It keeps the earlier
  `first_seen` and the later `last_seen`. It replaces the stored repository,
  commit, and version with the ones it states, or with none. It changes nothing
  else: the kind, the summary, the service, the file, and the line stay those of
  the first entry.
- When the report is closed, a later entry whose `last_seen` is later than the
  time of closing opens the report again. Any other entry leaves a closed report
  closed.

Each report and each reopening records the environment and the key's label as
its source and the system as its actor. An accepted request also writes the
key's `last_used_at`, at most once a minute. A report does not change whether a
watcher is listening.

One request is one transaction. A request that breaks a rule is refused whole
and records nothing.

### Open reports

An environment holds at most 200 open reports. A request whose new reports would
take the environment beyond 200 open reports is refused whole with 409
`open_report_limit`. A request that only updates or reopens reports is not
refused by this limit.

### Refusals

| Status | Cause |
| --- | --- |
| 400 `invalid_report` | `field` names the field by its path, such as `reports[0].summary`, and `message` states the rule. `body` names the whole body. The answer repeats no value that you sent |
| 401 | The credential is missing, is not an environment key, is revoked, or belongs to an archived environment. The answer does not say which |
| 409 `open_report_limit` | Accepting the request would store new reports beyond the limit of 200 open reports in the environment. Nothing was recorded |
| 413 | The body is over 256 KiB |
| 429 | The request arrived less than 5 seconds before or after another accepted report request of the key, or one that is on its way to the store. `Retry-After` holds the whole seconds to wait from the answer, from 1 to 5 |
| 500 | The store failed, or the check of the key took more than 10 seconds. The answer holds no detail |
| 503 | The server holds the most keys it can for this route. Try again. Nothing was recorded |
| 503 `retryable` | The store asks for the request to be sent again. The answer is JSON with `error` of `retryable`, and `Retry-After` is `1`. Nothing was recorded and the 5 seconds did not start, so send the same request again after that second |

Each rule of the body is checked before the 5 seconds. An invalid request gets
400 inside them, and it never delays a valid request of the same key. A refused
or failed request does not start the 5 seconds and does not change the key's
`last_used_at`.

The 5-second rule of this route keeps its own state, apart from the
[signal route's](production-feedback.md#refusals). A report request and a signal
of one key do not delay each other. Like the signal rule, it is kept in the
memory of each `conveyord` process for at most 4,096 keys.

## Events

A new report appends one `feedback_report.opened` workspace event, and a
reopening appends one `feedback_report.reopened` event. Each names the report's
ID and the environment's ID and holds no text of the report. An entry that only
updates a report appends no event.
