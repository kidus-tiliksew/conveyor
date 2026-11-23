# Authentication

Conveyor has one identity model and several credential planes. A person has
one account; how they prove it depends on where they are: a session cookie in
the browser, a personal access token in the CLI and in the MCP
registrations of the agent sessions the person starts, and an OAuth access
token that an MCP client obtains for `/mcp` only (see
[MCP OAuth 2.1](#mcp-oauth-21)). Conveyor launches no agent and mints no
credential for one (DEC-97(1)).

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

The CLI resolves the credential per server. `CONVEYOR_API_TOKEN` in the
environment is the credential only for its environment server — the normalized
`CONVEYOR_ADDR`, or `http://localhost:8080` when unset. Any other resolved
server ignores the environment token and uses its stored credential, and
`conveyor auth status` plus 401 errors report which source won without ever
printing a credential value.

Deliberate asymmetries between the two methods:

- Changing your own password or profile requires a session, not a token, so a
  leaked PAT cannot take over the account.
- Claiming and working a task over MCP requires a bearer token, not a
  session, so a browser cannot be tricked into claiming work.
- An MCP OAuth access token works only on `/mcp`. Every other route,
  including the claim-bound `/v1/work-orders/*` delivery routes that
  `conveyor submit` uses, refuses it with 401, so the CLI keeps using your
  personal access token.
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

The dashboard and API issue a link only for a pending invitation in the
workspace being managed: inviting an email that has no account, or resending
that workspace's pending invitation. Granting a role to an existing account
records the binding and issues no link. A resend for an email without a
pending invitation in that workspace answers `404 workspace_not_found`, and a
successful resend is recorded as `workspace.invitation_resent`. Recovery for
an existing account is the host-local `conveyor user issue-link <email>`.

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

That token row is the deployment marker, and its owner is the deployment
owner (DEC-75). Later starts keep the same owner even if the first-operator
variables change; changing `CONVEYOR_API_TOKEN` rotates the token for that
owner. A revoked deployment token is refused at startup, and a deactivated
owner stops startup rather than being replaced by another operator.

Bootstrap never writes workspace memberships. When the deployment file names a
workspace and the database has no workspaces yet, that first workspace is
created with the owner as its operator. Otherwise startup leaves memberships
alone, so demotions and removals survive restarts and token rotation. Without
a configured workspace, the owner creates the first workspace with
`POST /v1/workspaces` and becomes its operator.

## Instance administration and workspace creation

Two acts are deployment-wide rather than per workspace: provisioning an
account (`POST /v1/users`) and creating a workspace (`POST /v1/workspaces`).
Only the deployment owner may provision accounts, with their own personal
access token (any scope) or dashboard session. No workspace role grants it,
not even `operator`. Creating a workspace is open to the deployment owner and
to any active account that is an operator of at least one workspace, under the
same kinds of credential (DEC-75). Neither right
grants anything inside other workspaces: the owner reads and manages a
workspace only through a membership there, like anyone else, and the creator
of a workspace becomes its operator and gains nothing elsewhere.

Any other signed-in person gets the same `404 workspace_not_found` from
`POST /v1/users`, and anyone who is neither the owner nor an operator
somewhere gets it from `POST /v1/workspaces`, whatever they send, before
anything is created or looked up. A deactivated account is refused even if
its bindings remain. A missing or invalid credential gets `401`. `GET /v1/me` reports the caller's own creation right
as `can_create_workspaces`.

For the owner, `POST /v1/users` answers `200` with exactly `{"accepted":true}`
for a new account, an existing active account, and a deactivated account. It
returns no account details. A deactivated account stays deactivated and its
pending invitations stay pending. For an admitted creator,
`POST /v1/workspaces` answers `201` with the new workspace, or `409` with
`{"error":"workspace_conflict","message":"workspace could not be created"}`
when the ID or the case- and space-insensitive name is taken, including by a
workspace the creator cannot see. The response does not say which.

## Roles and capabilities

Authorization is per workspace. Every check names a capability, and roles are
just bundles of capabilities, each strictly containing the previous:

| Role | Capabilities added |
|---|---|
| `viewer` | `view_workspace` |
| `executor` | `claim_work`, `request_changes` |
| `contributor` | `propose_documents` |
| `maintainer` | `create_tasks`, `set_assignee`, `operate_gates`, `recover_work`, `manage_reference_documents` |
| `operator` | `confirm_documents`, `manage_membership`, `manage_workspace` |

Two boundaries are worth internalizing. Confirming documents is operator-only,
and it is deliberately not bundled with `operate_gates`: a maintainer can
approve plans and merges all day without ever being able to change what the
factory considers confirmed intent. Reference documents are informative, so
maintainers upload, revise, and remove them through
`manage_reference_documents` without gaining confirmation authority. And a
workspace can never lose its last operator; the server refuses the demotion.

Filing a task needs `create_tasks`, which maintainers and operators hold. It
is named apart from `operate_gates` because filing a task is not a gate
decision: a session may file one for a person who holds it (DEC-116(5)), as
described under [What an agent session gets](#what-an-agent-session-gets).

Requests against a workspace you are not a member of return 404, not 403, so
membership existence is not disclosed.

Demoting a user below `claim_work` clears their task assignments in the same
operation.

## GitHub App and local credentials

Connect the workspace GitHub App in Workspace settings and install it on each
registered repository. The server encrypts the App private key under
`CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY` and mints short-lived installation tokens
for control-plane GitHub operations. It stores no personal GitHub token.

The machine where an agent session works resolves its own Git credential:
`conveyor submit` pushes and opens the pull request with `CONVEYOR_GIT_TOKEN`
when it is set, otherwise with the host's Git credential for the repository
host. Claims do not depend on a token stored in account settings.

A merge from Conveyor uses the App and records the approving operator in the
merge event and commit message. An approved pull request merged on GitHub
completes through reconciliation when its recorded identity and approved head
match; the event records the GitHub actor.

## What an agent session gets

An agent session you start in Claude Code, Codex, Cursor, or OpenCode reaches
Conveyor's MCP endpoint through the registration `conveyor mcp install`
wrote. The registration presents your saved personal access token through the
harness's native header helper or a server-specific environment variable,
never by value in the registration file (see
[Native MCP credentials](#native-mcp-credentials)). The session claims a work
order itself with a session ID and a client token it generates, and the CLI
commands it runs, such as `conveyor checkout` and `conveyor submit`, use your
stored credential.

Everything the session can do through a claim is scoped: claim-bound mutation
tools work only against its own live claim, artifact reads are bounded to
what the work order's lineage selection actually served, and document content
injected into prompts is labeled untrusted data. Recording a gate or proposal
decision remains your own act; a session records one only on your direct
instruction, with your credential (DEC-84).

An agent session acts as the person whose token it holds. It has that person's
capabilities, and its acts are recorded as that person's. The server cannot tell
the person from the session when it authorizes a request, so an instruction
injected into a session can reach anything that person's role allows. Whether a
session may record its person's decision or author a document version is a rule
of conduct, held by the MCP tool list, the work-order instructions, and your
confirmation of every proposal, not by a credential check (DEC-116(2), (4)).

A session may file a task with the `create_task` MCP tool when its person holds
`create_tasks` in that workspace. The task records MCP as its source and the
person as its creator, and enters triage like any other task. An omitted plan or
merge gate takes the workspace default, and a boolean the caller supplies
applies, `false` included. Filing the task gives the session nothing further over
it (DEC-116(5)).

Earlier releases had worker credentials, worker enrollment, and agent-class
credentials minted for launched children. Neither class exists any more, and no
route, tool, or command issues either (DEC-97(1), DEC-116(1)). A stored
`kind='agent'` row is revoked and refused at its next use like any unknown
credential. The row stays, and the acts recorded under it stay readable: wherever
Conveyor shows an `agent:<id>` actor, it also names the user who owns that
credential as `actor_owner_user_id` (DEC-116(3)).

## Workspace context

Workspace scope is explicit everywhere. REST accepts the workspace as a path
segment, a `workspace_id` query parameter, or the `X-Workspace-ID` header,
and rejects disagreements between them. The CLI resolves it from `--workspace`,
`CONVEYOR_WORKSPACE`, then the stored per-server default. Omitting the
workspace entirely is compatible only when you belong to exactly one; with
zero the server says to create one first, with several it demands explicit
context.

## MCP OAuth 2.1

`conveyord` hosts an OAuth 2.1 authorization server for `/mcp` (DEC-146;
`req-mcp-oauth`). An MCP client such as Claude Code, Codex, Cursor, OpenCode,
or Antigravity discovers it, signs you in through the dashboard, and receives
an access token that works only on `/mcp`. The token is yours: every MCP call
it makes runs with your live account status and workspace roles and is
recorded as your act.

### Enabling it

OAuth is on only when `CONVEYOR_PUBLIC_URL` is a canonical origin:

- `https://<host>` or `https://<host>:<port>`, in lowercase, with no path,
  query, fragment, or user information and no default port; or
- for local development, `http://127.0.0.1`, `http://[::1]`, or
  `http://localhost`, with an optional port.

A trailing slash is removed when the setting is read. When the setting is
unset or is not such an origin, the discovery documents, registration,
consent, token, and revocation endpoints answer `404` with
`{"error":"oauth_unavailable"}`, and `/mcp` challenges with a bare
`WWW-Authenticate: Bearer`. The server never derives an issuer from a
request's `Host` or forwarded headers. While issuance is off, you can still
list and revoke your existing grants under `/v1/oauth/grants`. A backend
without OAuth storage turns those grant routes off too.

`CONVEYOR_TRUSTED_PROXIES` is an optional comma-separated list of proxy IP
addresses or CIDR prefixes. Client registration is rate limited per source
address. The source is the connection's peer address, or, when the peer is a
listed proxy, the nearest `X-Forwarded-For` address that is not a listed
proxy, read from the right. A malformed `X-Forwarded-For` chain is ignored. An
IPv4-mapped prefix such as `::ffff:10.0.0.0/104` is read as the IPv4 prefix it
covers. An invalid setting is logged at startup and trusts no proxy. Password sign-in
keeps its own limiter, which reads only the peer address.

### Discovery

| Route | Answer |
| --- | --- |
| `GET /.well-known/oauth-protected-resource` and `/.well-known/oauth-protected-resource/mcp` | RFC 9728 metadata: `resource` `<public-url>/mcp`, `authorization_servers` `[<public-url>]`, `bearer_methods_supported` `["header"]` |
| `GET /.well-known/oauth-authorization-server` | RFC 8414 metadata: `issuer` `<public-url>`, `authorization_endpoint` `<public-url>/oauth/authorize`, `token_endpoint`, `revocation_endpoint`, `registration_endpoint`, `response_types_supported` `["code"]`, `grant_types_supported` `["authorization_code","refresh_token"]`, `code_challenge_methods_supported` `["S256"]`, `token_endpoint_auth_methods_supported` `["none"]`, and `client_id_metadata_document_supported` `true` |

Other `/.well-known/*` paths answer a JSON 404. A request to `/mcp` with no
valid bearer answers 401 with
`WWW-Authenticate: Bearer resource_metadata="<public-url>/.well-known/oauth-protected-resource/mcp"`.

### Clients

Every client is public: it holds no secret and proves itself with PKCE `S256`.
A client identifies itself in one of two ways.

- **Client ID Metadata Document.** The `client_id` is an `https://` URL with a
  path, at most 2,048 bytes, with no fragment, user information, or dot
  segments. Conveyor fetches the JSON document at that URL when you open the
  consent page and again when the client exchanges its code. The document must
  name the same `client_id`, set `token_endpoint_auth_method` to `none`, allow
  only the `authorization_code` and `refresh_token` grants, and list 1 to 16
  redirect URIs. The fetch uses no proxy. It resolves the host, refuses the
  fetch when any address is private, loopback, link-local, unspecified,
  multicast, documentation, or another special-purpose range, and connects
  to the checked
  address while still verifying TLS for the host name. Each redirect is
  checked the same way. A fetch follows at most 3 redirects, takes at most 5
  seconds, and reads at most 64 KiB. A verified document is reused for up to
  5 minutes, at most 256 documents per process.
- **Dynamic Client Registration.** `POST /oauth/register` (RFC 7591) takes a
  JSON body of at most 16 KiB. It needs `token_endpoint_auth_method` `none`
  and 1 to 8 `redirect_uris`. `grant_types` may hold `authorization_code` and
  `refresh_token` and defaults to `authorization_code`. `response_types` may
  hold only `code`. `client_name` is optional, at most 256 bytes. Other RFC
  7591 fields are ignored. Each source address may register 10 clients per
  minute; more answer `429` with `Retry-After`. Invalid input answers `400`
  `invalid_client_metadata` or `invalid_redirect_uri` and stores nothing. A
  client that has not received a grant within 24 hours expires, and
  registration removes expired clients at most once every 10 minutes per
  process. Removal never touches a client that has a grant.

A redirect URI must be `https://`, or `http://` on `127.0.0.1`, `[::1]`, or
`localhost`, with no fragment or user information. Conveyor compares redirect
URIs as exact strings. Client metadata that holds a credential-shaped value is
refused.

### Consent

The `authorization_endpoint` is the dashboard page `/oauth/authorize`. It
reads the session-only consent API under `/v1`, so the `conveyor_session`
cookie stays at `Path=/v1` and `SameSite=Strict`. A personal access token or
OAuth token is refused on this API with 401.

`GET /v1/oauth/authorize?<the client's authorization query>` checks the
request and changes nothing. The query takes `response_type` (`code`),
`client_id`, `redirect_uri`, `code_challenge`, `code_challenge_method`
(`S256`), an optional `state` of at most 2,048 bytes, and an optional
`resource` that must equal `<public-url>/mcp` when present. A parameter given
twice is refused. A valid request answers:

```json
{
  "client": {"client_id": "...", "client_name": "...", "kind": "metadata_document", "origin": "https://client.example"},
  "redirect_host": "127.0.0.1:33418",
  "user": {"id": "usr_...", "email": "you@example.com", "display_name": "You"},
  "resource": "https://conveyor.example/mcp"
}
```

`kind` is `metadata_document` or `dynamic_registration`, and only a
metadata-document client has an `origin`.

`POST /v1/oauth/authorize` records your decision. It needs the session's
`X-Conveyor-CSRF: 1` header and `Origin`. Its JSON body repeats the same
request fields and adds `decision`, which is `approve` or `deny`. The server
checks the whole request again. Approval mints an authorization code
(`cv_oac_...`) that is single use, expires after 60 seconds, and is bound to
you, the client, the exact redirect URI, the resource, and the code
challenge. Both decisions answer `200` with `{"redirect_to": "..."}`: the
redirect URI with `code` and `state` added on approval, or with
`error=access_denied` and `state` on denial. The redirect URI's other query
parameters are kept as written, and any stale `code`, `state`, `error`,
`error_description`, `error_uri`, or `iss` parameter is removed.

An error answers JSON with `error` and `error_description`. When the client
or its redirect URI could not be verified (`invalid_client`,
`invalid_redirect_uri`, or a malformed request), the answer has no
`redirect_to`, and the page shows the error itself. When both were verified
and another parameter is wrong (`unsupported_response_type`,
`invalid_request`, or `invalid_target`), the answer also carries a
`redirect_to` that returns the error and `state` to the client.

Consent, token, revocation, and grant responses send
`Cache-Control: no-store` and `Referrer-Policy: no-referrer`, errors
included. The access log records OAuth paths without their query strings.

### Tokens

`POST /oauth/token` takes an `application/x-www-form-urlencoded` body of at
most 16 KiB. Parameters in the query string, or given twice, are refused.

- `grant_type=authorization_code` needs `code`, `client_id`, `redirect_uri`,
  `code_verifier` (43 to 128 characters), and `resource` set to
  `<public-url>/mcp`. The code must match every binding and the verifier.
  Exactly one exchange of a code can succeed. A refused exchange consumes
  nothing.
- `grant_type=refresh_token` needs `refresh_token`, `client_id`, and
  `resource` set to `<public-url>/mcp`.

Both answer `{"access_token","token_type":"Bearer","expires_in","refresh_token"}`.
The access token (`cv_oat_...`) lives 1 hour and the refresh token
(`cv_ort_...`) lives 30 days. Each refresh returns a new pair and retires the
refresh token it used. Conveyor stores every code and token only as a SHA-256
hash.

A refresh token that was already used is refused with `invalid_grant`, and
the whole grant is revoked at once, including the newest pair. This also
happens when a client retries a refresh after a network failure, or sends two
refreshes of one token at the same time: the client must send you through
sign-in again. Other refusals are `invalid_grant` without detail (an unknown,
expired, revoked, or mismatched code or token), `invalid_target` (a wrong or
missing `resource`), `invalid_client`, `invalid_request`, and
`unsupported_grant_type`. A brief storage conflict answers `503`
`temporarily_unavailable` with `Retry-After`, and the same request can be sent
again.

### Revoking access

- **From the client.** `POST /oauth/revoke` (RFC 7009) takes form fields
  `token` and `client_id`. An access or refresh token of that client revokes
  its whole grant. The answer is `200` with an empty body whether or not the
  token was known, belonged to that client, or was already revoked.
- **From Settings.** `GET /v1/oauth/grants` lists your own active grants with
  `id`, `client_id`, `client_name`, `resource`, `created_at`, and
  `last_used_at`. `last_used_at` changes at most once a minute. No token value
  or hash appears. `DELETE /v1/oauth/grants/{grant_id}` revokes one of your
  grants and answers it; any other ID answers 404. Both routes accept a
  session or a personal access token, and a session `DELETE` needs the CSRF
  proof.
- **By deactivation.** When the instance administrator deactivates an
  account, all of its OAuth tokens are refused at their next use.

A revoked token is refused at its next request. A workspace role change
applies to the next MCP call.

## Native MCP credentials

After login, `conveyor mcp install --server <base> --name <connection>` binds a
personal registration to that server's saved credential. Codex and Claude use
`conveyor auth token --format http-headers` through their native header helpers.
The installer includes the absolute executable and credential-file paths so
desktop launches resolve the same store. No secret appears in the registration.
Cursor and OpenCode use a distinct environment variable per canonical server;
run the server-specific export printed by the installer before launching them.

The token-print bridge always reads the selected saved entry, including after
rotation. It does not use an unrelated environment token. After a native
rejection, log in again for the selected server and reconnect; environment-based
clients also need their export refreshed. Server-side revocation takes effect
on use. See [MCP personal connections](mcp.md#personal-connections) for naming,
legacy migration, client refresh behavior, and separate parser/native checks.
