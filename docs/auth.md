# Authentication

Conveyor has one identity model and several credential planes. A person has
one account; how they prove it depends on where they are: a session cookie in
the browser, a personal access token in the CLI and MCP clients, a worker
credential on an enrolled machine. Agents never hold credentials of their own
beyond what the launcher hands them.

## The two human credential methods

A browser signs in with a session; everything else presents a bearer token.
When both are present, the bearer token wins.

A session starts from redeeming a sign-in link or from
email-and-password sign-in. The cookie (`conveyor_session`) is HttpOnly,
SameSite Strict, lives 7 days, and is refreshed on every request. Mutating
requests from a session must also carry the `X-Conveyor-CSRF: 1` header and an
`Origin` matching the server's public URL; the dashboard does this for you.
Bearer callers are exempt from the CSRF proof.

A personal access token has the shape `cv_pat_<id>_<secret>`. It is minted on
the Settings page (or `POST /v1/tokens`), shown once, and stored server-side
only as a hash. `conveyor auth login` verifies and stores it locally;
`conveyor auth logout --revoke` removes it on both ends.

Deliberate asymmetries between the two methods:

- Changing your own password or profile requires a session, not a token, so a
  leaked PAT cannot take over the account.
- Driving task execution (`conveyor run`'s claim plane) requires a bearer
  token, not a session, so a browser cannot be tricked into claiming work.
  The one gate action carved back out for the browser is request-changes,
  which stays session-accessible behind the CSRF proof.

## Accounts, sign-in links, onboarding

There is no self-registration. An account exists because an operator invited
the email, provisioned it directly, or because it is the first operator
created at bootstrap.

Sign-in links have the shape `<public-url>/sign-in#token=...` (the token
rides in the URL fragment, so it never reaches server logs). They live 30
minutes, are single use, and issuing a new one invalidates prior unredeemed
links for that email. Delivery is by SMTP when configured; otherwise the link
is surfaced in the dashboard, or printed on the host by
`conveyor user issue-link <email>`.

First sign-in routes to onboarding: set a display name and a password of at
least 12 characters. Password sign-in is rate limited to 5 attempts per 15
minutes per account and per source address, and unknown, deactivated, and
passwordless accounts all fail with the same message and timing.

## Bootstrap

`conveyord` requires `CONVEYOR_API_TOKEN` at startup. On a fresh database it
creates the organization and first operator (identity from
`CONVEYOR_ORGANIZATION_NAME`, `CONVEYOR_FIRST_OPERATOR_EMAIL`,
`CONVEYOR_FIRST_OPERATOR_DISPLAY_NAME`) and binds that env token as the first
operator's personal access token. `conveyor init` drives the same bootstrap
interactively and prints the first sign-in link.

## Roles and capabilities

Authorization is per workspace. Every check names a capability, and roles are
just bundles of capabilities, each strictly containing the previous:

| Role | Capabilities added |
|---|---|
| `viewer` | `view_workspace` |
| `executor` | `claim_work`, `request_changes` |
| `contributor` | `propose_documents` |
| `maintainer` | `set_assignee`, `operate_gates`, `recover_work` |
| `operator` | `confirm_documents`, `manage_membership`, `manage_workspace` |

Two boundaries are worth internalizing. Confirming documents is operator-only,
and it is deliberately not bundled with `operate_gates`: a maintainer can
approve plans and merges all day without ever being able to change what the
factory considers confirmed intent. And a workspace can never lose its last
operator; the server refuses the demotion.

Requests against a workspace you are not a member of return 404, not 403, so
membership existence is not disclosed.

Demoting a user below `claim_work` clears their task assignments and revokes
their enrolled workers in the same operation.

## GitHub (forge) tokens

Executing tasks requires a stored GitHub token, because Conveyor opens pull
requests and merges as the person doing the work, not as a shared bot
account. Save a fine-grained token with Contents read and write and Pull
requests read and write under Settings. The server validates it by reading
the authenticated login, encrypts it with AES-256 under
`CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY`, and never returns the value again;
status reads report only `{configured, forge_login, stored_at}`.

Claiming work is refused without one (the error code is
`forge_token_required`), and both `conveyor run` and the worker check for it
before doing anything else. Every forge write records which identity class
performed it: the executing user, the approving operator, or the host.

## Worker credentials

Workers are a separate credential class. An operator issues a short-lived
pairing token (`conveyor worker pair`); the worker exchanges it once at
enrollment for a durable opaque credential, saved owner-only on the worker
machine. The pairing records who issued it, and that person's GitHub identity
is the one the worker's task executions use.

Worker credentials are valid on the worker plane
(heartbeat, claim, release) and the MCP plane only, never for workspace REST
reads. That is why dispatched agents receive their task assignment through
environment variables rather than fetching the task themselves, and why the
human-reserved MCP tools (`create_task`, `set_assignee`,
`redispatch_work_order`, `report_continuation`) refuse worker credentials.

Revoke a worker with `conveyor worker revoke <worker-id>`; revoked
credentials terminate the worker with an actionable error instead of
retrying.

## What an agent session gets

The launcher (worker or `conveyor run`) starts the agent with an isolated
environment: the credential to reach Conveyor's MCP endpoint
(`CONVEYOR_API_TOKEN`), the server address and workspace, the work order and
session identity, and the task's branch and repository assignment. MCP
registration files reference the token through the environment, never by
value, and the launcher redacts the credential from the child's output
streams.

Everything the agent can do with that credential is scoped: claim-bound
mutation tools work only against its own live claim, artifact reads are
bounded to what the work order's lineage selection actually served, and
document content injected into prompts is labeled untrusted data.

## Workspace context

Workspace scope is explicit everywhere. REST accepts the workspace as a path
segment, a `workspace_id` query parameter, or the `X-Workspace-ID` header,
and rejects disagreements between them. The CLI resolves it from `--workspace`,
`CONVEYOR_WORKSPACE`, then the stored per-server default. Omitting the
workspace entirely is compatible only when you belong to exactly one; with
zero the server says to create one first, with several it demands explicit
context.
