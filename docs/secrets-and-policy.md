# Phase 1 secrets and tool policy

Phase 1 resolves secret references only on the trusted LocalDockerRunner
host. Plaintext values never enter task records, API payloads, Docker argv,
or the worktree. The runner writes a mode-0600 Docker env file, creates the
container, and immediately removes the staging directory.

## Configure SOPS

Install `sops` and configure one of its supported key providers. Age is a
practical local default:

```sh
brew install sops age
mkdir -p ~/.config/conveyor
age-keygen -o ~/.config/conveyor/age-key.txt
age-keygen -y ~/.config/conveyor/age-key.txt
export SOPS_AGE_KEY_FILE="$HOME/.config/conveyor/age-key.txt"
```

Put the printed public recipient in a SOPS creation rule:

```yaml
# ~/.conveyor/secrets/.sops.yaml
creation_rules:
  - path_regex: .*\.sops\.env$
    age: age1...
```

Point Conveyor at that file and declare delivery policy explicitly:

```yaml
secrets:
  root: ~/.conveyor/secrets
  backend: sops
  sops_config: ~/.conveyor/secrets/.sops.yaml
  sets:
    integration-tests:
      local_eligible: true
```

Set a value without putting it in argv or shell history:

```sh
printf '%s' "$DATABASE_URL" |
  bin/conveyor --config conveyor.yaml \
  secrets set acme/integration-tests/DATABASE_URL --from-stdin
```

The encrypted file is
`~/.conveyor/secrets/acme/integration-tests.sops.env`. `backend: plain`
uses the same layout without encryption and is intended only for disposable
fixtures.

Reference the environment variables needed by each repository:

```yaml
repos:
  - name: api
    secret_refs:
      - secretref://acme/integration-tests/DATABASE_URL
```

LocalDockerRunner fails closed when a referenced set is absent, is not
`local_eligible`, cannot be decrypted, contains an invalid environment name,
or contains a multiline value.

Note: harness credential directories such as `~/.claude` and `~/.codex` are
copied into per-job staging before being mounted read-only into the sandbox
(spec §5.2). The original live home directory is never mounted into the
sandbox; under Tier A confinement, only the job worktree set and bare-repo
cache are mounted (spec §8.5).
The per-job staged credential copy is deleted once artifact collection
completes.

## Configure command policy

Commands are argv prefixes, not shell snippets:

```yaml
repos:
  - name: api
    tool_policy:
      allowed_commands:
        - [git]
        - [go, test]
      denied_commands:
        - [printenv]
        - [env]
        - [rm, -rf]
```

The shim passes this policy to the adapter. The Codex adapter creates a
job-scoped `CODEX_HOME/rules/conveyor.rules`, maps allowed prefixes to
`allow`, denied prefixes to `forbidden`, and forces
`approval_policy="never"` for unattended execution. Codex applies the most
restrictive matching rule, so `forbidden` wins. The Tier A container remains
the filesystem confinement boundary; execpolicy is defense in depth.

`allowed_commands` contains explicit native allow decisions; it is not a
deny-by-default whitelist. Unmatched commands remain governed by the Codex
approval/sandbox posture and the outer container. `denied_commands` contains
the hard prefix blocks. A complete runner-independent command-policy shim
remains the Phase 4 deliverable in spec §19.

The adapter also enables Codex's `shell_environment_policy` retention for
names containing `KEY`, `SECRET`, or `TOKEN`. Codex normally filters those
names from tool shells; Conveyor can retain them because the runner has already
scoped the container environment to the explicitly referenced secret set.

Codex command rules are experimental and apply to command prefixes. They do
not implement domain-level egress filtering. The runner-level egress TODO is
therefore still explicit rather than being represented as enforced policy.
Configuration rejects non-empty `network_allow` values until that boundary is
implemented, and unknown YAML fields are errors so misspelled deny rules cannot
silently disappear.

References: [Codex rules](https://developers.openai.com/codex/rules),
[Codex configuration](https://developers.openai.com/codex/config-reference),
and [SOPS programmatic usage](https://getsops.io/docs/usage/advanced/).

## Human checkout and redispatch

`conveyor checkout <task-id>` fetches the pushed task branch and creates a
human worktree beside the current repository (or at `--path`). After committing
human edits, `conveyor done <task-id> --redispatch` verifies the worktree is
clean, pushes the branch, removes the human worktree, and invokes the
authenticated redispatch endpoint. The runner fast-forwards its isolated task
checkout to that human push before starting the successor job. Divergence is a
hard error in both the runner and human checkout paths; Conveyor never chooses
a side or resets either branch silently. Reopening a human checkout fetches and
fast-forwards a stale local task ref when the remote is strictly ahead.
