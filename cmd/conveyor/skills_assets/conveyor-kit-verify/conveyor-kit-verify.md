# Executing verification kits and ordinary checks

Use this playbook under a live `verify` claim. Follow the delivered contract,
approved plan and pinned authority. Implementation ends at submission; review
assesses sealed evidence. A successful kit supplies evidence and never grants
acceptance or operator approval.

Authority: `req-verification-kits` v1 REQ-8/AC-8.1 and REQ-8/AC-8.4;
`feature-verification-kit-execution` v5 VK-2 through VK-9, including VK-4.1,
VK-5.1 and VK-7.1; DEC-29, DEC-40 and DEC-43. The `conveyor-kit` skill covers
the full authoring contract. This playbook describes the shipped CLI and MCP
protocol, including ordinary checks when no kit is selected. The operator grant
flow and the grant wait follow `req-verification-kits` v2 REQ-3/AC-3.2,
REQ-7/AC-7.3 and REQ-8/AC-8.2 and `feature-verification-kit-execution` v9
VK-12.

## Establish the exact context

1. Fetch `get_work_order` with the exact workspace, order and session. Resolve
   relevant artifacts and keep the lease renewed. Only a task whose frozen
   `verify_stage` policy is enabled enters this stage. Do not add verification
   obligations to tasks whose policy keeps the implement-to-review route.
2. Use the dedicated task worktree on its attached task branch. Verify clean
   Git status, submitted HEAD and repository identity. Do not detach HEAD,
   edit or commit implementation files, move the branch or write output into
   checkout inputs.
3. Call `prepare_verification` with an idempotent `request_key`, or inspect an
   existing context with `get_verification_context`. Read exact revisions,
   pins, discovery, selected/excluded kits, reasons, obligations, grants,
   operations and attempts. The server reads the manifest and kit tree from
   the exact forge revision. Never substitute local discovery for that receipt.
4. Call `get_evidence_schemas` for current payload and provenance schemas.
   Register ordinary obligations and build coverage before execution. Obtain
   operator-issued grants through the grant wait below and inspect the local
   execution configuration.

The runner needs launcher-provided `CONVEYOR_WORK_ORDER_ID`,
`CONVEYOR_SESSION_ID`, `CONVEYOR_CLIENT_TOKEN` and explicit workspace context.
Keep the token out of argv, logs and children. Use the configured server; a
connection failure does not authorize guessing another endpoint.

## Claim identity and usage

A verifier that claims its own order, including one a self-claimed
implementation session starts, names itself in `claim_work_order`
(req-agent-skills AC-3.2):

- `agent` always names the harness running the session: `claude-code`,
  `codex`, `opencode`, `cursor`, or the harness's own name for itself.
- `model` is the concrete model ID when the session knows it from its own
  runtime, for example `claude-opus-5-5`.
- When the harness selects the model and does not expose it, as Cursor Auto
  does, `model` carries the harness's reported value verbatim, for example
  `auto`. The session never guesses a model ID and never derives one from a
  configured tier, a dashboard logo, or another session's claim.

When the harness exposes token counts, call `report_usage` with the cumulative
`tokens_in` and `tokens_out` for the verify order at natural checkpoints and
immediately before `submit_verification` or `release_work_order`. Each report
replaces the order's previous figures. Omit `cost_usd`: the server ignores it,
and an unknown cost is never recorded as zero (req-usage-telemetry AC-2.1). A
session without figures skips the call and invents none. Missing usage never
delays or blocks a verification submission (DEC-1).

## Manifest and selection checks

The schema-1 manifest lives at `.conveyor/kits/manifest.yaml`. A kit declares
`id`, `name`, `version`, `path`, `governing_pins`, `exercises` and optional `ui`.
An exercise declares `id`, `stages: [verify]`, `kind`, `argv`, `cwd`, positive
`timeout_seconds`, prerequisites, permissions, typed inputs,
`required_assertions`, `retry_policy`, `operations`, evidence outputs and
`supports`. Safe replay also needs `safety_basis`; reconciliation policy needs
a bounded reconciliation entrypoint for every operation. Both assertion and
operation lists are explicit, even when empty.

All declared requirement/design pins must match the authoritative context by
kind, ID and immutable version; extra context pins are allowed. No pins or a
mismatch excludes a valid kit. Invalid manifests or unavailable kit content
block complete success. `no_manifest` and `no_selected_kits` describe discovery,
not acceptance or a waiver of ordinary checks. The runner checks the local
kit digest against the server receipt before launching it (VK-2, VK-3).

## Register ordinary obligations and coverage

Call `register_verification_obligation` with `context_id`, a stable
`obligation_id`, `description`, `sources` and `contract`. Each source contains
`document_id`, `version`, and `section_id`. For a plan citation, use
`document_id: approved_plan`, its approved version, and an actual section or
criterion text from that plan. Governing citations must resolve in the pinned
documents. Free-text claims do not create authority.

The contract uses exercise fields, including positive timeout, permissions,
inputs, required assertion IDs, operations and replay policy. Ordinary output
types must be unique and have positive `minimum_items`. A non-executable check
uses `kind: observation`, an `observation_procedure` on the registration
request, and declared evidence outputs. The shipped CLI waits for those
outputs; it cannot run an argv-free `interactive` contract. Use an executable
interactive contract when the CLI must launch an interaction tool. Without a
demonstrated replay basis, ordinary checks default to `operator_action_required`.
Registration returns the immutable obligation identity and contract digest.

Write the coverage JSON outside the checkout. It contains:

| Field | Content |
| --- | --- |
| `obligation_ids` | Explicit list of all registered ordinary obligation IDs, including `[]` if none apply. |
| `justification` | Explanation of the scope and why the selected checks cover it. |
| `sources` | Source mappings with `source: {document_id, version, section_id}`, `disposition`, `explanation` and `subjects`. |

A `covered` mapping has one or more exact subjects copied from the server
context. A `not_applicable` mapping has `subjects: []` and an explanation.
Every selected kit subject and registered ordinary subject must be covered;
every governing pin needs a source mapping. Map the approved plan's done
criteria and verification instructions too. Ordinary subjects contain
`kind: ordinary`, `obligation_id` and `contract_digest`. Kit subjects contain
the selected identity and digest; do not reconstruct them from display labels.

The first attempt start records coverage. Later registration can extend it,
but completion cannot drop an existing source or check and must submit the
recorded coverage. If no checks apply, supply an explicit empty-set assessment
with source dispositions and scope justification. Manifest absence alone is
insufficient. The reviewer judges the adequacy of this interpretation under
DEC-29; mechanical coverage validation does not infer all governing prose.

## Operator grants and the claim-bound window

An operator grant binds the context's workspace, task, order, claim attempt,
revision set and subject contract digest. The server admits a grant only while
this verify claim is live at the submitted head. Grants become possible after
`prepare_verification` and, for an ordinary subject, after
`register_verification_obligation`. Renewal keeps the window open without
changing the attempt; the execution deadline stays fixed. Release, lease
lapse, deadline expiry, a changed head and a successor claim close the window.
A successor claim prepares a new context, and earlier grants never authorize
it. Preparation, queued orders and manifests confer no permission.

When a selected subject lacks a matching unrevoked grant, wait for it instead
of releasing at once:

1. Call `report_progress` naming the work-order ID, context ID, each subject
   (`kit:<kit-id>/<exercise-id>` or `ordinary:<obligation-id>`) with its
   declared action kinds and bindings, and the operator command
   `conveyor verification permissions inspect <work-order-id>`.
2. Keep renewing the claim. Reread `get_verification_context` at most every
   30 seconds.
3. Continue when every selected subject has a grant. Stop waiting when the
   remaining execution time falls below the longest declared timeout among the
   ungranted subjects plus ten minutes. Then submit the operator checkpoint
   described under "Stage outcome mapping": `feedback` names each missing
   grant and `required_action` names the operator act. The server records each
   ungranted subject as a `missing_grant` ground with no attempt, and releases
   the order at the operator checkpoint.

Never issue, request through MCP or simulate a grant yourself. No MCP tool or
worker route grants or revokes; only an authenticated operator user with
`operate_gates` can.

The operator works from the projection of
`GET /v1/work-orders/{id}/verification/permissions`:

```sh
conveyor --server '<server>' --workspace '<workspace>' \
  verification permissions inspect '<work-order-id>'
conveyor --server '<server>' --workspace '<workspace>' \
  verification permissions grant '<work-order-id>' \
  --subject 'kit:<kit-id>/<exercise-id>' \
  --action 'network:<binding>=https://<host>:<port>' \
  --request-key '<stable-key>'
conveyor --server '<server>' --workspace '<workspace>' \
  verification permissions revoke '<work-order-id>' \
  --grant-id '<grant-id>' --reason '<why>' --request-key '<stable-key>'
```

The task's Verify entry offers the same flow under Permissions. Both surfaces
copy the exact subject and digests from the frozen context. They show the
submitted revisions, the declared action kinds and bindings, the claim's lease
expiry and fixed deadline, and whether a grant can be issued. A filesystem
root, network origin or credential handle depends on the executing machine,
so it stays unresolved until the operator supplies it. A subject without
declared actions takes `--no-actions`, an explicit empty list. The operator
reviews the exact request before sending it and reads the receipt back by
grant ID. Reusing the request key after an uncertain response returns the same
grant; a changed request under that key is refused.

An authorized operator's refusal names a stable reason with recovery text:
`not_claimed`, `claim_expired`, `head_changed`, `context_missing`,
`context_stale`, `subject_unregistered`, `request_conflict`, `grant_unknown`
or `grant_revoked`. Foreign and unauthorized callers receive a generic
refusal. If the window closed before the grant arrived, the operator recovers
the verify order; the next verifier claim prepares a new context and repeats
the wait, and the operator grants against that context.

## Permission admission and invocation

Local `kit_permissions` entries identify `server`, `workspace`, `repository`,
`binding` and an explicit `actions` list. Actions contain `kind`, `binding`
and, where applicable, `target`: absolute filesystem roots, exact HTTP origins,
or credential handle names. `operator_interaction` has no target. The binding
of each action must match its local grant. The work-order grant separately
binds the subject contract and submitted revision. The runner requires both
grants to cover every requested action and checks revocation while running.

The manifest cannot grant access. Missing grants, prerequisites or interaction
require a truthful blocked/waiting report identifying the needed operator act.
Never approve access or supply an ambient factory/forge credential. Sensitive
inputs use approved `CONVEYOR_KIT_SECRET_*` handles; the runner rejects them in
the safe inputs file. Permission admission does not sandbox arbitrary code;
execute only in an operator-authorized environment that enforces the required
restrictions (VK-4).

## Toolchain environment and preflight

Verification children use the default toolchain unless the local execution
configuration has a `verification_toolchains` record for the exact server,
workspace and repository (VK-4.2). The default is
`PATH=/usr/local/bin:/usr/bin:/bin`, `LANG=C.UTF-8`, and an attempt-private
`HOME` and `TMPDIR`. Repository content, workspace or task policy, and the
parent environment cannot select, create or widen a record.

Records are honored only from operator-selected configuration: the file named
by `--config`, by `CONVEYOR_CONFIG`, or the user default, located outside the
verified checkout. A working-directory `conveyor.yaml` keeps its existing
precedence for other settings, but a matching `verification_toolchains` record
in it, or in any configuration file inside the checkout, is refused with
`toolchain preflight refused` before any attempt starts. The remedy moves the
record to operator configuration outside the checkout.

```yaml
verification_toolchains:
  - server: https://conveyor.example
    workspace: demo
    repository: funnelflux
    search_paths: [/opt/homebrew/bin, /Users/operator/go/bin, /usr/local/bin, /usr/bin, /bin]
    home: /Users/operator          # optional; omit to keep the private HOME
    settings:                      # optional; only these keys are accepted
      GOPATH: /Users/operator/go
      GOMODCACHE: /Users/operator/go/pkg/mod
      GOCACHE: /Users/operator/Library/Caches/go-build
```

A record replaces `PATH` with `search_paths` in order. The closed setting keys
are `GOPATH`, `GOROOT`, `GOENV`, `GOCACHE`, `GOMODCACHE`, `XDG_CONFIG_HOME`,
`XDG_CACHE_HOME` and `npm_config_cache`. The section holds at most 32 records;
a record holds at most 32 search paths; each path is at most 4096 bytes.
Values are literal absolute paths: `$` variables, a leading `~`, relative
paths, empty `GOPATH` list elements, control characters and a `:` inside one
search directory are refused at load. `GOENV` also accepts `off`. Two records
for one scope are refused. Values equal to a parent factory, forge or session
credential, or to an approved `CONVEYOR_KIT_SECRET_*` value, are refused
without echoing the value. The runner never discovers package-manager
prefixes, sources shell startup files, installs tools or copies user
configuration. Only the operator edits this section; never add or widen a
record on the operator's behalf.

Before `start_verification_attempt` and before any operation registration,
the runner preflights each subject. It resolves the entrypoint, every
`executable` prerequisite and a requested UI entrypoint through the snapshot.
For a configured record it also checks the search directories, `home`,
`GOROOT`, the `GOENV` file and `XDG_CONFIG_HOME`, and each configured cache
location that already exists. It fingerprints the `GOENV` file's content and
file identity and the resolved identity of `home`, `GOROOT` and
`XDG_CONFIG_HOME`; a `GOENV` file containing a credential value or credential
pattern is refused, because credentials travel only through approved
`CONVEYOR_KIT_SECRET_*` handles. A failure prints `toolchain preflight refused`
with the subject, the failed prerequisite or configuration field, the search
path and the `verification_toolchains` remedy. It starts no attempt, registers
no operation, launches no child and reports no execution. Report that
diagnostic and the operator act it names. With a matching unrevoked grant, it
is the runner's admission refusal for that subject (VK-13.2
`admission_refused`). A tool or fingerprinted configured location that changes
after preflight or during execution blocks the attempt through the existing
outcome path.

A configured `home` exposes operator-approved tool configuration and can make
caches shared between runs. A configured directory is neither a filesystem or
network grant nor a sandbox; the runner never creates or cleans shared `HOME`
or cache directories.

Run from the dedicated worktree, substituting the actual context and paths:

```sh
conveyor --server '<server>' --workspace '<workspace>' kit verify '<task-id>' \
  --context-id '<context-id>' --config '<local-execution-config>' \
  --coverage '<outside-checkout>/coverage.json' \
  --inputs '<outside-checkout>/inputs.json' \
  --attempt-root '<private-outside-checkout-directory>'
```

Omit `--inputs` when there are no supplied inputs. Its JSON maps
`kit:<kit-id>:<exercise-id>` or `ordinary:<obligation-id>` to safe input values.
Omit `--attempt-root` to use the task cache's `verification/<context-id>`
directory. The directory must be private and outside both task and primary
checkout inputs. Keep task caches on disk-backed storage and remove disposable
data only after preserving required evidence and completing the claim.

Without `--context-id`, the CLI prepares the current claim's context. Without
`--coverage`, it prints that context and exits with a request to register
obligations and grants; this is preparation, not a completed run. Reuse that
context after setup. The shipped CLI accepts exactly one revision for the task
repository. If the context requires additional repositories, report the scope
limitation rather than claim those repositories were executed.

The runner executes selected kit exercises, then covered ordinary obligations.
It checks HEAD and cleanliness before and during execution, supervises process
groups, limits runtime by the exercise and claim deadlines, and stops on claim
loss, cancellation or grant revocation. Children receive the resolved toolchain
environment, approved bindings, JSON `CONVEYOR_KIT_INPUTS`, and a private
`CONVEYOR_KIT_ATTEMPT_DIR`. Each child environment key is unique; a collision
with a toolchain or runner key is refused. Evidence environment attributes
record the toolchain scope and search path, the `HOME` mode, fingerprints of
`home` and setting values, the `GOENV` content digest
(`toolchain_config_GOENV_sha256`), the child environment key names, and the
resolved entrypoint, prerequisite and UI paths with SHA-256 digests. The runner
checks those identities and configured locations again after execution and
records `toolchain_after` as `unchanged` or `changed`. No credential value or
credential hash is recorded. Configured directory contents
(`toolchain_directory_contents`), transitive dependencies and
deployment/external state stay `unknown`.

## Evidence and success

| Type | Required basis |
| --- | --- |
| `api_exchange` | Sanitized request/response details, times, status or transport error. |
| `state_observation` | Target, method, capture time and observed value or artifact. |
| `assertion_result` | Assertion ID, expected/actual summaries, pass/fail/unknown and supporting references. |
| `execution_report` | Argv/tool/runtime, start/end, exit/timeout/cancellation and output hashes/truncation. |
| `visual_capture` | Authorized image/video artifact, verified media, capture tool, target and time. |
| `operator_observation` | Authenticated operator fact, time and supporting references. |

Keep observed tool results separate from an agent's assertions about them.
Keep authenticated operator observations separate from both. Capturing actor
and submitting actor are distinct provenance fields. The runner's tool label
does not let an agent claim authenticated human identity or change
self-reported attribution. No evidence type, passing assertion, process exit
or kit result grants acceptance or operator approval (REQ-8/AC-8.4).

The server binds evidence to context, run, subject, revisions and governing
pins. Capture time is UTC and remains separate from server receipt time.
Include safe inputs and environment; name unavailable optional context
`unknown`. Missing required provenance fails validation. Do not invent a kit
identity for ordinary evidence or relabel evidence from another revision.

Sanitize before upload. `submit_verification_evidence` validates a batch and
returns durable references; repeated identical submission keys are idempotent,
while changed content conflicts. Use `upload_verification_artifact` for bounded
chunks and finalization, then reference the finalized artifact in evidence.
Use `read_verification_evidence` to inspect retained items and authorized
artifact bytes. Limits are 256 KiB per typed item, 100 items per batch, 8 MiB
per request, 512 KiB decoded per artifact chunk, 25 MiB per artifact and 10 MiB
per screenshot. Binary media requires sanitation and masking attestations.

The runner creates execution reports and retains bounded sanitized output.
Children send other supported payloads through the nonce-protected loopback
channel in `CONVEYOR_KIT_OPERATIONS`; child signals cannot submit operator
observations or execution reports. A successful local write or spool file is
not durable server evidence. On upload failure, retain the reported spool and
inspect server receipts before retrying. Claim loss forbids further writes.
Never upload an old spool as observations of a successor attempt.

Success requires valid output cardinalities and exactly one passing result
for every frozen required assertion ID, with support from the same subject
and attempt. Missing/unknown required assertions leave the runner waiting;
failed required assertions fail it. Duplicate IDs are invalid. Optional
assertions remain reviewable and cannot replace a missing required one.
Scripts/hybrids also require exit zero and a successful execution report;
interactive/hybrid contracts require an authenticated operator observation.
Ordinary observations require their declared completion evidence. Every
declared operation must be completed or reconciled as applied. Neither
`supports` references nor an explicit empty assertion set claims AC acceptance
(VK-5, VK-7.1).

## Reconciliation, retries and UI

The runner registers the closed `operations` set before launch. A child sends
`operation.dispatching` with its step ID and capture time through the local
channel, waits for `DispatchAuthorized: true`, performs the provider call,
then sends `operation.completed` with a sanitized reference. A lost response
or an unresolved operation requires inspection, not another provider call.

Read operation history with `get_verification_context`. For
`reconciliation_required`, execute the declared bounded observation-only
reconciliation under approved read access and record the result using
`reconcile_verification_operation`: `context_id`, `operation_id`, `outcome`
(`applied`, `not_applied`, `unknown`), `source`, UTC `captured_at` and optional
safe `provider_reference`. The CLI does not invoke reconciliation entrypoints
automatically. Applied operations preserve original provenance and permit only
remaining safe work. Definitively not-applied operations can admit a new
dispatch; unknown stays blocked. Operator-action policy requires a recorded
operator disposition and authorization. A new source/context does not erase
unresolved prior operations (VK-4.1).

Repeating a CLI invocation with the original start key retries retained spool
uploads under the live claim but refuses to relaunch an existing attempt.
After replay admission, use an explicit new `--retry-key`; pass
`--replay-authorization` when operator recovery requires it. Safe replay needs
the declared safety basis. Timeout or successful process teardown is not proof
that the external mutation failed. Earlier attempts remain visible; only a
fresh successful attempt for the same contract/context resolves prior failure.

Use `--ui` only when the kit declares optional `ui: {argv, port, assets}` and
interaction is authorized. It starts a companion process from the kit root
with loopback host/port variables and stops it with the exercise. The UI must
honor those variables; the runner does not sandbox its network listener. UI
startup failure blocks that invocation. Script-only execution needs no UI.
The presentation identifies repository, kit/version and run, with Conveyor and
exact document links in Governance. Use consistent accessible controls and
show observed API results, not acceptance status. The relay enforces host,
origin and nonce checks. Its nonce proves local session access, not operator
identity; a visual click alone proves neither operator approval nor API success.

## Seal, report and exit

`conveyor kit verify` records exercises; it does not seal the stage. Inspect
the latest attempts, assertions, outputs, operations and coverage. Report
available usage as [Claim identity and usage](#claim-identity-and-usage)
describes, then call `submit_verification` with `context_id`, `outcome`, the completed `coverage`
and truthful `feedback` where needed. Complete success requires every selected
subject and ordinary obligation to satisfy its contract. No-kit discovery
still needs valid ordinary coverage. Sealing binds the result to the submitted
head, scope and pins and closes the context to new attempts/evidence.

### Stage outcome mapping

`submit_verification.outcome` accepts only `succeeded`, `feedback` and
`operator_action_required`. `blocked`, `waiting`, `failed`, `timed_out` and
`cancelled` are exercise states for `report_verification_outcome.state`; never
submit them as the stage outcome (feature-verification-kit-execution VK-13.1).

| Verification state at submission | `submit_verification.outcome` |
| --- | --- |
| Every required subject's latest attempt succeeded, coverage is complete and no operation is unresolved | `succeeded` |
| A required subject's latest attempt `failed` and needs a code correction, with no unresolved operation | `feedback`, naming the failure |
| A required subject's latest attempt is `blocked` or `waiting` | `operator_action_required` |
| A latest attempt is `timed_out` or `cancelled` and no replay is admitted within the remaining deadline | `operator_action_required` |
| A subject never started because admission was refused: a missing grant after the grant wait, or a missing local binding, credential handle or value, host prerequisite or sensitive input binding | `operator_action_required` |
| An external operation is unresolved | `operator_action_required` |

A checkpoint submission keeps the same `context_id` and completed `coverage`:

```json
{"context_id": "<context-id>", "coverage": {"...": "the registered coverage"},
 "outcome": "operator_action_required",
 "feedback": "<the reason, for example: no grant covers network:api after the grant wait>",
 "required_action": "<the exact operator act, for example: recover verify order <id>, then grant network:api for the next claim's context>"}
```

`feedback` and `required_action` are both required and bounded to 4096
characters. The server computes the checkpoint grounds from its own records
and never from this prose: blocked, waiting, timed-out or cancelled attempts,
`missing_grant` for an ungranted subject that never started,
`admission_refused` (labelled verifier-reported) for a granted subject the
runner could not admit, and unresolved operations. `conveyor kit verify`
prints this call for every subject it refused before start; an invalid
contract declaration or claim loss is an ordinary refusal instead. A checkpoint creates no
attempt, grant or evidence, so absent evidence stays visibly absent. It
releases the order with retry suppression, keeps the task on verify, and never
admits review. If a lost response leaves the result unknown, repeat the
identical call from the same claim; it returns the original receipt. Once a
context exists, submit the checkpoint instead of calling `release_work_order`.
Before any context exists, release with reason `operator checkpoint reached`
and a decision request. An unsupported outcome returns
`verification_outcome_unsupported` with this mapping and changes nothing.

Report reproducible code failures separately from infrastructure failures and
blocked/waiting operator actions; use the delivered failure/release lifecycle
rather than fabricate success. Check `get_verification_publication` separately:
a publication failure neither erases evidence nor changes an exercise outcome.
After verification submission succeeds, report the handoff and exit. Do not
poll `await_review`, judge acceptance, confirm governance, or merge. Independent
review consumes the sealed result and assesses criterion coverage.
