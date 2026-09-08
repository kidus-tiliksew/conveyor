# Getting started: multiplayer

Multiplayer mode is one shared `conveyord` server with several people holding
their own accounts, roles, and GitHub identities, each running agents on their
own machines. The server setup is the same as solo; what changes is identity,
credentials, and who may do what.

The host follows [Server setup](server-setup.md) once, using the team's public
HTTPS URL. Every contributor follows [Client setup](client-setup.md)
on their own machine. Contributors do not initialize a server or database.
This page covers invitations, roles, and shared GitHub identity settings.

## Host: extra environment

The server environment file needs the public URL before initialization:

```sh
CONVEYOR_PUBLIC_URL=https://factory.example.com
```

- `CONVEYOR_PUBLIC_URL` is the address users reach the dashboard at. Sign-in
  links are minted against it, and the server checks request origins against
  it, so set it before inviting anyone.
- `CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY`, already part of server setup,
  carries more weight here: it encrypts every member's stored GitHub token
  (AES-256), and without it nobody can save the token that task execution
  requires. Generate it once and keep it stable; rotating it invalidates
  stored tokens.
- Optionally configure SMTP (`CONVEYOR_SMTP_HOST`, `CONVEYOR_SMTP_PORT`,
  `CONVEYOR_SMTP_USERNAME`, `CONVEYOR_SMTP_PASSWORD`, `CONVEYOR_SMTP_FROM`)
  so invitations email themselves. Without SMTP, invitation links are shown
  in the dashboard for you to deliver by hand, which is fine for a small
  team.

## Workspace and user GitHub accounts

Store a workspace GitHub token in Workspace settings for issue and review
publication. Store each contributor's token in their account settings so task
pull requests use the executing user's identity. A gated merge uses the stored
token of the operator who approved it; if that token is missing, Conveyor keeps
the approval and waits until the operator adds one. The server does not fall
back to the host identity. Forge-write events record `workspace`,
`executing_user`, or `approving_operator` without recording token values.

## Host: invite the team

Membership is managed on the Workspace page (or over
`POST /v1/workspaces/{id}/members`). An invitation names an email and a role.
If no account exists for that email yet, redemption of the sign-in link
creates one.

Roles nest strictly; each includes everything below it:

| Role | Adds |
|---|---|
| `viewer` | see the workspace |
| `executor` | claim work orders, request changes |
| `contributor` | propose documents |
| `maintainer` | assign tasks, operate gates, recover stuck work |
| `operator` | confirm documents, manage membership and the workspace |

The split that matters most day to day: contributors and agents can propose
requirement, design, and decision revisions, but only an operator can confirm
one. Confirmation is the act that changes what the factory builds against, and
it stays with operators on purpose. A workspace must always keep at least one
operator; the server refuses to demote the last one.

Sign-in links expire after 30 minutes and are single use. If someone misses
the window, resend from the dashboard, or on the host:

```sh
conveyor user issue-link teammate@example.com
```

## Each contributor: sign in and connect

Give each contributor their invitation link, server URL, workspace ID,
registered repository name, and repository URL. They then follow
[Client setup](client-setup.md), including workspace selection,
account and local Git credentials, their repository clone, and execution setup.
The host follows the same client guide if they will operate or execute tasks.

## Dividing the work

Nothing else changes structurally: anyone with `claim_work` can run
`conveyor run <task-id>` or enroll a worker, and unassigned tasks go to
whoever claims first. Two levers shape who works on what:

- An assignee restricts claim eligibility to one person. It never reorders
  the queue; it only narrows who may take the task.
- `hold` reserves a task from everyone's workers so a person can attach an
  agent and claim it by hand.

Workers enroll per user via `conveyor worker pair` and inherit the enrolling
user's GitHub identity for the work they run. See
[Worker operations](worker-operations.md) for pairing, service install, and
recovery.

## Where to go next

- [Authentication](auth.md) for the full credential model, including what
  agents and workers authenticate as
- [Misalignment](misalignment.md) for the signals operators are expected to
  watch and judge
