# Conveyor documentation

Conveyor is a software factory for agent-written code. Operators confirm
Requirements, System Design documents, and decisions; agents on your machines
plan, implement, and review tasks triggered from those documents; every code merge
links back to the intent that justified it. 

These docs cover installing the
factory, running it alone or as a team, and the concepts that make it work.

## Getting started

- [Installation](installation.md): release installer, source builds, prerequisites
- [Getting started (solo)](getting-started-solo.md): one person, one machine, end to end
- [Getting started (multiplayer)](getting-started-multiplayer.md): a shared team server

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
- [Known limitations](known-limitations.md): accepted boundaries of the current implementation

## Playbooks

Agent-facing playbooks, installable as skills with `conveyor skills install`:

- [Planning](playbooks/conveyor-planning.md): draft and push documents from a local agent session
- [Task filing](playbooks/conveyor-task-filing.md): file well-formed tasks and dependency chains
- [Working a task](playbooks/conveyor-work.md): the claim, checkout, submit, review lifecycle
