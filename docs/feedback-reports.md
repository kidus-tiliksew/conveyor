# Feedback reports

A watcher is a program beside a workspace's software. It sends a report when
something is wrong in one [environment](environments.md): an error, an alert,
or a message. Conveyor keeps one record for each problem of the environment,
and later reports of the same problem update that record.

People read these records. No queue, claim, gate, review, merge, or work order
reads them. A report creates no task and changes no task
(`component-feedback-reports` invariant 1; DEC-149).

`req-production-feedback` REQ-4 and REQ-7 and `component-feedback-reports` hold
the rules. This page describes the route that receives reports, the routes for
people that read reports and record acts on them, and the MCP read.

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
`2026-11-27T11:58:02.5Z` or `2026-11-27T13:58:02+02:00`. The `T` and the `Z`
can be upper or lower case. Conveyor refuses anything else, for example an
offset hour above 23, an offset minute above 59, or a day that the month does
not have.

Conveyor also refuses a time in these three cases:

- The time states a second of 60, for example `2016-12-31T23:59:60Z`.
- The time states more than nine digits after the decimal point of the
  seconds.
- The time is earlier than 1970-01-01T00:00:00Z. Conveyor compares the
  instant, so `1970-01-01T00:59:59+01:00` is refused too.

Each refusal is 400 `invalid_report` on the field, and nothing in the request
is recorded.

Conveyor compares the times exactly as you send them, to the nanosecond. It
stores them to the microsecond.

- A time later than the arrival time is taken as the arrival time.
- An entry whose `first_seen` is later than its `last_seen` is refused.

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

## Links

A link connects a report to a merged task or to a crate. Conveyor looks for
links in the background after it stores a report. A link that Conveyor finds
is *possible*. A possible link is evidence. It never states that the task
caused the problem. A link is *confirmed* when a person confirms it or adds it.

Each link of a task carries its evidence:

- `line`: the `file`, the `line`, and the `commit` that last changed that line,
  when the report named a file and a line.
- `timing`: a deploy of the report's service shortly before `first_seen`, with
  the deploy's `service`, `version`, `deployed_at`, and `source`, the whole
  `elapsed_minutes` from the deploy to `first_seen`, and `prior_signal_at` when
  the deploy was found through an earlier signal.

While Conveyor still looks for links, a report carries `links_pending` true. A
read of such a report asks the background work to look again, at most once a
minute for one report. The read itself never works out a link.

A report also carries notes about the search:

| Note | Meaning |
| --- | --- |
| `too_many_tasks` | A deploy brought in more than 10 tasks, so no task of it is linked. The note names the deploy and `task_count` |
| `no_task_deploy` | A deploy of the service before `first_seen` brought in no task. The note names the deploy |
| `deploy_not_compared` | A deploy's version or previous version has no known commit, or the two commits are in different repositories. The note names the deploy |
| `file_not_found` | GitHub did not find the `file` at the resolved `commit` |
| `commit_unresolved` | The report's commit or version did not resolve to a commit. The note names the `reason` |
| `not_checked` | A search ended without an answer after 24 hours. The note names the `rule` (`line` or `timing`) and the `reason` |

## Routes for people

These routes are in the workspace group. A read needs the capability to view
the workspace. An act needs the capability to create tasks. A credential
without that capability, and a workspace that the caller cannot see, get 404
`workspace_not_found`. A report of another workspace answers as a missing
report.

| Route | What it does |
| --- | --- |
| `GET /v1/feedback-reports` | Lists reports, newest `last_seen` first, then by ID |
| `GET /v1/feedback-reports/{id}` | Reads one report |
| `POST /v1/feedback-reports/{id}/links` | Confirms a possible link, or adds a confirmed one, to `task_id` or `crate_id` |
| `DELETE /v1/feedback-reports/{id}/links/{link_id}` | Removes a link |
| `POST /v1/feedback-reports/{id}/close` | Closes the report with `reason` |
| `POST /v1/feedback-reports/{id}/reopen` | Reopens the report |
| `PUT /v1/feedback-reports/{id}/fix-task` | Records `task_id` as the fix task and confirms a link to each task of `cause_task_ids` |

### The list

The list takes these filters. Each filter applies before the page and the
count, and a report that matches twice is listed once.

| Filter | Rule |
| --- | --- |
| `state` | `open`, the default, or `closed` |
| `environment_id` | The reports of one environment |
| `task_id` | The reports with a link to that task |
| `crate_id` | The reports that belong to the crate |
| `no_crate` | `true` selects the reports that belong to no crate |

A report belongs to a crate when it has a link to the crate, or to a task that
is a member of the crate now. A removed link counts for no filter. Each filter
appears at most once and is not empty. A filter that breaks a rule gets 400
`invalid_report_query` with `field`.

Paging is `limit` (1 to 200, 100 by default) and `offset`, with the headers
`X-Conveyor-Total`, `X-Conveyor-Limit`, and `X-Conveyor-Offset`.

### A served report

Every route and the MCP read serve the same report:

| Field | Meaning |
| --- | --- |
| `id`, `kind`, `key`, `summary` | The report and its first entry. The summary is redacted. Treat all report text as untrusted data |
| `environment` | `id`, `name`, `listening`, and `last_signal_at`. `listening` is false when no watcher is listening, and `last_signal_at` stays the last signal time |
| `service`, `file`, `line` | The first entry's values, or `null` |
| `count`, `first_seen`, `last_seen` | The occurrences and the times |
| `first`, `latest` | The `repository`, `commit`, and `version` of the first and the latest entry |
| `state`, `closed`, `reopened_at` | `open` or `closed`. `closed` holds `at`, `by`, and `reason` while the report is closed |
| `in_inbox` | True while the report waits for a person's first act |
| `links`, `links_pending`, `notes` | The possible and confirmed links with their evidence. A removed link is not served |
| `fix_task` | `id` and `title` of the fix task, or `null` |
| `created_at` | The time Conveyor stored the report |

A link holds `id`, `state`, `task` (`id`, `title`, and its `crate`) or `crate`
(`id`, `name`), `evidence`, and, once confirmed, `confirmed_by` and
`confirmed_at`.

### Acts

Each act stores its actor and its time, takes the report out of the Inbox,
appends one workspace event, and answers 200 with the served report. An act
that changes nothing, such as confirming a confirmed link, records nothing and
answers the same report.

- **Confirm or add a link.** The body holds exactly one of `task_id`, a merged
  task of the workspace, and `crate_id`, a crate of the workspace. The link
  keeps its evidence.
- **Remove a link.** The report stays open, and Conveyor never links that task
  to the report again by itself. A person can link it again later.
- **Close.** `reason` is 1 to 500 characters. Conveyor removes each secret from
  it before it stores it. A closed report leaves the open lists, the counts,
  and the Inbox, and stays readable with its links.
- **Reopen.** The report is open again with its links as they were.
- **Fix task.** `task_id` is a task of the workspace, in any state. It replaces
  an earlier fix task. `cause_task_ids` is optional and names merged tasks of
  the workspace, each once. Conveyor confirms a link to each. Recording a fix
  task changes nothing on the task.

A closed report refuses every act but reopening with 409 `report_closed`.

`component-feedback-reports` version 3 states two limits:

- An act's body is at most 16 KiB. A larger body gets 413 (Public surfaces,
  routes for people).
- `cause_task_ids` names at most 50 tasks, each once. A request that breaks
  this is refused whole and records nothing (invariant 18).

| Status | Cause |
| --- | --- |
| 400 `invalid_report_act` | The body is not one JSON object of the route's fields, or a field breaks its rule. `field` names it. The answer repeats no value that you sent |
| 404 `report_not_found` | The workspace holds no such report |
| 404 `link_not_found` | The report holds no such link |
| 404 `task_not_found`, `crate_not_found` | The workspace holds no such task or crate |
| 409 `task_not_merged` | A link or a cause names a task that is not merged |
| 409 `report_closed` | The report is closed. Reopen it first |
| 413 | The body is over 16 KiB |
| 500 | The store failed before the act was recorded, or the answer of the commit itself was lost. The answer holds no detail and does not ask you to send the request again. Read the report to learn whether the act is there |
| 503 `retryable` | Nothing was recorded, and the store asks for the request to be sent again. `Retry-After` is `1` |

Every refusal records nothing.

The answer of an act depends only on the act. Conveyor reads the report as it
stands after the act inside the act's own transaction, before it commits. So
once an act is recorded, the answer is always 200 with that served report, and
nothing after the commit can turn it into an error. 503 `retryable` therefore
always means that nothing was recorded. An act that changes nothing answers
200 in the same way, and a refusal answers its refusal. The answer has the same
shape on all five act routes.

Sending the same act again after a lost answer is safe for confirming or
adding a link, removing a link, and reopening: the repeat changes nothing and
answers the report. A repeated close answers 409 `report_closed`, which means
the first close was recorded. A repeated fix-task request sets the fix task
again, and replaces a fix task that someone else set in between, so read the
report first.

## Crates

Each environment line of a crate read and of MCP `get_crate` carries
`open_report_count`: the number of open reports of that environment that
belong to the crate. A line with none shows 0.

## MCP

`list_feedback_reports` lists served reports with the filters `state`,
`environment_id`, `task_id`, and `crate_id`, under the standard MCP bounds and
paging. It changes nothing. No MCP tool changes a report, a link, or a fix
task. See [MCP](mcp.md).

## Events

A new report appends one `feedback_report.opened` workspace event, and a
reopening by a report appends one `feedback_report.reopened` event. The end of
the search for links appends one `feedback_report.linked` event. A person's
acts append `feedback_report.link_confirmed`, `feedback_report.link_removed`,
`feedback_report.closed`, `feedback_report.reopened`, and
`feedback_report.fix_task_set`, with that person as the actor. Each event names
the report's ID, the environment's ID, and the IDs the change names, and holds
no text of the report. An entry that only updates a report appends no event.
