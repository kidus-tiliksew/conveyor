# Getting started: solo

Solo mode is one person running the whole factory on one machine: the server,
the dashboard, the agents, and the operator judgment are all yours. This page
is the whole path as one command list. Each step links to the fuller guide
when you need the detail; [Server setup](server-setup.md) covers the first
half and [Client setup](client-setup.md) the second.

You need Docker (or a PostgreSQL 15+ you already run), Git, the GitHub CLI,
an API key for an OpenAI-compatible model endpoint, and an authenticated
agent CLI such as Claude Code.

The server and the client keep separate directories even on one machine:
`~/.conveyor/server` holds the daemon's config and secrets,
`~/.conveyor/client` holds your execution setup. Treat that as a given and
the rest is copy and paste.

## 1. Install and start a database

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
docker run -d --name conveyor-postgres --restart unless-stopped \
  -e POSTGRES_USER=conveyor -e POSTGRES_PASSWORD=conveyor \
  -e POSTGRES_DB=conveyor -p 127.0.0.1:5432:5432 \
  -v conveyor-postgres-data:/var/lib/postgresql/data postgres:16-alpine
```

Add the `PATH` line to your shell startup file.
Detail: [Server setup, steps 1 and 2](server-setup.md#1-install-the-binaries).

## 2. Create the server environment and initialize

```sh
mkdir -p "$HOME/.conveyor/server" && cd "$HOME/.conveyor/server"
umask 077
cat > .env <<EOF
CONVEYOR_DATABASE_URL=postgres://conveyor:conveyor@127.0.0.1:5432/conveyor?sslmode=disable
CONVEYOR_API_TOKEN=$(openssl rand -hex 32)
CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY=$(openssl rand -base64 32)
CONVEYOR_LLM_API_KEY=<provider API key>
CONVEYOR_PUBLIC_URL=http://127.0.0.1:8080
EOF
conveyor init
```

Edit `.env` to fill in the API key before running `conveyor init`. The
wizard asks for your organization, name, email, a workspace ID, and your
repository's name, URL, and default branch. Note the workspace ID and
repository name for step 4, and keep the printed sign-in link.

A workspace accepts no task until a repository is registered. The dashboard
shows a notice on the Board and New task sheet until one exists. Use the
Workspace page's General tab to register a repository.

Detail: [Server setup, steps 3 and 4](server-setup.md#3-create-the-server-environment-file).

## 3. Start the server and sign in

```sh
conveyord install --config ./conveyor.yaml
curl -fsS http://127.0.0.1:8080/healthz
```

Open the sign-in link, set a password, then on Settings mint a personal
access token for Conveyor. In Workspace settings, connect the GitHub App and
install it on your repository. Your machine supplies the Git credential used
to push and open pull requests.
Detail: [Server setup, steps 5 and 6](server-setup.md#5-start-and-check-the-server).

## 4. Connect the CLI

Open a new shell outside the server directory:

```sh
export CONVEYOR_ADDR=http://127.0.0.1:8080/mcp
conveyor auth login
conveyor config set workspace <workspace-id>
gh auth login
gh auth setup-git
git clone <repository-url> ~/src/<repo>
```

Add the `CONVEYOR_ADDR` line to your shell startup file. `auth login`
prompts for the personal access token.
Detail: [Client setup, steps 2 and 3](client-setup.md#2-sign-in-and-select-the-server-and-workspace).

After cloning, run `conveyor repo init` in the checkout as described in [Prepare the repository](client-setup.md#4-prepare-the-repository).

## 5. Create the execution setup

```sh
mkdir -p "$HOME/.conveyor/client"
export CONVEYOR_CONFIG="$HOME/.conveyor/client/conveyor.yaml"
conveyor config init-execution
```

Add the `CONVEYOR_CONFIG` line to your shell startup file, then add your
repository to the generated file:

```yaml
repos:
  - name: <registered-repository-name>
    url: <repository-url>
    checkout: /absolute/path/to/src/<repo>
    base: main
```

Detail: [Client setup, step 5](client-setup.md#5-create-the-local-execution-setup).

## 6. Connect agent sessions and run a task

```sh
conveyor skills install
conveyor mcp install
export CONVEYOR_API_TOKEN=$(conveyor auth token)
conveyor task new --repo <registered-repository-name> -m 'fix the typo in README'
conveyor run <task-id>
```

`conveyor run` walks the stages and asks you at each operator gate.
Detail: [Client setup, steps 6 and 7](client-setup.md#6-connect-agent-sessions).

## Where to go next

- [Client setup, step 8](client-setup.md#8-build-the-document-corpus): write
  the requirements and designs the factory reviews against before filing
  real work.
- [Worker operations](worker-operations.md): run a background worker instead
  of attaching to each task.
- [Tasks](tasks.md) for what happens between `queued` and `merged`, and
  [Concepts](concepts.md) for the shape of the whole factory.
