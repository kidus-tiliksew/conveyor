# Server setup

Follow this guide once per factory, on the machine that will host `conveyord`.
It creates the database-backed organization and first workspace. People joining
an existing factory follow [Client setup](client-setup.md) instead. For one
person on one machine, the [solo quick start](getting-started-solo.md) runs
both guides in order.

## 1. Install the binaries

The release installer needs `curl`, `tar`, and a SHA-256 tool (`sha256sum` or
`shasum`), and does not need `sudo`. It installs the latest `conveyor` and
`conveyord` into `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
conveyor version
conveyord version
```

Add the same `PATH` line to your shell startup file for future sessions.
Server bootstrap uses `conveyor`; the service runs `conveyord`.

To install a reviewed version, replace `v1.2.3` with an existing release tag
and pin both the installer and the release:

```sh
curl -fsSL https://raw.githubusercontent.com/kidus-tiliksew/conveyor/v1.2.3/install.sh | sh -s -- v1.2.3
```

The installer supports Linux and macOS on amd64 and arm64. It downloads the
selected GitHub release, verifies the archive against the published
`checksums.txt` before replacing either binary, and rolls both back if
anything fails. Set `CONVEYOR_INSTALL_DIR` to choose another destination.
The installer prints `Next: conveyor init`; do the database and environment
steps below first.

## 2. Start a database

Use a running PostgreSQL 15 or newer, or follow [SingleStore operations](singlestore.md).
For a new local factory with Docker installed, this creates a persistent
PostgreSQL database reachable only through the host's loopback interface:

```sh
docker run -d --name conveyor-postgres --restart unless-stopped \
  -e POSTGRES_USER=conveyor -e POSTGRES_PASSWORD=conveyor \
  -e POSTGRES_DB=conveyor -p 127.0.0.1:5432:5432 \
  -v conveyor-postgres-data:/var/lib/postgresql/data postgres:16-alpine
docker exec conveyor-postgres pg_isready -U conveyor -d conveyor
```

Wait for `accepting connections` before proceeding. These example credentials
are for local development. For a hosted database, use its supplied credentials
and TLS connection URL. If port 5432 is occupied, choose a different host port
and use that port in the URL below. Keep the volume when restarting the database.

## 3. Create the server environment file

Use a dedicated directory outside any project checkout. The heredoc generates
the two secrets for you; only the model provider API key needs filling in:

```sh
mkdir -p "$HOME/.conveyor/server"
cd "$HOME/.conveyor/server"
umask 077
cat > .env <<EOF
CONVEYOR_DATABASE_URL=postgres://conveyor:conveyor@127.0.0.1:5432/conveyor?sslmode=disable
CONVEYOR_API_TOKEN=$(openssl rand -hex 32)
CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY=$(openssl rand -base64 32)
CONVEYOR_LLM_API_KEY=<provider API key>
CONVEYOR_PUBLIC_URL=http://127.0.0.1:8080
EOF
```

Then edit `.env` and replace `<provider API key>`. Use `KEY=value` lines
without shell `export` prefixes; `.env` is loaded by both binaries from their
working directory, and existing process environment values take precedence.

- Keep `CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY` stable. Changing it invalidates
  the stored GitHub App private keys.
- `CONVEYOR_API_TOKEN` is the server bootstrap token. It is separate from the
  personal tokens users mint later and from agent CLI logins on executor
  machines.
- The in-process model client defaults to `https://api.openai.com/v1`. For
  another provider, set `CONVEYOR_LLM_BASE_URL` to its API base URL and choose
  provider-supported model IDs in the generated configuration. The endpoint
  must support the Responses API features Conveyor uses, including tools and
  structured output; a chat-completions-only endpoint is insufficient.
- For a team deployment, set `CONVEYOR_PUBLIC_URL` to the external HTTPS URL
  before initialization. Arrange DNS, TLS, and a reverse proxy to the daemon's
  loopback listener at `127.0.0.1:8080` yourself; the public URL does not
  create that proxy or change the listener. SMTP settings and invitations are
  covered in [Getting started: multiplayer](getting-started-multiplayer.md).

Service installation requires a readable owner-only environment file; shell
exports alone do not satisfy that requirement. The default is `.env` beside
the server config; `--env-file /absolute/path` selects another file for
`conveyord install`.

## 4. Initialize the factory

From the server directory:

```sh
conveyor init
```

Enter the organization, your display name and email, workspace ID and name,
and repository name, URL, and default branch. Record the workspace ID and
repository name; clients need them. Initialization registers repository
metadata and does not clone the repository. It writes `conveyor.yaml`,
initializes the database, and prints your sign-in link.

A workspace accepts no task until a repository is registered. The dashboard
shows a notice on the Board and New task sheet until one exists. Register
repositories on the Workspace page under General.

The dashboard's Install Conveyor switch is on by default. It files one install
task per repository to add agent instructions and Conveyor's skills through a
pull request. Run and merge that task like any other task; it is also your first
end-to-end delivery. `conveyor repo init` is the local equivalent. Run it again
after a CLI upgrade to refresh the repository guidance and skills. See
[Prepare the repository](client-setup.md#4-prepare-the-repository).

Review the generated control-plane triage and planning models before starting.
Use model IDs available through your chosen endpoint; see
[Configuration](configuration.md) and the [annotated example](../conveyor.example.yaml).
The generated agent execution defaults do not establish that an executor has
the required CLI, account access, or models. Executors configure that
themselves in [Client setup](client-setup.md).

## 5. Start and check the server

```sh
conveyord install --config ./conveyor.yaml
conveyord status
curl -fsS http://127.0.0.1:8080/healthz
```

Installation creates a launchd user agent on macOS or a systemd user service
on Linux. `status` reports service state and log paths. For a foreground run,
use `conveyord -config ./conveyor.yaml` instead of installing the service.
On Linux, configure user lingering separately if the service must survive
logout; macOS user agents start at login.

Check that `/healthz` succeeds and the dashboard opens at your public URL.
These checks establish server availability; the first client task also
exercises model access and execution.

## 6. Sign in and invite clients

Open the printed sign-in link, set your display name and a password of at
least 12 characters, then use `/sign-in` for subsequent logins. Links expire
after 30 minutes and are single use. From the server directory, replace a
lost or expired link with:

```sh
conveyor user issue-link you@example.com
```

Select your workspace, open **Settings**, and find the **Workspace GitHub App**
card. Enter an organization name if the app belongs to an organization, then
choose **Connect GitHub**. Conveyor sends a manifest to
GitHub, where you create the app and install it on the account that owns your
repositories. Select every registered workspace repository and check its
coverage on the card. Use **Install on GitHub** beside a repository marked
**Not covered** to include it. The app requests metadata read access and
contents, pull requests, issues, and commit statuses write access.

The server encrypts the app private key with
`CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY`. It mints installation tokens on demand
and keeps them only in process memory until five minutes before expiry. Set
the public server URL before connecting so GitHub can return your browser to
Conveyor. Use **Disconnect** on the card and confirm to remove the stored app; manage
or revoke its GitHub installation on GitHub.

Settings no longer asks for a personal or workspace GitHub token. Invite
other users through [Getting started: multiplayer](getting-started-multiplayer.md).

Give each person the public server URL, workspace ID, registered repository
name, repository URL, and invitation link. Then continue with
[Client setup](client-setup.md), including when the server and client run on
the same machine.

## Upgrade the server

Rerun the installer above, verify `conveyord version`, then restart the server
service or foreground process. Startup applies pending embedded database
migrations. The binary refuses to start against a database that a newer release
already migrated, so upgrade the binary before the database gets ahead of it.

## Alternative deployments

The release installer is the normal path. Two others exist: the container
image for hosted deployments, and a source build for developing Conveyor.

### Run the container image

Release tags are also published as
`ghcr.io/kidus-tiliksew/conveyor:<version>`. Stable releases additionally
publish `ghcr.io/kidus-tiliksew/conveyor:latest`; prereleases do not move that
tag. The image contains both Conveyor binaries plus the `git` and `gh` runtime
tools, but it contains no credentials or configuration.

Provide a `conveyor.yaml`, persist the default cache directory, and pass a
container-reachable listen address:

```sh
docker run --rm \
  -p 8080:8080 \
  -v "$PWD/conveyor.yaml:/etc/conveyor/conveyor.yaml:ro" \
  -v conveyor-cache:/home/conveyor/.conveyor/cache \
  -e CONVEYOR_API_TOKEN \
  -e CONVEYOR_DATABASE_URL \
  -e CONVEYOR_LLM_API_KEY \
  -e CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY \
  ghcr.io/kidus-tiliksew/conveyor:v1.2.3 \
  -config /etc/conveyor/conveyor.yaml -addr 0.0.0.0:8080
```

`CONVEYOR_API_TOKEN`, `CONVEYOR_DATABASE_URL`, `CONVEYOR_LLM_API_KEY`, and
`CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY` are required process environment. The
GitHub monitor uses the workspace app installation. Secret values should come from
your container platform's secret facility; do not add them to the image or
`conveyor.yaml`. The selected PostgreSQL or SingleStore database must be
reachable from the container. Set `database.backend` to `postgres` or
`singlestore`; [configuration](configuration.md) lists their URL forms.

Once the process starts, `/healthz` is available on the published port. Image
upgrades use the same protocol as binary upgrades: replace the image and
restart it. `conveyord` applies embedded database migrations during startup;
there is no separate container migration command.

The default shutdown budget is 25 seconds. On Kubernetes, set
`terminationGracePeriodSeconds` to at least the selected budget plus five
seconds (30 seconds for the default). Cloud Run has a fixed ten-second
termination window, so start `conveyord` with `-shutdown-timeout` or
`CONVEYOR_SHUTDOWN_TIMEOUT` set below ten seconds, such as `8s`. An explicit
flag takes precedence over the environment value.

### Build from source

This path is for developing Conveyor and running its development server.
Source development also needs Go 1.24, Node 22 with npm, and Docker with
Compose (for the development Postgres).

```sh
git clone https://github.com/kidus-tiliksew/conveyor
cd conveyor
cp conveyor.example.yaml conveyor.yaml
cp .env.example .env
chmod 600 .env
```

Before starting, set `CONVEYOR_LLM_API_KEY` in `.env`, replace the operator
token with `openssl rand -hex 32` output, and enable
`CONVEYOR_FORGE_TOKEN_ENCRYPTION_KEY` with `openssl rand -base64 32` output.
Review the provider endpoint, model IDs, and repository in the example config.

```sh
make dev
```

`make dev` starts a health-checked Postgres on port 5432, builds the project,
and starts `conveyord` on port 8080.

`make build` alone writes `bin/conveyor` and `bin/conveyord` without starting
anything. The full local test aggregate is `make test`; integration tests
against PostgreSQL run with `make test-integration`. SingleStore tests use
`make test-integration-singlestore-ci` with `CONVEYOR_TEST_SINGLESTORE_URL`
pointing to a disposable database whose name ends in `_test`.
