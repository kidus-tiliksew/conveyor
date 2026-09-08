# Client setup

Use this guide on each machine where you operate Conveyor or run agents.
You need an existing factory and an account invitation. Ask the host for the
server URL, workspace ID, registered repository name, and repository URL.
To host a new factory first, follow [Server setup](server-setup.md).

Client setup needs no database, `conveyor init`, or `conveyord install`.

## 1. Install the CLI and agent tooling

If Conveyor is not yet on this machine, install it. The installer needs
`curl`, `tar`, and a SHA-256 tool, and does not need `sudo`:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
conveyor version
```

Add the `PATH` line to your shell startup file. The installer ships both
binaries; client work uses `conveyor` only, and its printed `conveyor init`
suggestion is for server hosts. Pinned versions, supported platforms, and
`CONVEYOR_INSTALL_DIR` are described in
[Server setup, step 1](server-setup.md#1-install-the-binaries).

If you will execute tasks, install Git and your chosen agent CLI (Claude Code,
Codex, Cursor, or your own wrapper), authenticate it with its own login flow,
and verify it can run a prompt with the model you intend to use. Conveyor's
version probe only checks that a CLI launches; it does not prove account or
model access.

## 2. Sign in and select the server and workspace

Open your invitation, set your display name and password, and mint a personal
access token on Settings. The value is shown once. In your client shell:

```sh
export CONVEYOR_ADDR=https://factory.example.com/mcp
conveyor auth login
conveyor config set workspace <workspace-id>
conveyor auth status
```

`auth login` prompts for the token. Replace the example address and workspace
ID with the host's values; for a local solo factory the address is
`http://127.0.0.1:8080/mcp`. The `/mcp` suffix is what MCP clients need in
step 6, and the CLI strips it for its own API calls, so one value serves both.
Add the `CONVEYOR_ADDR` line to your shell startup file. Keep the same
hostname throughout, because credentials are stored per server URL. A one-off
`--server` flag does not save a default server; the workspace default is saved
for the selected server.

Work outside the server directory, in a shell without the server's bootstrap
`CONVEYOR_API_TOKEN` exported. An environment token takes precedence over the
token saved by `auth login`, and the server's `.env` would be loaded into
client commands run from that directory.

## 3. Configure GitHub identity and repository access

On account Settings, save a fine-grained GitHub token with Contents read/write
and Pull requests read/write on the repositories you will execute against.
Conveyor uses the stored token for claim eligibility and pull request writes,
so PRs open as you rather than as a shared bot. Merge approvers need their
own token too.

Git on your machine needs credentials separately. The quickest route is the
GitHub CLI, which configures the HTTPS credential helper:

```sh
gh auth login
gh auth setup-git
```

SSH authentication for the registered repository URL works as well. Headless
options for unattended workers are covered in
[Worker operations](worker-operations.md).

Clone the registered repository and confirm access without a prompt:

```sh
git clone <repository-url> <local-directory>
cd <local-directory>
GIT_TERMINAL_PROMPT=0 git ls-remote --heads origin <default-branch>
```

Expect a commit hash and branch reference. Conveyor performs its own bounded
access check before claiming; restart a run or worker after changing
credentials because that check is cached.

## 4. Prepare the repository

From the cloned checkout, run:

```sh
conveyor repo init
```

The command adds a Conveyor section to `AGENTS.md` and `CLAUDE.md` while
preserving existing text outside its markers. It installs project-scoped
skills for Claude, Codex, and Cursor even when their CLIs are absent. An
unowned skill file or unsafe guidance file causes a named refusal before
installation. Review the generated files and deliver them through a task.
The command creates no commit, branch, or push.

The section and skills are versioned with the CLI. Re-run `conveyor repo init`
after an upgrade to refresh the committed copies, as DEC-40 defines. If origin
cannot be resolved to a registered repository with your credential, the
section uses `<registered-repository>` and `<base-branch>` placeholders.

<a id="4-create-the-local-execution-setup"></a>

## 5. Create the local execution setup

A machine that runs tasks needs a local execution configuration. It answers
the three questions the server never decides for you: which agent CLI runs
each stage, as what model and effort, and who sits on the review panel. Keep
it separate from any server config, including on a solo machine:

```sh
mkdir -p "$HOME/.conveyor/client"
export CONVEYOR_CONFIG="$HOME/.conveyor/client/conveyor.yaml"
conveyor config init-execution
conveyor config list
```

Add the `CONVEYOR_CONFIG` line to your shell startup file. The wizard detects
and probes installed agent CLIs and lets you choose stage models, effort, and
review seats. Select models your agent account can use. `config list` shows
the effective server, workspace, and execution config path.

For a machine with Claude Code installed, the generated file looks like this:

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
the prompt and the generated MCP config, plus a probe command Conveyor runs
to verify the CLI is present before claiming anything. The review seats are
the review panel: one independent review order per seat, in order, so a
second seat with a different model buys a second opinion on every delivery.
The file stays on this machine; the server only learns whether a serviceable
harness is present.

Add the repository entry to this file, preserving the wizard's other settings:

```yaml
repos:
  - name: <registered-repository-name>
    url: <registered-repository-url>
    checkout: /absolute/path/to/local-clone
    base: <default-branch>
```

Use the exact registered name and a clone with the matching remote. Running
from inside that repository also permits discovery, but the explicit checkout
mapping lets a worker find it when launched elsewhere. `worktree_root` controls
where task worktrees are created; it does not provide the primary clone.

Named alternatives are managed with `conveyor setup create <name>`,
`conveyor setup edit <name>`, and `conveyor setup default <name>`, and
selected per run with `conveyor run <task-id> --setup <name>`.

<a id="5-connect-agent-sessions"></a>

## 6. Connect agent sessions

For sessions that plan documents or operate Conveyor through MCP:

```sh
conveyor skills install
conveyor mcp install
export CONVEYOR_API_TOKEN=$(conveyor auth token)
```

The skills command configures detected Claude Code, Codex, Cursor, and OpenCode
clients. OpenCode skills install under `~/.config/opencode/skills`, or under
`.opencode/skills` with `--project`. OpenCode also reads the Claude skills root,
so a machine with both tools receives duplicate copies by design; the copies are
identical and harmless. Use `--tool` to select a client or `--list` to inspect
planned changes. MCP registration
references credentials through the environment rather than writing the token
anywhere, which is why the `CONVEYOR_API_TOKEN` export is needed alongside
the `CONVEYOR_ADDR` you set in step 2. Launch the agent client from a shell
with both variables set; a desktop application launched elsewhere may not
inherit them. Cursor registration is global in `~/.cursor/mcp.json`. The
install command prints any missing bridge instruction. OpenCode registration is
global in `~/.config/opencode/opencode.json`, or
`$XDG_CONFIG_HOME/opencode/opencode.json` when set. Run
`conveyor mcp install --tool opencode` to install its environment-backed
`mcp.conveyor` entry. OpenCode needs `CONVEYOR_ADDR=<server>/mcp` and uses
`{env:VAR}` substitution. The installer keeps its ownership marker inside the
entry because OpenCode rejects unknown top-level keys. It refuses comments
and symlinks, skips unmarked entries unless `--adopt` is passed, and validates
changed config before replacement when OpenCode is on PATH.

<a id="6-run-a-first-task"></a>

## 7. Run a first task

File a small real change and run it:

```sh
conveyor task new --repo <registered-repository-name> -m 'fix the typo in README'
conveyor run <task-id>
```

Titles are generated from the body; there is no title field. Tasks can also
be filed from the dashboard board or from an agent session with the
`create_task` MCP tool.

`conveyor run` presents each stage and chains claimable stages by default.
It surfaces operator gates (plan approval, merge approval) inline so you can
decide without switching to the browser; approve only after reviewing the
proposed work. Pass `--step` to confirm each stage before it is claimed.
Check the task and pull request through completion. A healthy server or a
successful login alone does not verify agent execution or delivery.

If a run reports `workspace_required`, set the workspace for the effective
server. If it cannot resolve a primary checkout, check `repos[].checkout` and
the clone's remote. For authentication failures, distinguish the Conveyor
personal token, the stored account GitHub token, local Git credentials, and
the agent CLI login; they serve different operations.

<a id="7-build-the-document-corpus"></a>

## 8. Build the document corpus

Confirmed documents are what the factory implements and reviews against, so
write them before filing real work. Open an agent session in your project
(the installed skills wrap the [planning playbook](playbooks/conveyor-planning.md))
and draft requirements, System Design documents, and decisions. Every push is
a proposal; confirm each one in the dashboard.

Skipping this step works, in the sense that tasks will run. But an empty
corpus means reviews have nothing to check deliveries against, and the
misalignment machinery has nothing to arm. The factory degrades into a plain
task queue. [The document corpus](document-corpus.md) explains what each
document tier does.

For unattended execution, follow [Worker operations](worker-operations.md)
to pair and install a local worker.

## Upgrade the client

Rerun the installer with the chosen version and verify `conveyor version`.
Restart any running Conveyor worker to load the new binary. Coordinate the
worker version with the server host. If the release updates embedded skills,
rerun `conveyor skills install` and restart agent sessions that use them.

## What gets installed where

| Thing | Path |
|---|---|
| Binaries | `~/.local/bin` (or `CONVEYOR_INSTALL_DIR`) |
| CLI credentials and per-server defaults | `<user-config-dir>/conveyor/credentials.json` |
| Default per-user execution config | `<user-config-dir>/conveyor/conveyor.yaml` |
| Worker enrollment credentials | `<user-config-dir>/conveyor/workers/<workspace>.json` |
| Task worktrees | `~/.conveyor/worktrees` (override with `worktree_root`) |

`<user-config-dir>` is `~/Library/Application Support` on macOS and
`~/.config` on Linux. Credential files are created with mode 0600 in 0700
directories. This guide selects `$HOME/.conveyor/client/conveyor.yaml`
explicitly with `CONVEYOR_CONFIG`, overriding the default per-user path.
