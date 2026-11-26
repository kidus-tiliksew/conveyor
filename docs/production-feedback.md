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
| 429 | The signal arrived less than 5 seconds before or after another accepted signal of the key, or one that is on its way to the store. `Retry-After` holds the whole seconds to wait from the answer, at least 1 |
| 500 | The store failed, or the check of the key took more than 10 seconds. The answer holds no detail |
| 503 | The server holds the most keys it can, and each one is inside its 5 seconds or has a request in progress that arrived inside them. Try again |

Each rule of the body is checked before the 5 seconds. An invalid signal gets 400
inside them, and it never delays a valid signal of the same key.

A refused or failed signal does not start the 5 seconds and does not change the
key's `last_used_at`.

The 5-second rule is kept in the memory of each `conveyord` process for at most
4,096 keys. A restart forgets it, and two processes each apply their own. Only
arrival times decide, whatever order the requests finish in: two accepted
signals of a key arrived at least 5 seconds apart, and a signal that arrived at
least 5 seconds from each accepted one is not refused by this rule. While one
signal of a key is on its way to the store, another one that arrived at least 5
seconds from it waits for it.

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

The records are those of the arrival order, whatever order signals reach the
store in. Signals with the same arrival time keep the order in which the server
accepted them, and each one stays a report of its own. When a signal reaches the store after a later arrival that named the
same service:

- It records its deploys and its stops at its own arrival.
- The next arrival that names the service ends what it reported. A version that
  it alone names is stopped then. A version that it dropped and that the next
  arrival names is deployed again then.
- The versions that run now and the newest signal stay. The deploy time of a
  version that runs now can change, for example when the late signal shows that
  the version was deployed earlier.

This holds however late the signal is. An arrival that changed nothing ends an
earlier one too, so the server keeps each accepted signal: the arrival, the
key's label, and which services it named. These rows are never removed while the
environment exists.

A version is stored as it was sent. It stays unresolved until the background
work in [Resolving a version](#resolving-a-version) resolves it.

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

Each row also holds `newest_unresolved`: the running version deployed last
that has no resolved commit, or `null` when every running version is resolved.

| Field | Meaning |
| --- | --- |
| `service` | The service that runs the version |
| `repository`, `commit`, `version` | The version as the signal stated it. An empty member is left out |
| `deployed_at` | When the version was deployed |
| `reason` | Why it is unresolved. See the reasons below |

## Resolving a version

After the route answers, a background step resolves each version that the
signal names and that has no stored answer. The route never waits for it, and
no forge call runs before the answer is sent.

1. A stated `commit` is read first. With a `repository`, only that repository
   is read. Without one, the commit must be in exactly one configured
   repository.
2. Without a commit, the environment's version rule is applied to `version`.
   A commit pattern reads a commit of 7 to 40 hexadecimal characters: each `*`
   matches any text and every other character is literal. A tag template
   replaces `{version}` and `{service}` once and reads that tag.
3. The read is `GET repos/{repository}/commits/{ref}` with the workspace
   GitHub App's installation token. conveyord runs no git command and holds no
   clone.

A resolved version holds its repository and its full commit ID, and it is never
resolved again. An unresolved version keeps one reason:

| Reason | Meaning |
| --- | --- |
| `pending` | No attempt has finished yet |
| `no_github_app` | The workspace has no connected GitHub App |
| `no_repository` | No configured repository has a GitHub slug, or the named one has none |
| `no_version_rule` | The version states no commit, and the environment has no version rule |
| `version_rule_mismatch` | The version does not fit the version rule |
| `no_match` | No candidate repository holds the commit or the tag |
| `ambiguous` | Two or more repositories hold it, even with one commit ID |
| `forge_request`, `forge_status`, `forge_response`, `forge_rate_limited`, `forge_permission` | A GitHub read failed with that category. A failure in one candidate repository proves neither absence nor uniqueness. A 422 proves absence only when GitHub's message says no commit was found. A 422 with a retry header is `forge_rate_limited` and keeps GitHub's retry time |

Each background job has a two-minute bound. A GitHub read that ends when
that time runs out still saves its outcome, with its own short bound, so a
claimed version never stays `pending`. No new request starts after the
job's time is over.

An unresolved version is tried again only when a later accepted signal names
it, at least 10 minutes after the last attempt. When GitHub names a later time
for the next request, the version waits until then. Reads and timers never try
again.

The installation token is requested only after a version's attempt is
claimed. A failed token request is that attempt's outcome: the version keeps
the failure's category as its reason, and a rate-limited token request keeps
GitHub's retry time. After a failed token request, no background work of the
workspace asks for a token again until that retry time, or for one minute
when GitHub named none.

## Live status

A merged task's merge commit is the `merge_commit_sha` of its latest
`merge.confirmed` or `merge.reconciled` event. A resolved version contains a
task when the version's repository is the task's repository and the version's
commit equals the merge commit or descends from it. An unresolved version
contains no task.

The relevant environments of a merged task are the unarchived environments
that hold a deploy for its repository or track a branch for it.

| Status | Meaning |
| --- | --- |
| `live` | At least one service runs a version that contains the task. When every running version has its answer, a missing answer for a stopped version does not hold this status back |
| `no_longer_live` | A past deploy contained the task, and no running version does |
| `not_live_yet` | No deploy contained the task |
| `unknown` | GitHub reports the merge commit missing from the repository |

### Comparing commits

The server stores GitHub's answer for each pair of commits it compares and
never asks for that pair again. Every comparison runs in the background:

- When a version resolves and descends from the service's previous version,
  one comparison of the previous commit with the new one lists up to 100
  commits that arrived. A merged task whose merge commit arrived, equals the
  previous commit, or was in the previous version is contained. A list that is
  not complete proves only the commits it lists.
- A service's first version, a version that does not descend from the
  previous one, and every pair still unproven are compared directly with each
  merged task of the repository, newest merge first.
- A workspace starts at most 60 comparisons and branch reads in any minute,
  across every environment, read, worker, and server. Each request is counted
  when it starts, however long it takes. A pair already in flight is not
  asked twice. Work past the allowance waits for a later signal or read.
- Pairs never tried go before pairs tried earlier, and those go oldest
  attempt first. A pair that keeps failing therefore never holds back the
  pairs after it.
- For `not_live_yet`, the head of the environment's tracked branch is read in
  the background and compared by its full commit ID. A read asks for a new
  head when the stored one is 10 minutes old or older.
- When GitHub confirms that the tracked branch does not exist, that answer
  replaces the stored head. The status stays `not_live_yet` with
  `on_tracked_branch` set to `false`, and a read still asks for a new head
  every 10 minutes. A head stored before the branch was deleted is never used
  again. A GitHub failure that proves nothing, such as a timeout, keeps the
  stored head.

### The fields

A read asks GitHub nothing. When a stored answer that a status needs is
missing, the read asks the background work for it and leaves out only that
environment's status. A later read serves it. A placeholder status is never
shown.

Each list below is always present in its scope, as a JSON array. In a
workspace with no unarchived environment, or when no environment is relevant,
the list is `[]`, never `null` and never a missing key:

| List | Where it appears | Not present on |
| --- | --- | --- |
| `environments` (statuses) | `GET /v1/tasks/{id}` and MCP `get_task` for a merged task | A task that is not merged |
| `environments` (lines) | `GET /v1/crates/{id}` and MCP `get_crate` | — |
| `live_in` | Each merged member row of `GET /v1/crates/{id}`, and each merged member's value in the MCP `get_crate` `live_in` object | A crate member that is not merged |

No list endpoint carries these fields, and a task read carries no `live_in`.

`GET /v1/tasks/{id}` and MCP `get_task` add `environments` to a merged task:

| Field | Meaning |
| --- | --- |
| `environment` | `id` and `name` |
| `status` | `live`, `not_live_yet`, `no_longer_live`, or `unknown` |
| `services` | For `live`, the services that run a containing version. Otherwise empty |
| `live_since` | For `live`, the earliest deploy time of a containing version that runs now. Otherwise `null` |
| `on_tracked_branch` | For `not_live_yet`, `true` when the tracked branch holds the merge commit |
| `as_of` | The newest signal or GitHub deployment report of the environment, or `null` |

`GET /v1/crates/{id}` and MCP `get_crate` add `environments`, one line for each
environment relevant to at least one merged member:

| Field | Meaning |
| --- | --- |
| `environment` | `id` and `name` |
| `live_task_count` | Merged members that are `live` there |
| `merged_task_count` | Every merged member of the crate |
| `newest_live_at` | The latest `live_since` among them, or `null` |
| `listening`, `last_signal_at` | As on `GET /v1/environments` |
| `as_of` | As on the task read |

Each merged member row of `GET /v1/crates/{id}` adds `live_in`, the names of
the environments where it is live, or `[]`. MCP `get_crate` adds `live_in` as
an object from task ID to those names. It has one entry for each merged member
that `task_ids` lists, with `[]` for a member live nowhere, and no entry for a
member that is not merged or that `task_ids` leaves out. The crate lines count
every merged member, also when `task_ids` is truncated.

Live status is a record for people to read. No gate, queue, claim, merge,
review, next action, or work order reads it, and it changes no task.
