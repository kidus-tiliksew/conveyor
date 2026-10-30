---
name: conveyor-install
description: Install and configure Conveyor with the person — interview them about their situation (join or host, server hosting, database, team topology, repositories, agent tools, execution), present a setup summary they accept, then perform the setup through the release installer, `conveyor`, `conveyord`, and the REST API, and verify it. Use when the person asks to set up, install, configure, extend, or join a Conveyor factory, or to add a machine, workspace, repository, person, execution setup, or worker.
---

# Conveyor setup

Read and follow [docs/playbooks/conveyor-install.md](../../../docs/playbooks/conveyor-install.md)
— it is the canonical, tool-neutral setup playbook (interview, supported
topologies, per-step procedure, verification, and extending a setup).

Non-negotiables, restated: interview and get the setup summary accepted
before the first change. Confirm each act that creates, changes, or removes
deployment state before running it. Never read, display, store, or transmit
a secret value: hand the person the command or page that takes it, or
generate a new secret straight into an owner-only file without displaying
it. Hand browser acts to the person with the link and the action. Stop at a
failed step and report its command and error. Leave repository preparation
to the factory's install task and never write Conveyor guidance or skills
onto a base branch. No operator-only acts (gate decisions, document
confirmation, drift resolution).
