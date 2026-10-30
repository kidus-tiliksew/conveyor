# Conveyor setup

You are setting up Conveyor with a person. You interview them about their
situation, agree a setup with them, and then perform it yourself through the
release installer, `conveyor`, `conveyord`, and the REST API (AC-3.1),
handing them only the acts that need their secrets, their browser, or their
decision. This playbook is the only setup procedure (DEC-92). The reference
pages it links describe commands, settings, and behavior in more detail.

You can follow this playbook before Conveyor is installed. It is published
at
`https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/docs/playbooks/conveyor-install.md`,
fetchable over HTTPS without a clone, a binary, or a credential
(req-agent-guided-setup AC-1.1). After installation the CLI also carries it
as the `conveyor-install` skill (AC-1.3).

## Ground rules

These rules hold in every section.

- **Interview before changing anything.** Sections 2 to 5 read state and ask
  questions. Make no change to the machine or a deployment until the person
  accepts the setup summary in section 5 (REQ-2).
- **Confirm each act that changes deployment state.** Before factory
  initialization, service installation, workspace creation, repository
  registration, an invitation, or worker enrollment, name the act and wait
  for the person's confirmation in the session (AC-3.2).
- **Never handle a secret value.** Do not read, display, store, or transmit
  a password, personal access token, sign-in link, pairing token, database
  password, model provider API key, GitHub App private key, or SMTP
  credential (AC-3.3). Give the person the exact command or page that takes
  the secret and resume when they report completion. You may generate a new
  secret straight into a file readable only by the person's account, as
  section 6 shows, without displaying it. Never print a file that holds a
  secret: the server `.env`, the generated deployment `conveyor.yaml`, which
  records the database URL, or the CLI credentials file. Never print the
  output of `conveyor auth token`; pipe it straight into a request header as
  section 8 shows. Never print a REST response body, because some carry
  secrets: an invitation response holds the invitee's sign-in link. Save the
  body to an owner-only file, print the HTTP status and only an allowlist of
  nonsecret fields, and delete the file, as section 8 shows.
- **Hand browser acts to the person.** When a step needs their browser, such
  as accepting an invitation, minting a token, or connecting the workspace
  GitHub App, give the link and the action to take there, and resume when
  they report completion (AC-3.4).
- **Stop at a failure.** When a step fails, report the command and its
  error, and run no step that depends on it (AC-3.6). Retry only after the
  cause is fixed.
- **Leave operator acts to the person.** Never approve a plan or a merge,
  confirm or dismiss a document proposal, or resolve drift. Those acts belong
  to the person through the dashboard or their own commands.
- **Name the server and workspace.** On every client command after sign-in,
  pass `--server` and, for workspace commands, `--workspace`. Run client
  commands outside the server directory, because both binaries load `.env`
  from the working directory and the server's `.env` holds the bootstrap
  token.

## 1. Install the CLI and this skill

This section is the first step of every agreed setup. Run it only after the
person accepts the summary in section 5. Each machine in the setup installs
the CLI.

The release installer needs `curl`, `tar`, and a SHA-256 tool (`sha256sum`
or `shasum`), and does not need `sudo`. It supports Linux and macOS on amd64
and arm64, verifies the archive against the release's `checksums.txt`, and
installs `conveyor` and `conveyord` into `~/.local/bin`
(`CONVEYOR_INSTALL_DIR` selects another directory):

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
conveyor --version
conveyord version
```

To pin a release, replace `v1.2.3` with an existing tag:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/v1.2.3/install.sh | sh -s -- v1.2.3
```

Ask the person before adding the `PATH` line to their shell startup file.
The installer prints `Next: conveyor init`; ignore it until section 6.

Then install the embedded skills for the agent tools on this machine, so
later sessions load this playbook natively as the `conveyor-install` skill:

```sh
conveyor skills install
```

The command detects Claude Code, Codex, Cursor, and OpenCode on `PATH`,
writes user-global skills for each, and reports one line per file. It
refuses to overwrite a file it did not write. `--tool` narrows it to one
tool. Restart the agent session to load new skills.

## 2. Read the machine before asking

Read observable state first, without changing it. For each answer you can
determine, state what you found and ask the person to confirm it (AC-2.2).

```sh
command -v conveyor conveyord git docker claude codex cursor-agent opencode grok
conveyor --version
conveyord status
conveyor auth status
conveyor config list
conveyor setup list
conveyor skills install --list
conveyor mcp install --list
```

- `conveyord status` reports whether a server service is installed on this
  machine.
- `conveyor auth status` reports the effective server, the signed-in
  identity, and the credential's source, never its value.
- `conveyor config list` reports the effective server, workspace, and local
  execution configuration, with the source of each.
- For a server address the person names, read
  `curl -fsS <server-url>/v1/version`. It needs no credential.
- If a `~/.conveyor/server` directory exists, list its file names with
  `ls -la`. Do not print its files.

A command that fails because Conveyor is absent is a finding, not a failure.

## 3. Interview

Ask in plain language, one topic at a time, and skip a topic that observable
state already answered and the person confirmed. Before the first change,
establish each of these (AC-2.1):

1. **Join or host.** Does the person join an existing factory, or host a
   new one? To join, ask for the server URL, the workspace ID, and the
   repository names they will work on, skip questions 2 to 5, and build the
   summary in section 5 from the join steps in section 12.
2. **Where the server runs.** For a new factory: on this machine as a
   foreground process, as a long-running user service on a host
   (`conveyord install`), or on a container platform from the release
   image.
3. **Database.** PostgreSQL 15 or newer is the default. SingleStore is the
   documented alternative. Ask whether a database already runs, or whether
   to start a local PostgreSQL container with Docker.
4. **Who the factory serves.** One person, one team sharing one workspace,
   or several teams.
5. **Several teams.** If several, do they share one server with a workspace
   per team, or run a separate server each?
6. **Repositories.** Which repositories to register, with each one's URL and
   default branch. Conveyor works with GitHub repositories through the
   workspace GitHub App.
7. **Agent tools.** Which coding-agent tools to connect: Claude Code, Codex,
   Cursor, or OpenCode for skills and MCP, and which harness runs each stage
   (Codex, Claude, Grok, Cursor, or OpenCode).
8. **Execution.** Do tasks run in local agent sessions (`conveyor run`),
   under a background worker, or both?

For a team server, also ask for the public HTTPS URL people will use, and
whether invitations should be emailed through SMTP or delivered by hand.

## 4. Map the answers to a supported setup

Choose one topology:

| Situation | Topology |
|---|---|
| One person | A solo factory: server, database, and client on this machine, at `http://127.0.0.1:8080`. |
| One team | One server with one workspace. Each member has an account with a role, and runs the client on their own machine. |
| Several teams, one server | One server with a workspace per team. A workspace creator becomes its operator; each team's members are invited into their workspace with a role. |
| Several teams, separate servers | One factory per team. Repeat the new-factory path per server. A person on several servers keeps one sign-in and one MCP registration per server. |

Choose one hosting form for the server:

- **This machine, foreground.** `conveyord -config <path>` runs until its
  shell ends. Suits a trial.
- **User service on a host.** `conveyord install` installs a systemd user
  unit on Linux or a launchd user agent on macOS. If the host is another
  machine, the steps in section 6 run on that host, in an agent session
  there or over access the person already has.
- **Container platform.** The release image
  `ghcr.io/kidus-tiliksew/conveyor:<version>` contains both binaries and
  `git`, and no credentials or configuration.

When an answer needs a configuration Conveyor does not support, say so and
offer the supported alternatives (AC-2.4). Unsupported configurations
include:

- an operating system other than Linux or macOS, or an architecture other
  than amd64 or arm64, for the released binaries; offer the container image
  on a supported platform;
- a database other than PostgreSQL 15 or newer or SingleStore, such as MySQL,
  SQLite, or an in-memory store; offer PostgreSQL;
- a model endpoint without the Responses API features Conveyor uses (tools
  and structured output), such as a chat-completions-only endpoint, for the
  in-process triage and planning stages;
- a forge other than GitHub for the workspace GitHub App;
- a root or system-wide service manager unit; `conveyord install` and
  `conveyor worker install` install per-user services only, so offer a user
  service, with Linux user lingering when it must run without a login;
- one workspace spread across several servers; offer one server per team or
  one shared server;
- self-registration; accounts exist through bootstrap, invitation, or the
  deployment owner's provisioning.

## 5. Present the setup summary

Before the first change, present a summary and proceed only after the person
accepts it (AC-2.3). Name:

- each component (database, server, client CLI, skills, MCP registrations,
  workspaces, repositories, execution setups, worker) and where it runs;
- each step in order, marking the steps the person performs: entering a
  secret, a browser act, or a confirmation;
- what the setup leaves out.

If the person changes an answer, revise the summary and ask again.

## 6. Host a new factory

Skip this section when the person joins an existing factory. The server
directory keeps the server's files apart from the client's, even on one
machine. Run every command in this section from it.

### 6.1 Start a database

For a new local PostgreSQL with Docker, generate the database password
straight into owner-only files. The password never appears in output:

```sh
mkdir -p "$HOME/.conveyor/server" && cd "$HOME/.conveyor/server"
umask 077
pw=$(openssl rand -hex 24)
printf 'POSTGRES_USER=conveyor\nPOSTGRES_DB=conveyor\nPOSTGRES_PASSWORD=%s\n' "$pw" > postgres.env
printf 'CONVEYOR_DATABASE_URL=postgres://conveyor:%s@127.0.0.1:5432/conveyor?sslmode=disable\n' "$pw" > .env
unset pw
docker run -d --name conveyor-postgres --restart unless-stopped \
  --env-file postgres.env -p 127.0.0.1:5432:5432 \
  -v conveyor-postgres-data:/var/lib/postgresql/data postgres:16-alpine
docker exec conveyor-postgres pg_isready -U conveyor -d conveyor
```

Wait for `accepting connections`. If port 5432 is taken, choose another host
port in both the `-p` mapping and the URL before running the block.

For an existing PostgreSQL or a hosted database, ask the person to add the
line `CONVEYOR_DATABASE_URL=<their connection URL>` to
`~/.conveyor/server/.env` in their editor, with TLS where the provider
offers it, and to create the file with mode 0600. For SingleStore, the
person creates an empty database and adds its URL the same way; the URL
forms and operating rules are in
[SingleStore operations](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/singlestore.md).
Check that the line exists without printing it:

```sh
grep -c '^CONVEYOR_DATABASE_URL=.' "$HOME/.conveyor/server/.env"
```

### 6.2 Complete the server environment

Generate the bootstrap token and the GitHub App key encryption key into the
same file, and record the public URL:

```sh
cd "$HOME/.conveyor/server"
umask 077
printf 'CONVEYOR_API_TOKEN=%s\n' "$(openssl rand -hex 32)" >> .env
printf 'CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY=%s\n' "$(openssl rand -base64 32)" >> .env
printf 'CONVEYOR_PUBLIC_URL=%s\n' 'http://127.0.0.1:8080' >> .env
chmod 600 .env
```

For a team server, record the public HTTPS URL instead of
`http://127.0.0.1:8080`. Conveyor provides no reverse proxy: DNS, TLS, and a
proxy to the daemon's listener stay with the person's own infrastructure.

Then hand the person these secrets to add to `.env` in their editor, as
`KEY=value` lines without `export`:

- `CONVEYOR_LLM_API_KEY`, the model provider key for in-process triage and
  planning. For a provider other than `https://api.openai.com/v1`, also
  `CONVEYOR_LLM_BASE_URL`.
- For emailed invitations: `CONVEYOR_SMTP_HOST`, `CONVEYOR_SMTP_PORT`,
  `CONVEYOR_SMTP_USERNAME`, `CONVEYOR_SMTP_PASSWORD`, and
  `CONVEYOR_SMTP_FROM`. Without SMTP, invitation links appear in the
  dashboard for manual delivery.

Check the keys are present, printing names only; a line without `=` is not
printed:

```sh
sed -n 's/=.*//p' "$HOME/.conveyor/server/.env"
```

Keep `CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY` stable: a new value cannot
decrypt stored GitHub App keys. Every variable is described in
[Configuration](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/configuration.md#environment-variables).

### 6.3 Initialize the factory

Confirm the act with the person first. `conveyor init` creates the
organization, the first operator, and the first workspace, registers the
first repository by name, URL, and default branch, and writes the
deployment `conveyor.yaml` with mode 0600. Give every answer as a flag, so
nothing prompts (req-agent-guided-setup AC-5.1). The output carries the
first sign-in link, which holds a token, so send it to an owner-only file
and show only the summary lines:

```sh
cd "$HOME/.conveyor/server"
umask 077
conveyor init --organization 'Example Org' \
  --operator-name 'Ada Lovelace' --operator-email ada@example.com \
  --workspace-id demo --workspace-name Demo \
  --repository-name app --repository-url https://github.com/example/app \
  --default-branch main > init-output.txt
sed -n '/^Initialized/,/^Next:/p' init-output.txt
```

Replace each example value with the person's answer. Init exits nonzero and
names each missing answer when one is blank. Rerunning init with the same
answers issues a fresh sign-in link and changes nothing else.

Review the control-plane models with the person before starting the server.
Print only the model lines:

```sh
grep -n 'model' "$HOME/.conveyor/server/conveyor.yaml"
```

Change `execution_settings.control_plane.triage.model` and
`execution_settings.control_plane.planning.model` to model IDs the
person's endpoint serves. The other deployment settings are described in
[Configuration](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/configuration.md#deployment-config-server).

### 6.4 Start the server

Confirm the act with the person first. Use the hosting form from section 4.

**User service.** On the server host:

```sh
cd "$HOME/.conveyor/server"
conveyord install --config "$HOME/.conveyor/server/conveyor.yaml"
conveyord status
curl -fsS http://127.0.0.1:8080/healthz
```

The service reads the owner-only `.env` beside the config; `--env-file`
names another file. On Linux, the unit stops at logout unless the person
enables user lingering, a host policy they decide.

**Foreground on this machine.**

```sh
cd "$HOME/.conveyor/server"
nohup conveyord -config "$HOME/.conveyor/server/conveyor.yaml" > conveyord.log 2>&1 < /dev/null &
curl -fsS http://127.0.0.1:8080/healthz
```

**Container platform.** The image's entrypoint is `conveyord`. The process
needs `CONVEYOR_API_TOKEN`, `CONVEYOR_DATABASE_URL`, `CONVEYOR_LLM_API_KEY`,
`CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY`, and `CONVEYOR_PUBLIC_URL` in its
environment, the deployment file at the path `-config` names, a
container-reachable listen address, and a database the container can reach.
The person moves the `.env` values and `conveyor.yaml` into the platform's
secret facility, because both hold secrets; never put them into an image.
With Docker, where the variables are already in the person's environment:

```sh
docker run --rm -p 8080:8080 \
  -v "$HOME/.conveyor/server/conveyor.yaml:/etc/conveyor/conveyor.yaml:ro" \
  -e CONVEYOR_API_TOKEN -e CONVEYOR_DATABASE_URL -e CONVEYOR_LLM_API_KEY \
  -e CONVEYOR_GITHUB_APP_KEY_ENCRYPTION_KEY -e CONVEYOR_PUBLIC_URL \
  ghcr.io/kidus-tiliksew/conveyor:v1.2.3 \
  -config /etc/conveyor/conveyor.yaml -addr 0.0.0.0:8080
```

Pin a release tag; `latest` follows stable releases only. Set `GOMEMLIMIT`
to about 75 to 80 percent of the container memory limit, because the Go
runtime does not read that limit. The default shutdown budget is 25 seconds:
on Kubernetes set `terminationGracePeriodSeconds` to at least 30, and on
Cloud Run, whose termination window is 10 seconds, set
`CONVEYOR_SHUTDOWN_TIMEOUT=8s`. These settings are described in
[Configuration](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/configuration.md#environment-variables).

Startup applies database migrations; an upgrade replaces the binary or image
and restarts it.

### 6.5 First sign-in and the GitHub App

Hand these browser acts to the person:

1. Open `~/.conveyor/server/init-output.txt` themselves, open the sign-in
   link inside it within 30 minutes, and set a display name and a password
   of at least 12 characters. A lost or expired link is replaced by the
   person running `conveyor user issue-link <email>` in the server
   directory; its output is theirs to read.
2. In the dashboard, open the workspace's **Settings**, find the
   **Workspace GitHub App** card, enter the organization name if the app
   belongs to one, choose **Connect GitHub**, create the app on GitHub, and
   install it on every registered repository. The card marks a repository
   **Not covered** until the installation includes it.
3. On **Settings**, mint a personal access token for the CLI. It is shown
   once; keep it for section 7.

Resume when the person reports each act complete. Once they have signed in,
delete the init output without reading it:

```sh
rm "$HOME/.conveyor/server/init-output.txt"
```

The credential model is described in
[Authentication](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/auth.md).

## 7. Connect this machine

Run this section on each machine that operates Conveyor or runs agents,
including the server host when the person works there. Work outside the
server directory.

### 7.1 Sign in

Hand the sign-in to the person. `conveyor auth login` prompts for the
personal access token with hidden input and refuses piped input:

```sh
conveyor auth login --server <server-url>
```

Then select the workspace and check the result:

```sh
conveyor --server <server-url> config set workspace <workspace-id>
conveyor --server <server-url> auth status
```

Use the same server address everywhere, because credentials are stored per
server URL. Resolution rules are in the
[CLI reference](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/cli.md#global-flags-and-resolution).

### 7.2 Skills and MCP

Install the skills if section 1 has not, then register the server with each
chosen agent tool:

```sh
conveyor skills install
conveyor mcp install --server <server-url> --name <connection-name>
```

`--tool claude`, `--tool codex`, `--tool cursor`, or `--tool opencode`
selects one tool, and `--list` previews without writing. Codex and Claude
Code read the saved credential through a native helper. Cursor and OpenCode
read a server-specific environment variable; the installer prints the
export line, which the person adds to the environment that launches the
client, because its value is a token. Ask the person to restart each
client. Naming, migration, and credential details are in the
[MCP reference](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/mcp.md)
and in [Authentication](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/auth.md#native-mcp-credentials).

### 7.3 Clone the repositories

A machine that runs tasks needs a clone of each repository it works on and a
Git credential for the repository host, such as SSH or a credential helper.
The person sets up the credential when it needs a secret. Check access
without a prompt:

```sh
git clone <repository-url> <local-directory>
GIT_TERMINAL_PROMPT=0 git -C <local-directory> ls-remote --heads origin <default-branch>
```

Expect a commit hash and the branch reference.

## 8. Workspaces, repositories, and people

These acts change deployment state; confirm each one first. `conveyor init`
already created the first workspace and registered its first repository.

### 8.1 Authenticated API requests

Workspace creation and invitations use the REST API. Every request in this
section follows one pattern, so neither the credential nor a secret in the
response reaches your context:

- Pipe the CLI credential straight into the request header.
- Write the response body to an owner-only file with `-o`, and print only
  the HTTP status with `-w`. `curl` without `-f` still reports a connection
  failure, and the printed status shows an HTTP failure.
- Print only the allowlisted fields with `grep -oE`, then delete the file.

A status other than the one each step names is a failed step (AC-3.6):
report the status and the allowlisted `error` and `message` fields, and run
no step that depends on it.

```sh
umask 077
conveyor --server <server-url> auth token | sed 's/^/Authorization: Bearer /' |
  curl -sS -H @- -o response.json -w 'HTTP %{http_code}\n' <server-url>/v1/me
grep -oE '"(email|display_name|error|message)":"[^"]*"|"can_create_workspaces":(true|false)' response.json
rm -f response.json
```

Expect `HTTP 200`. `/v1/me` reports the caller's identity and
`can_create_workspaces`. The deployment owner and any operator of a
workspace may create workspaces.

### 8.2 Create a workspace

For several teams on one server, create one workspace per team:

```sh
umask 077
conveyor --server <server-url> auth token | sed 's/^/Authorization: Bearer /' |
  curl -sS -H @- -H 'Content-Type: application/json' -X POST \
  --data '{"id":"team-a","name":"Team A"}' \
  -o response.json -w 'HTTP %{http_code}\n' <server-url>/v1/workspaces
grep -oE '"(id|name|error|field|message)":"[^"]*"' response.json
rm -f response.json
```

Expect `HTTP 201`. The ID is a lowercase slug of 1 to 63 letters, digits,
or hyphens. The creator becomes the workspace operator. `HTTP 409` with
`workspace_conflict` means the ID or name is taken. Workspace creation rules are in
[Authentication](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/auth.md#instance-administration-and-workspace-creation).

### 8.3 Register repositories

A workspace accepts no task until it has a repository. Register more
repositories by round-tripping the workspace configuration:

```sh
conveyor --server <server-url> --workspace <workspace-id> config export > workspace.yaml
```

Add an entry under `repos:` with `name`, `url`, and `base`, and keep
`install_conveyor` true or absent. Then replace the configuration; the
import refuses unknown keys and a concurrent change:

```sh
conveyor --server <server-url> --workspace <workspace-id> config import workspace.yaml
rm workspace.yaml
```

Leave repository preparation to the factory (DEC-40). Registering a
repository with the Install Conveyor switch on files one install task, whose
implementing agent runs `conveyor repo init` in a task worktree and delivers
a pull request. Never run `conveyor repo init` in a checkout of the base
branch, and never commit Conveyor guidance or skills onto a base branch
yourself (AC-3.5). Find the install task with
`conveyor --server <server-url> --workspace <workspace-id> task list` and
run it like any task in section 9; its plan and merge gates stay with the
person. The person also installs the workspace GitHub App on each new
repository, as in section 6.5.

### 8.4 Invite people

Invite each member by email with a role: `viewer`, `executor`,
`contributor`, `maintainer`, or `operator`. Roles nest, each including the
ones before it; a person who runs tasks needs at least `executor`, and only
an operator confirms documents.

The response to an invitation carries the invitee's sign-in link in
`sign_in_url`, and the link holds a token. The allowlist below prints
`email`, `role`, and `delivery`, never `sign_in_url`:

```sh
umask 077
conveyor --server <server-url> auth token | sed 's/^/Authorization: Bearer /' |
  curl -sS -H @- -H 'Content-Type: application/json' -X POST \
  --data '{"email":"teammate@example.com","role":"contributor"}' \
  -o response.json -w 'HTTP %{http_code}\n' <server-url>/v1/workspaces/<workspace-id>/members
grep -oE '"(email|role|delivery|error|field|message)":"[^"]*"' response.json
rm -f response.json
```

Expect `HTTP 201`. A grant to an existing account reports no `delivery` and
issues no link. For an invitation, `delivery` is `sent` when SMTP emailed
the link, `fallback` when no SMTP is configured, and `failed` when sending
failed. For `fallback` or `failed`, the person copies the invitation link
from the Workspace page in the dashboard and delivers it, or resends the
invitation there. Never read the link yourself, from the response or the
dashboard. Each
invitee then follows section 12 on their own machine. Roles and capabilities
are in [Authentication](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/auth.md#roles-and-capabilities).

## 9. Create the execution setup

A machine that runs tasks needs a local execution configuration. It names the
harness, model, and effort for each stage, the review seats, and where task
worktrees go. Its contents never leave the machine. Ask the person for the
worktree location: `repository` places worktrees under the primary checkout
at `_worktrees`, and `machine` places them under `~/.conveyor/worktrees`.

```sh
mkdir -p "$HOME/.conveyor/client"
conveyor config init-execution --config "$HOME/.conveyor/client/conveyor.yaml" \
  --defaults --worktree-location machine
```

`--defaults` writes the first healthy detected harness without prompting.
To choose harnesses and models per stage, create or change a named setup
with field flags; each harness is probed before the file is written
(AC-5.2):

```sh
conveyor setup create main --config "$HOME/.conveyor/client/conveyor.yaml" \
  --implement-harness claude --implement-effort high \
  --review-harness codex --review-effort high --worktree-location machine
conveyor setup default main --config "$HOME/.conveyor/client/conveyor.yaml"
conveyor setup edit main --config "$HOME/.conveyor/client/conveyor.yaml" --implement-model <model-id>
```

Add a `repos:` entry to the file for each registered repository the machine
works on, with the registered `name`, the `url`, the absolute `checkout`
path of the clone from section 7.3, and `base`. Ask the person whether to
add `export CONVEYOR_CONFIG="$HOME/.conveyor/client/conveyor.yaml"` to their
shell startup file. Harness definitions and stage settings are in
[Configuration](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/configuration.md#local-execution-config-executor-machine);
harness-specific setup is in the
[Cursor](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/cursor-build.md),
[OpenCode](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/opencode-build.md),
and [Grok](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/grok-build.md)
pages.

For local agent sessions, the person runs a task with
`conveyor --server <server-url> --workspace <workspace-id> run <task-id>`,
which probes the setup's harnesses before it claims and asks the person at
each gate.

## 10. Enroll a worker

When tasks run under a background worker, confirm the act first. The
pairing token is a secret, so capture it into a shell variable and pass it
on without displaying it:

```sh
pairing_token=$(conveyor --server <server-url> --workspace <workspace-id> worker pair | sed -n 's/^pairing_token=//p')
conveyor --server <server-url> --workspace <workspace-id> worker run \
  --config "$HOME/.conveyor/client/conveyor.yaml" --pairing-token "$pairing_token" --once
unset pairing_token
conveyor --server <server-url> --workspace <workspace-id> worker install \
  --config "$HOME/.conveyor/client/conveyor.yaml"
```

The first `worker run` enrolls the machine and saves an owner-only worker
credential; later runs reuse it. `worker install` installs a user service
for the workspace. Git credentials and the agent CLI login must work in the
service's environment, which does not inherit shell exports. Pairing,
headless Git credentials, and recovery are in
[Worker operations](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/worker-operations.md).

## 11. Verify the setup

After the setup steps, check each component the setup included and report
each result to the person (AC-3.7):

- **Server version.** `curl -fsS <server-url>/v1/version` reports the
  release, and `conveyord version` reports the installed binary on the host.
- **CLI sign-in.** `conveyor --server <server-url> auth status` names the
  intended server and the person's identity, and
  `conveyor --server <server-url> --workspace <workspace-id> config list`
  names the intended workspace.
- **MCP registrations.** For each tool, ask the person to open a new client
  session and confirm the registered connection lists Conveyor's tools; for
  Claude Code, `claude mcp list` reports each connection's state.
  `conveyor mcp install --list` reports the registration on disk only.
- **Harness probes.** For each execution setup, run every harness's
  `probe_command` from the setup's `harnesses` entries, such as
  `claude --version`. `conveyor run` repeats the probe before it claims.
- **Workers.** `conveyor --server <server-url> --workspace <workspace-id> worker status`
  reports `local_service` as running and `remote_worker` as live, with its
  harness probes.

A healthy server and a sign-in do not prove that agents can deliver. Suggest
a first small task, such as the repository's install task, as the end-to-end
check.

## 12. Extend an existing setup

When a setup already exists, read the current state with section 2 first,
perform only the steps the requested change needs, and leave unrelated
configuration unchanged (AC-4.1). Present a summary of only those steps, as
in section 5.

| Change | Sections |
|---|---|
| Add a team workspace | 8.2, then 8.3 and 8.4 |
| Add a repository | 8.3, then the GitHub App installation in 6.5 |
| Invite a person | 8.4 |
| Connect another agent tool | 7.2 with `--tool` |
| Add or change an execution setup | 9 |
| Add a worker | 10 |
| Upgrade | 1 on every machine, then restart the server and each worker; rerun `conveyor skills install` |

### Join an existing factory from a new machine

A machine that joins hosts nothing: it needs no database, `conveyor init`,
or `conveyord install` (AC-4.2). Ask the person for the server URL, the
workspace ID, the repository names, and their invitation, then:

1. Install the CLI and skills (section 1).
2. Hand the person the invitation link to open, a password to set, and a
   personal access token to mint on **Settings** (section 6.5, items 1
   and 3).
3. Sign in and select the workspace (section 7.1).
4. Register MCP for each chosen tool (section 7.2).
5. Clone the repositories it will work on (section 7.3).
6. Create the execution setup (section 9).
7. Optionally, enroll a worker (section 10).
8. Verify (section 11), skipping the server host checks.

## Reference

- [CLI reference](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/cli.md)
- [Configuration](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/configuration.md)
- [Authentication](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/auth.md)
- [MCP reference](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/mcp.md)
- [Worker operations](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/worker-operations.md)
- [SingleStore operations](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/singlestore.md)
- Harness pages: [Cursor](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/cursor-build.md),
  [OpenCode](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/opencode-build.md),
  [Grok](https://github.com/kidus-tiliksew/conveyor/blob/main/docs/grok-build.md)
