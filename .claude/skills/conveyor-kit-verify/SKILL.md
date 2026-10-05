---
name: conveyor-kit-verify
description: Execute selected kits and ordinary obligations under a live verify claim, collect typed evidence, reconcile operations and submit the verification result.
---

# Verify a submitted revision

Read and follow [docs/playbooks/conveyor-kit-verify.md](../../../docs/playbooks/conveyor-kit-verify.md).
It is the canonical playbook for this skill. Keep tool observations, agent
assertions and authenticated operator observations distinct. No result grants
acceptance or operator approval (req-verification-kits REQ-8/AC-8.4).
A verifier's claim names the harness in `agent` and the runtime's concrete
model ID in `model`, or the harness's reported value such as `auto` verbatim
when the harness does not expose one; never guess a model ID.
