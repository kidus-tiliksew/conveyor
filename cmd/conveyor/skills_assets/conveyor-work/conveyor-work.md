# Working a Conveyor task

Work on a Conveyor task only while holding the live claim for its current work
order. The work-order contract delivered by Conveyor is authoritative for the
stage: this playbook describes the client loop without replacing that
contract.

Never edit, test, commit, or push for a task without its claim. If a claim is
declined, fails, expires, or is lost, stop. Never bypass the factory by working
the task branch bare.

## Enter the claimed loop

1. Call `list_work_orders` in the task's workspace and select the claimable
   pending order whose `task_id` matches the requested task. Do not infer the
   current stage from a branch or an old order.
2. Create a fresh session ID and secret client token, then call
   `claim_work_order` for that exact order. Keep the client token out of chat,
   logs, transcripts, source, and commits. A failed or declined claim is a stop
   condition.
3. Immediately call `get_work_order` with the claimed order and session. Follow
   its approved plan, acceptance criteria, assigned repository, base, exact
   task branch, deadlines, feedback, role prompt, and artifact references.
4. Resolve every relevant artifact reference with `read_artifact` under the
   same workspace, order, and session. Decode its returned base64 content by
   MIME type. Missing required artifact content is a blocker; filename metadata
   is not a substitute.
5. For an implementation or review order, run `conveyor checkout <task-id>`
   and use the returned dedicated worktree. Perform every repository read
   needed for implementation and every edit, test, commit, and push there.
   Preserve the assigned branch history and obey the delivered contract's
   validation and delivery instructions. A spec order instead executes
   read-only in the checkout where its session was launched: never run
   `conveyor checkout` for a spec order and never alter that checkout's Git
   state.

Use `report_progress` at meaningful milestones. Usage reporting is
observational and best-effort; it does not replace lifecycle completion.

## Keep scratch data outside checkouts

At the start of every claimed loop, create one task-specific scratch root with
the shape `$XDG_CACHE_HOME/conveyor/<task-id>` (defaulting to
`$HOME/.cache/conveyor/<task-id>`). It must be outside both the shared primary
checkout and the dedicated task worktree, and it must live on a disk-backed
filesystem rather than `tmpfs`, `ramfs`, or another RAM-backed temporary
mount. Resolve the candidate and both checkout paths to canonical absolute
paths before use; stop and report the problem if the candidate is inside a
checkout or its backing filesystem cannot be established as disk-backed.

On Linux, `findmnt -T "$CONVEYOR_TASK_CACHE" -o TARGET,SOURCE,FSTYPE,OPTIONS`
provides the required mount check. Use the platform's equivalent mount or
filesystem inspection on other hosts. Do not silently fall back to `/tmp`.

Create separate children and export every cache variable before any build or
test command. Preserve the operator's `XDG_CACHE_HOME`; derive the task root
from it instead of replacing it:

```sh
task_id='<task-id>'
cache_base="${XDG_CACHE_HOME:-$HOME/.cache}/conveyor"
export CONVEYOR_TASK_CACHE="$cache_base/$task_id"
mkdir -p "$CONVEYOR_TASK_CACHE"/{go-build,go-tmp,tmp,playwright,npm}

export GOCACHE="$CONVEYOR_TASK_CACHE/go-build"
export GOTMPDIR="$CONVEYOR_TASK_CACHE/go-tmp"
export TMPDIR="$CONVEYOR_TASK_CACHE/tmp"
export PLAYWRIGHT_BROWSERS_PATH="$CONVEYOR_TASK_CACHE/playwright"
export npm_config_cache="$CONVEYOR_TASK_CACHE/npm"
```

`PLAYWRIGHT_BROWSERS_PATH` controls the browser download used by this
repository's `npx playwright install` command, while `npm_config_cache` routes
npm/npx package cache data. Generated logs, reports, archives, review clones,
and other disposable artifacts belong under `CONVEYOR_TASK_CACHE` too, unless
the work-order contract requires a tracked repository output.

Register cleanup for normal exit, command failure, and catchable interruption,
and also remove the directory explicitly when the claim concludes. Before a
recursive removal, canonicalize and verify that the target is exactly the
current task's child of the selected `conveyor` cache base; never remove the
base or another task's directory. A process killed without a catchable signal
cannot run cleanup, so at the next claim entry inspect and remove only a stale
directory for the same task after confirming no live process uses it.

If a confined sandbox denies the sanctioned external path, first request
write permission scoped only to that exact task cache directory. Only when
that permission is unavailable may the session use a last-resort directory at
the task worktree root named `.codex-cache-<task-id>/` (or the corresponding
`.claude-<purpose>/` or `.grok-<purpose>/` harness form). Never put the fallback
in the shared primary checkout. Before generating anything, require both
`git check-ignore -q <fallback-path>` and an empty
`git status --porcelain --untracked-files=normal`; repeat the status check
after creating a probe file. If Git can see the fallback, remove it and stop
rather than dirtying either checkout. Apply the same guarded cleanup rules to
this fallback when the claim ends.

## Keep the lease alive

The claim lease is short-lived and renewable. The repository default is five
minutes (`internal/core.DefaultWorkOrderClaimLease`), and the existing run and
worker loop attempts renewal every ten seconds, reducing that interval to one
third of the remaining lease when necessary. The live `lease_expires_at` and
execution deadline returned by claim, renewal, and `get_work_order` are the
authority for this session; renewal never extends the fixed execution deadline.

Start renewing at claim time, including during setup, long tests, and review
waiting. Call `renew_work_order` at the live response's advertised or implied
safe cadence; when it provides no tighter cadence, follow the repository loop's
ten-second cadence and renew sooner when one third of the remaining lease is
shorter. Update the local expiry from every successful response.

If renewal fails or the server no longer reports the order claimed by this
session, stop repository work immediately. Return to `list_work_orders` and
reclaim only a claimable current order with fresh credentials, then fetch its
contract again. Do not keep working during a stale interval and do not treat a
reclaim as an extension of the original execution deadline.

## Finish through the factory

End every claimed stage with its registered lifecycle tool or, when genuinely
abandoning the attempt, `release_work_order` with a truthful reason:

- A plan-stage order ends with `submit_plan`. The current MCP registration
  intentionally rejects the retired `submit_spec` name and directs callers to
  `submit_plan`; use the tool and schema delivered by the live server.
- An implementation order ends only after validation, commit, and a successful
  upstream push of the exact assigned task branch, followed by
  `submit_for_review`. Do not open the pull request or submit review yourself.
- An independently claimed review order ends with `submit_review_verdict`.
  Implementation and review must use separate sessions; an implementer never
  claims or judges its own review order.

After the stage's submission tool succeeds, report the result and exit the
session. Never poll `await_review` from a stage session. Verdict handling,
bounces, and successor orders belong to the launcher (`conveyor run` or the
worker), and a changes-requested bounce always arrives as a new order in a
fresh session. The same report-and-exit rule applies after an explicit truthful
release.

`release_work_order` is an explicit abandonment or checkpoint handoff, not a
way to declare success. Do not simply exit while leaving a claim to expire.

## Review bounces

Approval completes the review handoff; it does not authorize the executor to
merge. A `changes_requested` result creates a successor implementation order
that the launcher schedules in a fresh session. That successor must call
`get_work_order` before changing anything, then reuse the existing dedicated
worktree and task branch, add and push corrective commits, and submit the new
order. Never amend or re-submit through an already submitted order, and never
apply feedback outside a live successor claim.

## Authority boundary

An executor's claim confers only the stage-scoped capabilities registered for
that order. Implementation sessions may create allowed governance proposals,
but proposals confer no authority and do not pause delivery. Gate approval,
requirement or design confirmation, decision confirmation, hold or assignment
changes, drift resolution, review judgment by the implementer, and merge are
operator or independent-review acts. Never perform, simulate, or report those
acts as completed.

This loop implements `req-260811-0ee057` v16 REQ-12/AC-12.4 and the shared
distribution rules in AC-12.1 and AC-12.3, preserves the executor proposal
boundary in AC-1.5, and parallels the REQ-5 run path. The work-order mechanism
remains governed by `design-260805-973cd4`; this playbook changes no lifecycle
semantics.
