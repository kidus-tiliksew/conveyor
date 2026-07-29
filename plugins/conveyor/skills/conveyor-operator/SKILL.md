---
name: conveyor-operator
description: Operate Conveyor through its MCP task-intake and work-order tools. Use when Codex must create or triage a Conveyor task, claim and implement an approved work order in a dedicated task worktree, submit work for review, await feedback, or perform an independent Conveyor review.
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
   only when required. Omit mode to use the workspace default; request Auto or
   Manual explicitly only when the caller requires it.
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
4. Immediately after `get_work_order` returns, announce a concise,
   plain-language summary of what the work order is about and what you will do
   next. Make this announcement before resolving the checkout, inspecting
   files, or starting implementation. It is informational: continue
   automatically without asking for confirmation, waiting for a user response,
   or pausing.
5. Resolve every relevant artifact reference with `read_artifact`, passing the
   same workspace, work order, and session plus the referenced artifact ID.
   Decode the returned base64 content according to its MIME type. Treat a read
   failure as missing required context; never continue from filename metadata.
6. Resolve the dedicated checkout with `conveyor checkout
   <task-id>`. Use its returned path as the working directory. If the current
   checkout is already a clean dedicated clone/worktree for the assigned
   branch, the helper may return it. If the CLI is unavailable, use the safe
   fallback below; never implement in a shared primary checkout.
7. Work only in the configured repository, returned task worktree, and exact
   assigned branch. Implement the approved specification and run the
   repository's required validation there.
8. Use `report_progress` for meaningful milestones and `report_usage` with
   cumulative, truthful usage. Respect the lease and fixed execution deadline;
   reclaiming an expired lease does not extend execution time. If a never-
   claimed order is reported `stale`, use `redispatch_work_order` for the
   supported audited recovery path and then claim it with fresh credentials.
   Never redispatch an active or execution-timed-out order. Upload a
   transcript only when required and only after confirming it contains no
   secrets.
9. Commit the completed work in the dedicated worktree, push the assigned
   branch with upstream tracking, and verify the remote push succeeded.
10. Call `submit_for_review` only after the push and when the user's instruction
   authorizes the review handoff. Use `await_review` when keeping the
   implementation session available for feedback.
11. If `await_review` returns `changes_requested`, keep the same Codex session,
   list and claim the newly queued implementation order before editing. Return
   to the original worktree path, add commits to its existing branch, push,
   resubmit, and reuse the existing PR. Never edit under the submitted order or
   claim the subsequent review order from the implementation session.

### Safe task-worktree setup

`conveyor checkout <task-id>` is the shared safe implementation point. It must
fetch the assigned base, preserve branch history, return the resolved path, and
fail closed on dirty or ambiguous state. Do not substitute manual Git commands
while the CLI is available.

When the CLI is unavailable, apply this equivalent fallback without switching
the primary checkout:

1. Before fetching or inspecting refs/worktrees, compare the current
   checkout's normalized `origin` with the assigned configured repository URL
   (`CONVEYOR_TASK_REPO_URL` in a worker session). Equivalent GitHub HTTPS/SSH
   forms are acceptable; missing, ambiguous, or mismatched identity is a
   blocker even with an explicit path. Then inspect the repository root,
   primary-checkout cleanliness and current branch, `git worktree list
   --porcelain`, and merge/rebase/cherry-pick/revert markers. Dirty unrelated
   work, detached or ambiguous state, or an in-progress Git operation is a
   blocker. Never clean, stash, discard, or repair it automatically.
2. If a clean registered worktree already owns the assigned branch, reuse that
   exact path. Otherwise use the deterministic contained sibling
   `../conveyor-worktrees/<repo>-task-<task-id>` unless an explicit path was
   supplied. An existing unregistered directory, symlink escape, or conflicting
   worktree is a blocker.
3. Fetch the assigned base from `origin` and verify `origin/<base>`. Inspect the
   local task ref and query/fetch the exact remote task ref. If both exist,
   allow only an ancestry-safe relationship; fast-forward normally when remote
   is ahead, preserve local commits when local is ahead, and stop on divergence.
4. If the local task branch exists and is not checked out elsewhere, run
   `git worktree add <path> <branch>`. If only the remote branch exists, run
   `git worktree add --track -b <branch> <path> origin/<branch>`. If neither
   exists, run `git worktree add -b <branch> <path> origin/<base>`.
5. Never use `git worktree add -B`, `git switch -C`, `git checkout -B`, reset,
   rebase, forced ref updates, force pushes, automatic stash, branch deletion,
   or equivalent history recreation. Report blockers through `report_progress`
   and stop instead of rewriting history.
6. Persist the resolved path for the task. Reuse it across review bounces and
   redirects; do not recreate the branch from base or discard prior commits.

Before `submit_for_review`, run `git push --set-upstream origin <task-branch>`
from the dedicated worktree and verify the pushed ref. The pushed branch is
Conveyor's review trust boundary.

### Cleanup

- Keep the worktree through every implementation, review-bounce, and human
  redirect round.
- Only after the task merges or closes, run `conveyor done <task-id>` from the
  primary checkout.
- Cleanup must refuse a dirty worktree, must not remove the primary checkout,
  and must never delete an unmerged task branch. Repeating cleanup after the
  worktree or registration is gone is safe and should report a skipped action.

## Review a work order

1. Review in a fresh Codex session with a fresh client token. The implementation
   session must not review its own task.
2. Select and claim the matching review order, then call `get_work_order` for
   its approved specification, acceptance criteria, pushed-branch diff,
   feedback, and artifacts.
3. Resolve relevant artifacts with `read_artifact` under the claimed review
   workspace, work order, and session; a failed read is a review blocker rather
   than permission to review from metadata alone.
4. Review the pushed PR diff. If surrounding code is required, use a separate
   read-only or detached checkout; never share or mutate the implementation
   worktree.
5. Compare the implementation with the specification, Non-goals, diff, and
   validation evidence. Call `submit_review_verdict` with `approve` or
   `changes_requested`, a precise reason code, concise summary, and actionable
   feedback, then wait for and observe a successful tool response. Printing,
   returning, or describing verdict JSON is not completion; a missing or failed
   tool response is not terminal success.
6. Do not merge. Conveyor hands off the review verdict; CI and the final human
   merge gate remain outside this skill.

## Boundaries

- Do not create synthetic tasks in a real workspace to test the tool.
- Do not fabricate progress, usage, validation, branches, commits, pushes, or
  review evidence.
- Do not bypass the specification gate or implement unapproved work.
- Do not make Conveyor create or mutate the implementation agent's branch or
  checkout, and do not reintroduce a sandbox execution path.
- Do not implement Phase 8 multi-repository worktree sets through this local
  single-repository helper.

## Phase 5.1 workers

- `conveyor worker pair` creates a short-lived, single-use enrollment token.
  Never paste that token or the exchanged worker credential into source,
  transcripts, or chat.
- `conveyor worker run` may claim only persisted Auto-mode work. Manual work
  continues through the implementation/review workflow above unchanged.
- Worker dispatch is labeled `dispatch: worker`, `confinement: none`, and
  `auth: byoa`. Harness routing is enforced for worker claims and advisory for
  manually attached agents.
