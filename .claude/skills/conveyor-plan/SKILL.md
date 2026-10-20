---
name: conveyor-plan
description: Draft and push Conveyor planning documents from a local session — requirements (v2 REQ→AC), System Design documents, DEC-n decision records, task items, and product-overview uploads — through a local draft that `conveyor plan` checks, approves item by item, and pushes as proposals, with the propose→confirm discipline intact. Use when the operator wants to plan features, write or revise any document tier, promote an overview claim into a requirement, or record a decision, without using the in-product planning chat.
---

# Conveyor local planning

Read and follow [docs/playbooks/conveyor-planning.md](../../../docs/playbooks/conveyor-planning.md)
— it is the canonical, tool-neutral playbook (draft flow, fence formats,
ID discipline, promotion).

Non-negotiables, restated: every draft item reaches the factory only through
`conveyor plan push`, never a proposal or task-creation endpoint. Record a
draft approval only from the operator's own approval, at the digests the
operator was shown. Every push is a **proposal** — the operator confirms, in
the UI or on their explicit word in this conversation, never you. No
operator-only acts (gate approvals, drift resolution). No fabricated lineage
or origins. The confirmed factory document corpus — requirements with
REQ-n/AC-n.m statements, System Design documents, and DEC-n decisions — is
the authority; do not direct new work to amend or cite a repository-resident
specification.
