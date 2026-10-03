---
name: conveyor-plan
description: Draft and push Conveyor planning documents from a local session — requirements (v2 REQ→AC), System Design documents, DEC-n decision records, and product-overview uploads — over the factory's REST API with the propose→confirm discipline intact. Use when the operator wants to plan features, write or revise any document tier, promote an overview claim into a requirement, or record a decision, without using the in-product planning chat.
---

# Conveyor local planning

Read and follow [docs/playbooks/conveyor-planning.md](../../../docs/playbooks/conveyor-planning.md)
— it is the canonical, tool-neutral playbook (fence formats, endpoints,
ID discipline, promotion).

Non-negotiables, restated: draft first — build the draft folder, run
`conveyor plan check`, and get every item approved on the
`conveyor plan review` page before any push, and never confirm.
Every push is a **proposal** — the operator confirms each layer in the
web UI, never you.
No operator-only acts (gate approvals, drift resolution).
No fabricated lineage or origins.
The confirmed factory document corpus — requirements with REQ-n/AC-n.m
statements, System Design documents, and DEC-n decisions — is the
authority; do not direct new work to amend or cite a repository-resident
specification.
