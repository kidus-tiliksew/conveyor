# Getting started: solo

Solo mode is one person running the whole factory on one machine: the server,
the dashboard, the agents, and the operator judgment are all yours. This guide
stands that up end to end. If you are joining or hosting a shared server, read
[Getting started: multiplayer](getting-started-multiplayer.md) instead; the
first half is the same.

## 1. Install

Follow [Installation](installation.md). You need both binaries on your PATH,
a running PostgreSQL 15 or newer, and the environment values in the next step.
Task execution later requires Git on the worker machine, but initialization
does not require Git, a forge CLI, or a local repository clone.

## 2. Export the environment

The server reads three required variables, plus the key that encrypts stored
GitHub tokens. Generate the operator token and the encryption key:

```sh
openssl rand -hex 32
```

```sh
openssl rand -base64 32
```

```sh
export CONVEYOR_DATABASE_URL='postgres://conveyor:conveyor@localhost:5432/conveyor?sslmode=disable'
export CONVEYOR_API_TOKEN='<hex token>'
export CONVEYOR_LLM_API_KEY='<provider API key>'
export CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY='<base64 key>'
```

Without the encryption key, the server cannot store the GitHub token that
task execution requires (step 5). Keep it stable; rotating it invalidates
stored tokens.

Both binaries also load a `.env` file from the working directory, and process
environment values win over file values. Putting these three lines in a `.env`
next to your config is the usual arrangement.

## 3. Initialize the factory

```sh
conveyor init
```

`conveyor init` prompts for the organization name, your name and email, a
workspace id, and the repository Conveyor will work on (name, URL, default
branch). It registers that metadata without inspecting the deployment host's
filesystem or forge tools, writes `conveyor.yaml`, creates the organization and
workspace in the database, and binds `CONVEYOR_API_TOKEN` as your operator
token.

It ends by printing two things: the command to install the server as a user
service, and a sign-in link for your new operator account. Keep the link; you
use it in step 5.

## 4. Start the server

Either install it as a user service (a launchd agent on macOS, a systemd user
unit on Linux):

```sh
conveyord install --config ./conveyor.yaml
```

or run it in the foreground while you experiment:

```sh
conveyord -config ./conveyor.yaml
```

Startup applies database migrations and serves the dashboard at
`http://127.0.0.1:8080`. `conveyord status` reports the service state and log
paths.

Before starting, open `conveyor.yaml` and adjust the `harnesses:` entries for
the agent CLIs actually installed on this machine. The annotated
[conveyor.example.yaml](../conveyor.example.yaml) explains every field.

## 5. Sign in and mint your tokens

Open the sign-in link that `conveyor init` printed. It lands on an onboarding
page where you set your display name and a password (at least 12 characters).
From then on the dashboard uses an ordinary session; sign in at `/sign-in`
with email and password. If you ever lose the link before onboarding, issue a
fresh one on the host:

```sh
conveyor user issue-link you@example.com
```

Two tokens live on the Settings page:

- A personal access token, for the CLI and MCP clients. Mint one now; the
  value is shown once.
- A GitHub token, required before you can execute tasks. Create a
  fine-grained GitHub token with Contents read and write and Pull requests
  read and write on your repository, and save it here. Conveyor opens PRs and
  merges as you, not as a shared bot.

## 6. Connect the CLI, skills, and MCP clients

Authenticate the CLI with the personal access token from Settings, then
install Conveyor's agent skills and MCP registrations:

```sh
conveyor --server http://127.0.0.1:8080 auth login
conveyor skills install
conveyor mcp install
```

The install commands configure detected Claude Code and Codex clients; pass
`--tool` to pick one or `--list` to see what would change. MCP registration
references the token through the `CONVEYOR_API_TOKEN` environment variable
rather than writing the value anywhere. If that variable is not set in your
shell, the command prints the line to add:

```sh
export CONVEYOR_API_TOKEN=$(conveyor auth token)
```

## 7. Create an execution setup

A machine that runs tasks needs a local execution configuration. It answers
the three questions the server never decides for you: which agent CLI runs
each stage, as what model and effort, and who sits on the review panel. The
`conveyor.yaml` written by `conveyor init` already qualifies on the factory
host. To generate one elsewhere, or to redo it:

```sh
conveyor config init-execution
```

The wizard detects installed agent CLIs, probes them, and writes the file.
For a machine with Claude Code installed, the result looks like this:

```yaml
execution_settings:
  spec:
    harness: claude
    model: claude-opus-5
    effort: high
    timeout: 30m
  implementation:
    harness: claude
    effort: high
    timeout: 4h
  review:
    timeout: 1h

harnesses:
  - name: claude
    mcp_transport: json_file
    command:
      [claude, -p, "{prompt}", --mcp-config, "{mcp_config}",
       --allowedTools, "mcp__conveyor__*", --output-format, stream-json,
       --verbose, --permission-mode, bypassPermissions, --add-dir, ..]
    model_args: [--model, "{model}"]
    effort_args:
      high: [--effort, high]
    probe_command: [claude, --version]

review:
  seats:
    - {model: claude-opus-5, harness: claude}
```

Reading it top to bottom: each stage names the harness that runs it, the
model, and a wall-clock timeout. The harness entry is the launch recipe: the
exact argv (executed directly, never through a shell) with placeholders for
the prompt and the generated MCP config, and a probe command Conveyor runs to
verify the CLI is actually present before claiming anything. The review seats
are the review panel: one independent review order per seat, in order, so
adding a second seat with a different model buys a second opinion on every
delivery.

The file stays on this machine; the server only learns whether a serviceable
harness is present. Built-in templates exist for Claude Code, Codex, Grok, and
Cursor, so you rarely write a harness entry by hand. Named variants of the
setup are managed with `conveyor setup` and selected per run with
`conveyor run --setup <name>`; see the [CLI reference](cli.md#setup).

## 8. Build the document corpus

Confirmed documents are what the factory implements and reviews against, so
write them before filing work. Open an agent session in your project (the
installed skills wrap the [planning playbook](playbooks/conveyor-planning.md))
and draft requirements, System Design documents, and decisions. Every push is
a proposal; confirm each one in the dashboard.

Skipping this step works, in the sense that tasks will run. But an empty
corpus means reviews have nothing to check deliveries against, and the
misalignment machinery has nothing to arm. The factory degrades into a plain
task queue. [The document corpus](document-corpus.md) explains what each
document tier does.

## 9. File and run a task

File a task from the CLI:

```sh
conveyor task new --repo api -m 'fix the typo in README'
```

or from an agent session with the `create_task` MCP tool, or from the
dashboard board. Titles are generated from the body; there is no title field
to fill in.

Then run it:

```sh
conveyor run <task-id>
```

`conveyor run` shows each claimable stage before it claims, and surfaces
operator gates (plan approval, merge approval) inline so you can decide
without switching to the browser. Pass `--auto` to chain claimable stages
without the per-stage prompts; gates still apply.

When you would rather have a machine poll the queue and run work without you
attached, enroll a durable worker. That flow, including running the worker as
a user service, is covered in [Worker operations](worker-operations.md).

## Where to go next

- [Tasks](tasks.md) for what actually happens between `queued` and `merged`
- [Concepts](concepts.md) for the shape of the whole factory, including how
  far you can push it toward hands-off operation
