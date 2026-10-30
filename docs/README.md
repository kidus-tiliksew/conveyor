# Conveyor documentation

## Getting started

Install and configure Conveyor with the
[setup playbook](playbooks/conveyor-install.md). Give a coding agent its raw
URL,
`https://raw.githubusercontent.com/kidus-tiliksew/conveyor/main/docs/playbooks/conveyor-install.md`,
and the agent interviews you, agrees a setup with you, and carries it out. The
pages below are reference: they describe commands, settings, and behavior, and
hold no setup procedure (DEC-92).

## Guides

- [CLI reference](cli.md): every `conveyor` and `conveyord` command
- [Authentication](auth.md): sign-in, tokens, roles, GitHub identity
- [Configuration](configuration.md): the three config surfaces and every environment variable
- [MCP reference](mcp.md): the tools an agent uses to work a task

## The factory

- [Concepts](concepts.md): the software factory, the knowledge graph, light and dark factory patterns
- [The document corpus](document-corpus.md): requirements, System Designs, decisions, and the propose-confirm cycle
- [Tasks](tasks.md): how work is created, given context, executed, reviewed, and linked
- [Misalignment](misalignment.md): drift, staleness, and pending proposals

## Operations

- [Worker operations](worker-operations.md): durable worker enrollment, service install, recovery
- [GitHub lifecycle](github-lifecycle.md): how issues, PRs, and review statuses are projected onto GitHub
- [SingleStore operations](singlestore.md): running the factory on SingleStore instead of PostgreSQL
- [The log core](log-core.md): the event log and the durable queue that runs on it
- [Known limitations](known-limitations.md): accepted boundaries of the current implementation

## Agent harnesses

- [Cursor CLI](cursor-build.md): worker setup with environment MCP transport
- [Grok Build](grok-build.md): worker setup with environment MCP transport
- [OpenCode](opencode-build.md): worker setup with environment MCP transport

## Playbooks

Agent-facing playbooks, installed as project-scoped skills for Claude Code,
Codex, Cursor, and OpenCode by `conveyor repo init`, or user-globally by
`conveyor skills install`:

- [Setup](playbooks/conveyor-install.md): install and configure Conveyor with a person, from one instruction
- [Planning](playbooks/conveyor-planning.md): draft and push documents from a local agent session
- [Task filing](playbooks/conveyor-task-filing.md): file well-formed tasks and dependency chains
- [Working a task](playbooks/conveyor-work.md): the claim, checkout, submit, review lifecycle
- [Testing strategy](playbooks/conveyor-testing-doc.md): write the testing-strategy System Design document that reviewers judge verification against
- [Authoring verification kits](playbooks/conveyor-kit.md): declare repository kits in `.conveyor/kits/manifest.yaml`
- [Executing verification kits](playbooks/conveyor-kit-verify.md): run kits and ordinary checks under a verify claim and submit typed evidence
- [Coordinating tasks](playbooks/conveyor-coordinate.md): keep several tasks moving with one thread per stage order, routine decisions within delegated scope, and plain-language timeline text

Operator procedures:

- [Checkpoint recovery](playbooks/checkpoint-recovery.md): recover preserved implementation work

## Contributing to this repository

- [Local validation evidence](validation-evidence.md): scratch space, `make validate`, retained evidence, and resource recovery
