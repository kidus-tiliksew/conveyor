# Executing verification kits and ordinary checks

Use this playbook under a live `verify` claim. Follow the delivered contract,
approved plan and pinned authority. Implementation ends at submission; review
assesses sealed evidence. A successful kit supplies evidence and never grants
acceptance or operator approval.

Authority: `req-verification-kits` v1 REQ-8/AC-8.1 and REQ-8/AC-8.4;
`feature-verification-kit-execution` v5 VK-2 through VK-9, including VK-4.1,
VK-5.1 and VK-7.1; DEC-29, DEC-40 and DEC-43. The `conveyor-kit` skill covers
the full authoring contract. This playbook describes the shipped CLI and MCP
protocol, including ordinary checks when no kit is selected.

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
   operator-issued grants and inspect the local execution configuration.

The runner needs launcher-provided `CONVEYOR_WORK_ORDER_ID`,
`CONVEYOR_SESSION_ID`, `CONVEYOR_CLIENT_TOKEN` and explicit workspace context.
Keep the token out of argv, logs and children. Use the configured server; a
connection failure does not authorize guessing another endpoint.

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
loss, cancellation or grant revocation. Children receive minimal environment,
approved bindings, JSON `CONVEYOR_KIT_INPUTS`, and a private
`CONVEYOR_KIT_ATTEMPT_DIR`. The runner records resolved executable hashes before
and after; unavailable deployment/external state stays `unknown`.

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
the latest attempts, assertions, outputs, operations and coverage. Call
`submit_verification` with `context_id`, `outcome`, the completed `coverage`
and truthful `feedback` where needed. Complete success requires every selected
subject and ordinary obligation to satisfy its contract. No-kit discovery
still needs valid ordinary coverage. Sealing binds the result to the submitted
head, scope and pins and closes the context to new attempts/evidence.

Report reproducible code failures separately from infrastructure failures and
blocked/waiting operator actions; use the delivered failure/release lifecycle
rather than fabricate success. Check `get_verification_publication` separately:
a publication failure neither erases evidence nor changes an exercise outcome.
After verification submission succeeds, report the handoff and exit. Do not
poll `await_review`, judge acceptance, confirm governance, or merge. Independent
review consumes the sealed result and assesses criterion coverage.
