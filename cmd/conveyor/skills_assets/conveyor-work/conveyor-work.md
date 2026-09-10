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

For implementation orders, call `report_progress` at these milestones:

- Immediately after `get_work_order`, summarize the work order and next action
  before checkout, file inspection, or implementation. Continue automatically
  without asking for confirmation or waiting for a response.
- After `conveyor checkout` succeeds, name the worktree path and base commit.
- After completing each numbered contract item or approved-plan step, name the
  completed item or step and the files changed.
- Before `submit_for_review`, list the validation commands run.

Keep each progress message under a few sentences. Usage reporting is
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

## Run one validation session and preserve its evidence

For this repository, `make validate` runs the existing `build`, `vet`,
`fmt-check`, and complete `test` aggregate in one Make graph. It installs web
dependencies once with `npm ci` and builds the dashboard once. This is the
supported form of the existing `make build vet fmt-check test` sharing;
separate Make invocations prepare dependencies again. Under `make -j validate`,
vet and the installer wait for the dashboard build because they compile Go
packages that embed it. Standalone targets retain their own prerequisites.
The target adds no cache/stamp shortcut and never accepts an existing bundle
without rebuilding and checking its diff. `make test-validation` exercises the
orchestration and evidence helper and is also part of `make test`.

The complete ordinary gate still includes Compose isolation, installer checks,
Go tests, dashboard TypeScript compilation, Biome, and Playwright. Explicit
capability-parity typechecking remains in `make test-web`. Run configured
`make test-integration` and `make test-integration-singlestore-ci` separately
when the contract requires them. An unset backend, skipped suite, narrowed
command, failed aggregate, or local reuse never satisfies a mandatory fresh
boundary. Each database run uses disposable isolated fixtures.

`scripts/validation_evidence.py` records a fresh command with `run`, checks an
existing record with `check`, and associates eligible evidence with the actual
pushed branch using `bind`. It never skips a Make prerequisite or changes a
required gate. Run it from the dedicated worktree. Use a new durable directory
for each execution, under `$XDG_STATE_HOME/conveyor/<task-id>/` (default
`$HOME/.local/state/conveyor/<task-id>/`), outside both checkouts and all task
caches. Keep the manifest, private integrity key, log, and head-binding file
together after disposable caches are removed. The key detects accidental
corruption; it is not a signature against an author who can rewrite the bundle.

Before `run`, write a JSON policy describing the actual command and every input
boundary. Schema 1 requires these fields; unknown or omitted fields fail closed:

- `schema`: `1`; `task`: the current task ID; `layer`: `local`, `postgres`, or
  `singlestore`; `command`: the complete argv, starting with `make`.
- `environment`: an explicit list of variable names. The child receives only
  listed variables that are set. Include `PATH` and `HOME`, applicable task-cache
  variables, flags, backend URLs, and every relevant configuration variable.
  An absent variable differs from an empty one. Values are stored only as
  keyed fingerprints; never put credentials in argv or policy prose. The log
  replaces inventoried environment values before retention. Inspect logs for
  any application-specific secret encoding before sharing them.
- `tools`: a map from executable name to its version-probe argv, for example
  `"go": ["go", "version"]`. Include all invoked tools and nested runtimes,
  including `make`, `git`, `python3`, and `sh`. A shell probe may use
  `["sh", "-c", "printf POSIX-shell"]`; the resolved executable is also hashed.
  Probes must be read-only, deterministic, and receive the same environment.
- `external_inputs`: absolute file/directory paths covering inputs outside the
  worktree, including SDK/standard-library files, runtime libraries, loaded
  configuration, resolved dependency trees, and browsers where consumed.
  Inventory the configured HOME files or use a clean dedicated HOME. A version
  string alone does not inventory a runtime or its dependencies.
- `exclude`: an object giving a reason for each excluded output directory.
  Only `bin`, `web/playwright-report`, and `web/test-results` can be excluded,
  and never when tracked or read by the command. All other tracked, untracked,
  ignored, generated, dependency, and fixture files in the worktree are hashed,
  including modes and symlink targets. Do not exclude `node_modules`; installed
  dependencies must be inventoried. Files outside the root reached by symlinks
  require an explicit external input.
- `audit`: `inputs_complete` must be a boolean; `input_rationale` explains the
  reviewed inventory and output exclusions. Set it to `false` for unknown
  inputs: `run` still records fresh execution, but `check`/`bind` refuse reuse. `git_metadata` is `dependent` by
  default in author judgment, or `independent` only after inspecting every
  command and transitive input; `git_rationale` records that inspection.
  Unknown inputs or external state require another run, not an optimistic
  audit declaration. Reviewers assess this policy against the actual code.
- `backend`: `null` for local runs. Database records require an object with
  `isolation: "disposable-per-run"` and `probe`: a read-only argv returning JSON
  with nonempty `identity`, `version`, `configuration`, and unique `instance`
  fields from the configured backend. Probe results are fingerprinted before
  and after execution. This helper records backend evidence but refuses its
  reuse: matching configuration cannot prove unchanged mutable database state.
  Preserve fresh backend results and rerun the isolated target when needed.

For example, after authoring and auditing `policy.json` outside the worktree:

```sh
python3 scripts/validation_evidence.py run --policy "$policy" --output "$evidence"
python3 scripts/validation_evidence.py check --policy "$policy" --output "$evidence"
# After committing and pushing the assigned task branch:
python3 scripts/validation_evidence.py bind --policy "$policy" --output "$evidence" \
  --remote origin --branch "conveyor/task-<task-id>"
```

`run` always executes the command, preserves its full exit status, timestamps,
redacted log and digest, and captures input fingerprints before and after.
A successful command whose inputs changed is still recorded as fresh execution,
but cannot be reused. Dependency installation that changes resolved inputs can
therefore make a session ineligible. Do not edit an old manifest to claim that
it tested the final state; rerun the affected validation into a new directory.

`check` refuses missing/corrupt manifests, keys or logs; prior failure;
before/after drift; changed commands, flags, tools, dependencies, files,
environment, configuration, host, worktree, or task; and uncertain Git history.
A refusal exits nonzero: run the affected required target again. No automatic
skip toggle or cross-task/cross-environment cache exists.

`bind` additionally requires a clean worktree/index and queries the remote to
prove the assigned branch was pushed at the current head. It writes a separate
`reused-execution` binding, preserving the original execution time and reason.
A commit can preserve file content while changing Git inputs. `make validate`
and `make build` consume `git describe --tags --always --dirty` through
`VERSION`/`LDFLAGS`; treat their evidence as metadata-dependent. Git/history tests
may also consume repository state. Only an audited metadata-independent command
can carry evidence across ordinary linear commits of identical inputs. Even an
identical-content merge, authored conflict resolution, rebase, reset, missing
reflog, or Git change during execution requires fresh validation. An ordinary
`make fmt-check` is a candidate only after auditing the current Makefile and
resolved gofmt runtime; target names alone do not establish independence.

Local manifests and logs are task-context evidence, not artifacts carrying the
`verification_evidence` role, whose permitted image/recording types remain
unchanged. Record fresh exact-head CI independently. Local equivalence never
makes a prior reviewed-head approval current or replaces independent reviewer
judgment, mandatory done criteria, or either operator gate (REQ-4/AC-4.1 and
REQ-7/AC-7.1; `component-verification-strategy`, DEC-29).

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
- An implementation order ends after validation, commit, and
  `conveyor submit <task-id>` from its dedicated task worktree. The command
  pushes the exact head, opens or reuses the pull request with the executing
  machine's credential, and calls `submit_for_review` with `head_sha`. Keep
  `CONVEYOR_WORK_ORDER_ID` and `CONVEYOR_SESSION_ID` from the claimed session.
  Direct MCP submission remains available when the pull request is already
  open: supply its pushed `head_sha`, work order, session, and workspace.
  The server validates head and base, records the PR, and dispatches review.
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
