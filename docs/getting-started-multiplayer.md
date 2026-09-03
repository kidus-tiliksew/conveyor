# Getting started: multiplayer

Multiplayer mode is one shared `conveyord` server with several people holding
their own accounts, roles, and GitHub identities, each running agents on their
own machines. The server setup is the same as solo; what changes is identity,
credentials, and who may do what.

This guide assumes you have read [Getting started: solo](getting-started-solo.md).
Steps 1 through 4 there (install, environment, `conveyor init`, start the
server) apply unchanged to the team host. This page covers what the host needs
beyond that, and what each contributor does.

## Host: extra environment

Beyond the solo environment, a team server needs:

```sh
export CONVEYOR_PUBLIC_URL='https://factory.example.com'
```

- `CONVEYOR_PUBLIC_URL` is the address users reach the dashboard at. Sign-in
  links are minted against it, and the server checks request origins against
  it, so set it before inviting anyone.
- `CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY`, already part of the solo setup,
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

Every contributor, on their own machine:

1. Open the sign-in link, set a display name and password on the onboarding
   page.
2. On Settings, mint a personal access token, and save a GitHub token
   (fine-grained, Contents read/write and Pull requests read/write on the
   team's repositories). Without the GitHub token the server refuses to let
   you claim work, because it could not open PRs as you.
3. Connect the CLI and agent tooling:

```sh
conveyor --server https://factory.example.com auth login
conveyor skills install
conveyor --server https://factory.example.com mcp install
```

The install commands detect Claude Code, Codex, and Cursor. Use `--tool` to
narrow either command to one client. Cursor MCP installation writes the owned
global `~/.cursor/mcp.json` entry and leaves project-level configuration alone.
Before an operator starts Cursor, bridge the selected server and stored
credential through the environment:

```sh
export CONVEYOR_ADDR=https://factory.example.com/mcp
export CONVEYOR_API_TOKEN=$(conveyor auth token)
```

4. Create a local execution setup describing the agent CLIs on this machine:

```sh
conveyor config init-execution
```

Execution setups are local by design. Your harness commands, models, and
review seats never leave your machine; the server only learns whether a
serviceable harness is present.

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
