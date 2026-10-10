# Operator UX proposal: review package

Status: a proposal for developer review before implementation.

It is not confirmed corpus.

Nothing has been filed in Conveyor.

Code citations were verified against origin/main 1c70303e.

## What's here

Read in this order: this README, then [proposal.md](proposal.md), then [mockup/index.html](mockup/index.html), then [analysis.md](analysis.md) as evidence, then [skills-review.md](skills-review.md).

| File | Purpose |
|---|---|
| [README.md](README.md) | Entry point for the reviewer. |
| [proposal.md](proposal.md) | The change set, operator decisions (§8), and the must-ship bug fix (§10). |
| [mockup/index.html](mockup/index.html) | Static three-tab mockup of inbox, flowchart, and board. |
| [analysis.md](analysis.md) | Evidence: current clocks, stops, and code citations. |
| [skills-review.md](skills-review.md) | Skills and harness-neutrality review. |
| [screenshots/](screenshots/) | PNG captures of the mockup tabs. |

## Operator decisions

From proposal.md headings, proposal.md §8, and skills-review.md §7.

- Auto-recovery: default of 2 automatic recoveries per order per 24h with exponential backoff; a workspace can override it (A1).
- Silence: a session counts as gone after 1 hour with no check-in; before that, a missed check-in only shows "may be stuck" (A10).
- Board: the "Needs operator" column becomes a count in the header that links to the inbox; there is no column (C1).
- Kit pin drift: an older version of the same document stays eligible; the selection receipt and the review show a drift warning, and the exact-version rule is relaxed (A8).
- Token approvals: record provenance only (channel web/cli/mcp, credential ID, agent identity) on every approval event; no step-up.
- Plan approval and merge are soft gates that wait for a human; the channel does not matter.
- An agent may record an approval only when the operator states it explicitly in that conversation; both UI and that recording count the same.
- Coordinators decide only mechanical steps; DEC-45 stands.
- No machine-wide harness or model config; each harness self-identifies (harness, model, user) on every claim (A11).
- `conveyor run` and `conveyor worker` remain optional adapters that skills do not mention.
- MCP stays a required capability where CLI parity is missing.
- `conveyor-work` carries no harness launch steps; those become non-normative notes at most.
- `conveyor-coordinate` is a developer's uncommitted skill; it should arrive rewritten to these decisions.
- `repo init` writes skills to one neutral, git-ignored folder and prints install instructions; copying into tool folders is opt-in.
- The Codex `conveyor-operator` plugin shrinks to a pointer to the canonical skills.

## Mockup

Open [mockup/index.html](mockup/index.html) in a browser.

It has three tabs: 1 Operator inbox, 2 Task flowchart (scenario chips), 3 Board (flowchart icon per card; the card body opens a "What this task is doing" panel).

It is a static mockup and buttons don't change data.

Screenshots:

- [screenshots/inbox.png](screenshots/inbox.png)
- [screenshots/board.png](screenshots/board.png)
- [screenshots/board-task-panel.png](screenshots/board-task-panel.png)
- [screenshots/flow-auto-retry.png](screenshots/flow-auto-retry.png)
- [screenshots/flow-plan-gate.png](screenshots/flow-plan-gate.png)
- [screenshots/flow-dependency.png](screenshots/flow-dependency.png)

## First batch (must ship)

- Recover/restart bug (proposal §10): operator recover rewrites the task's frozen setup contract from workspace defaults, which can leave a verify order queued but unclaimable; restart then fails with a bare 500. The section includes the fix and its regression tests, and lists the server log and DB rows that would confirm the cause on the live system.
- A2 (timed_out visible) and C3 projection fixes, which proposal §7 says can go first.

Visual quality (typography, wrapping) is listed as lower priority than C1–C3 and is a separate track.

A1, A7, and A8 need corpus decisions before implementation.

## What we want from the reviewer

- Feasibility of the next-action projection, auto-recovery bound, inbox, flowchart, and CLI/MCP surface.
- Corpus or DEC conflicts, especially DEC-10, DEC-17, DEC-18, DEC-45, DEC-60, and the kit exact-version rule.
- Sequencing: whether A2+C3 first, then B1/B2, then C1/C2/CLI, matches how you would ship this.
