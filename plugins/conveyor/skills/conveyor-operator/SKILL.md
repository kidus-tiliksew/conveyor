---
name: conveyor-operator
description: Operate Conveyor through its MCP task-intake and work-order tools. Use when Codex must create or triage a Conveyor task, claim and implement an approved work order with safe agent-owned Git branch setup, submit work for review, await feedback, or perform an independent Conveyor review.
---

# Conveyor Operator

Use Conveyor's MCP tools to move work through its normal durable pipeline. Do
not replace triage, specification, implementation, or review with an ad hoc
parallel workflow.

## Preconditions

- Use the configured Conveyor MCP server at `http://127.0.0.1:8080/mcp`.
- Read authentication from `CONVEYOR_API_TOKEN` at runtime. Never print, paste,
  commit, or store the token in source files or transcripts.
- If the server is unavailable, report the connection problem and expected
  endpoint. Never simulate successful Conveyor actions.

## Create and triage a task

1. Require explicit user intent before calling `create_task`; it creates
   durable state.
2. Supply the task title, configured repository name, and an idempotency key.
   Prefer a stable source key such as `github:owner/repo#123`; otherwise use a
   caller-scoped UUID. Reuse a key only for an exact retry.
3. Include the issue body and source URL when available. Include a base branch
   only when required. Default to L2 unless the user or issue warrants another
   supported level.
4. Report the returned task ID. Creation enqueues Conveyor's existing triage
   and specification pipeline; do not run a second triage path in Codex.
5. If no task-status tool is available, direct the user to Conveyor's dashboard
   or worker logs instead of inventing polling.

## Implement a work order

1. Call `list_work_orders` and select an implementation order matching the
   user's scope.
2. Generate a fresh session ID and client token. Treat the client token as a
   secret and never expose it in chat, logs, source, or transcripts.
3. Call `claim_work_order` with a bounded lease, then call `get_work_order` for
   the approved specification, configured repository, assigned base, exact
   task branch, deadline, feedback, artifacts, and acceptance criteria.
4. Immediately establish the assigned branch using the safe procedure below,
   before making implementation changes.
5. Work only in the configured repository and exact assigned branch. Implement
   the approved specification and run the repository's required validation.
6. Use `report_progress` for meaningful milestones and `report_usage` with
   cumulative, truthful usage. Respect the lease and stage deadline. Upload a
   transcript only when required and only after confirming it contains no
   secrets.
7. Commit the completed work, push the assigned branch with upstream tracking,
   and verify the remote push succeeded.
8. Call `submit_for_review` only after the push and when the user's instruction
   authorizes the review handoff. Use `await_review` when keeping the
   implementation session available for feedback.

### Safe assigned-branch setup

Conveyor assigns branch metadata; it does not create a local or remote Git ref.
The implementing agent owns this checkout and must preserve existing work:

1. Inspect `git status --short --branch`, the current branch, and Git operation
   markers. Treat any dirty worktree, detached or otherwise unsafe checkout,
   unexplained merge/rebase/cherry-pick/revert, or unrelated work as a blocker.
   Do not clean, stash, discard, reset, or rewrite it automatically.
2. Fetch the assigned base from `origin` and verify the freshly fetched
   `origin/<base>` exists. Treat an unavailable or unsafe base as a blocker.
3. Inspect `refs/heads/<task-branch>` locally and query
   `refs/heads/<task-branch>` on `origin` with `git ls-remote`. If the remote
   branch exists, fetch that exact ref into
   `refs/remotes/origin/<task-branch>` before comparing histories.
4. If the local task branch exists, switch to it and preserve its commits. If
   the remote also exists, allow only an ancestry-safe relationship: fast-
   forward the local branch when the remote is ahead, preserve the local branch
   when it is ahead, and block when the histories diverge.
5. Otherwise, if the remote task branch exists, create and switch to a local
   tracking branch for it.
6. Otherwise, create the exact assigned task branch from the freshly fetched
   `origin/<base>`.
7. Never use `git switch -C`, `git checkout -B`, forced ref updates, force
   pushes, or equivalent branch recreation. Never rebase, reset, delete, or
   overwrite an existing task branch to make it match the base.
8. On a review bounce or redispatch, continue on the existing task branch. Do
   not recreate it from the base or discard prior task commits.
9. Before `submit_for_review`, run `git push --set-upstream origin
   <task-branch>` and verify the pushed ref. The pushed branch is Conveyor's
   review trust boundary.

Report ambiguous ownership, unsafe ancestry, divergence, or unrelated dirty
work through `report_progress` and stop instead of rewriting history.

## Review a work order

1. Review in a fresh Codex session with a fresh client token. The implementation
   session must not review its own task.
2. Select and claim the matching review order, then call `get_work_order` for
   its approved specification, acceptance criteria, pushed-branch diff,
   feedback, and artifacts.
3. Compare the implementation with the specification, Non-goals, diff, and
   validation evidence. Submit `approve` or `changes_requested` with a precise
   reason code, concise summary, and actionable feedback.
4. Do not merge. Conveyor hands off the review verdict; CI and the final human
   merge gate remain outside this skill.

## Boundaries

- Do not create synthetic tasks in a real workspace to test the tool.
- Do not fabricate progress, usage, validation, branches, commits, pushes, or
  review evidence.
- Do not bypass the specification gate or implement unapproved work.
- Do not make Conveyor create or mutate the implementation agent's branch or
  checkout, and do not reintroduce a sandbox execution path.
