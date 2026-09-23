# Authoring verification kits

Use this playbook to author a repository kit under an approved implementation
claim. The `conveyor-kit-verify` skill covers execution under a separate verify
claim. Kit outcomes supply evidence; they never grant acceptance or operator
approval. A manifest's `supports` references route evidence to criteria without
marking those criteria satisfied.

Authority: `req-verification-kits` v1 REQ-8/AC-8.1 and REQ-8/AC-8.4;
`feature-verification-kit-execution` v5 VK-2, VK-3, VK-4, VK-4.1, VK-5,
VK-5.1, VK-7.1, VK-8 and VK-9; DEC-40 and DEC-43. The shipped parser and
runner live in `internal/verification`, `internal/core/verification.go`, and
`cmd/conveyor/kit_*.go`. This playbook describes those interfaces.

## Manifest and selection

Put one schema-1 YAML document in `.conveyor/kits/manifest.yaml`. The top-level
fields are `schema_version: 1` and `kits`. The parser rejects unknown fields,
duplicate keys, aliases, anchors, duplicate identities, unsupported schemas,
absolute paths and traversal. Limits are 1 MiB, 100 kits and 100 exercises per
kit. Paths must remain inside the repository and the kit root after symlink
resolution; nested repositories and submodules do not expand scope.

Each kit declares these fields:

| Field | Contract |
| --- | --- |
| `id`, `name`, `version` | Nonempty kit identity, display name and readable content version. |
| `path` | Repository-relative kit root containing committed files. |
| `governing_pins` | `requirements` and `system_designs` lists of `{document_id, version}` with positive immutable versions. |
| `exercises` | One or more exercise contracts described below. |
| `ui` | Optional `{argv, port, assets}`; assets are kit-relative paths. |

Automatic selection requires a nonempty set of pins that exactly matches a
subset of the work order's authoritative document identities, kinds and
versions. Additional work-order pins are allowed. An unpinned kit or a
mismatched pin is ineligible, with reasons in the discovery receipt. Invalid
or unavailable content is unresolved, never passed. A verified absent manifest
is `no_manifest`; it does not waive ordinary verification obligations (VK-2,
VK-5.1). Only tasks whose frozen policy enables `verify_stage` enter this
stage (DEC-43).

The content digest binds the normalized kit contract and the sorted committed
Git tree entries under its root, including modes, paths and blob IDs. A
readable `version` alone does not identify the bytes or replace the source SHA.

## Exercise fields

| Field | Contract |
| --- | --- |
| `id`, `stages` | Unique exercise ID and `stages: [verify]`. |
| `kind` | `script`, `interactive` or `hybrid`. |
| `argv`, `cwd` | Direct argument vector and kit-relative working directory; no implicit shell expansion or package installation. |
| `timeout_seconds` | Positive timeout, bounded again by the work-order deadline. |
| `prerequisites` | Unique `{id, kind, environment_binding}` entries; kinds are `executable`, `service`, `credential`, `operator_interaction`. |
| `permissions` | `filesystem_read`/`filesystem_write` with kit-relative `path`, `network` with `target_binding`, or `operator_interaction`. |
| `inputs` | Unique `{name, type, required, sensitive}` entries; types are `string`, `boolean`, `integer`, `number`. |
| `required_assertions` | Explicit unique assertion-ID list, including `[]` when none are required. |
| `retry_policy` | `safe_to_replay`, `reconciliation_required`, or `operator_action_required`. |
| `safety_basis` | Required nonempty explanation for `safe_to_replay`. |
| `operations` | Explicit list of every external mutation, or `[]` for none. |
| `evidence_outputs` | `{type, schema_version: 1, minimum_items}` entries; manifest minima are nonnegative. |
| `supports` | `{document_id, version, acceptance_criterion_id}` references to ACs in declared requirement pins. |

Example with illustrative document IDs; replace the pins with the approved
contract and commit the referenced executable before local validation:

```yaml
schema_version: 1
kits:
  - id: fixture-read
    name: Fixture read check
    version: "1.0.0"
    path: .conveyor/kits/fixture-read
    governing_pins:
      requirements:
        - document_id: req-example
          version: 1
      system_designs: []
    exercises:
      - id: read
        stages: [verify]
        kind: script
        argv: ["./read.sh"]
        cwd: "."
        timeout_seconds: 60
        prerequisites: []
        permissions: []
        inputs: []
        required_assertions: []
        retry_policy: safe_to_replay
        safety_basis: Reads a committed fixture without external mutations.
        operations: []
        evidence_outputs:
          - type: execution_report
            schema_version: 1
            minimum_items: 1
        supports:
          - document_id: req-example
            version: 1
            acceptance_criterion_id: AC-1.1
```

An empty assertion list makes this a process/output check. It does not prove
the cited AC. For a behavior assertion, declare its stable ID before execution
and emit an `assertion_result` supported by actual observations.

## Permissions and child inputs

Governance matching and repository content grant no access. The runner checks
requested actions against both an operator-issued work-order grant and local
`kit_permissions` for the exact server, workspace, repository and binding.
Missing permission, credentials, service bindings or interaction blocks the
run with a required action. Never widen a grant on the operator's behalf.
These checks admit execution into an operator-authorized environment; they do
not sandbox arbitrary scripts. Block execution if required restrictions cannot
be enforced there (VK-4, REQ-7/AC-7.3).

The runner supplies a minimal environment. It resolves bare executable names
from `/usr/local/bin`, `/usr/bin`, and `/bin`. It passes typed values as JSON
in `CONVEYOR_KIT_INPUTS` and network bindings as
`CONVEYOR_KIT_BINDING_<UPPERCASE_BINDING>` with hyphens replaced by underscores.
Sensitive inputs use approved `CONVEYOR_KIT_SECRET_*` credential handles, not
the inputs JSON file. Factory, forge and parent-session credentials must not
enter the child, argv, browser assets or evidence.

Write output beneath `CONVEYOR_KIT_ATTEMPT_DIR`, outside checkout inputs. The
runner also points child `HOME` and `TMPDIR` there. Source changes or resolved
executable changes during execution block the result. Report unavailable
deployment or external-state information as `unknown`; executable hashes do
not prove all external dependencies stayed unchanged.

## Operations and uncertain execution

Declare each external mutation with a stable `id` and `target_binding` naming
a permission binding. `reconciliation_required` also requires a bounded
`reconciliation: {argv, timeout_seconds}` entrypoint for each operation. That
entrypoint observes status without repeating the mutation. Compensation is a
new declared and authorized mutation, not an implicit reconciliation action.

Before launch, the runner durably registers every declared operation. The
child reads `CONVEYOR_KIT_OPERATIONS`, a JSON object with `url`, `nonce`, and
an `operations` map from step IDs to durable operation IDs. POST JSON to that
URL with the `X-Conveyor-Kit-Nonce` header:

```json
{"type":"operation.dispatching","step_id":"create-record","captured_at":"2026-09-22T09:00:00Z"}
```

Wait for a successful response with `DispatchAuthorized: true` before the
provider call. Where the provider supports idempotency, use the durable
logical operation identity and honor its scope and validity window. After the
call, send `operation.completed` with the same step and a fresh capture time,
plus a sanitized `provider_reference`. Do not retry a provider call merely
because the local response was lost. A duplicate start receipt or dispatch
acknowledgement never authorizes another mutation.

Missing completion, timeout and claim loss leave uncertainty, including for
registered steps that never sent a dispatch signal. Inspect the durable record.
Run the declared observation procedure only with approved read access, then
record `applied`, `not_applied` or `unknown` through
`reconcile_verification_operation`, with source, capture time and safe provider
reference. The CLI does not automatically run reconciliation entrypoints.
`applied` preserves the original operation and allows only remaining safe
steps; `not_applied` can admit a fresh dispatch under the replay checks;
`unknown` remains blocked. `operator_action_required` needs authenticated
operator recovery authorization. Changing attempt IDs, context or source SHA
does not clear an unresolved predecessor (VK-4.1).

## Evidence and assertions

Call `get_evidence_schemas` under the claim to inspect the closed schema-1
payloads and envelope. Do not invent fields from these summaries:

| Type | Observation or claim retained |
| --- | --- |
| `api_exchange` | Method, sanitized URL, request/response times, status or transport error and safe summaries. |
| `state_observation` | Target, observation method, capture time and structured value or artifact. |
| `assertion_result` | Stable assertion ID, text, expected/actual summaries, `pass`/`fail`/`unknown`, and supporting references. |
| `execution_report` | Safe argv, tool/runtime identity, start/end, exit/timeout/cancellation and sanitized output hashes/truncation. |
| `visual_capture` | Authorized image/video artifact, verified media type, capture tool, target and time. |
| `operator_observation` | Authenticated operator identity, observed fact, time and supporting references. |

Every item preserves capture time separately from receipt time, capturing
actor/tool separately from authenticated submitter, context, attempt, source
revision, subject, governing pins, safe inputs and environment. Kit subjects
carry kit identity/content digest and exercise ID. Ordinary subjects carry
obligation ID/contract digest, without invented kit fields. Artifact hashes
describe retained sanitized bytes. Missing required provenance fails
validation; optional unavailable context is explicit `unknown` (VK-5).

A tool observation records what a tool saw. An agent assertion interprets
observations and links its basis. An operator observation comes from the
authenticated user endpoint with the operate-gates capability. A child or
local UI nonce cannot establish human identity. Self-reported tool attribution
must remain labeled as such. None of these evidence types grants approval.

Each required assertion needs exactly one passing result with resolvable
support from the same subject and attempt. Missing, failed or unknown required
assertions prevent success even with exit zero. Duplicate assertion IDs are
invalid; a later passing item cannot erase a failed item. Optional assertions
remain visible but cannot substitute for a required ID. Script/hybrid success
also needs exit zero and a successful execution report; interactive/hybrid
success needs an operator observation. Output cardinalities and resolved
operations remain required. `supports` and output counts do not establish
assertion success (VK-7.1).

Children can POST `type: evidence` to the run-local channel with a stable
`key`, `evidence_type`, UTC `captured_at` and schema-valid `payload`. The runner
supplies provenance. This channel refuses `operator_observation` and
`execution_report`; the runner creates execution reports itself. Use the
claim-bound artifact/evidence tools when artifacts or explicit supporting
links are needed. Do not treat stdout as automatic typed evidence.

Sanitize credentials, authorization/cookie headers, query values and provider
payloads before capture/upload. The server repeats filtering and validation.
Typed items are limited to 256 KiB and batches to 100 items within the 8 MiB
request limit. Artifact chunks are at most 512 KiB decoded; finalized artifacts
are at most 25 MiB, with a 10 MiB screenshot limit. Binary media needs a
sanitation record and masking attestation. Upload finalization does not attach
the artifact as evidence. Supporting references must resolve without cycles;
older revision evidence remains context and cannot satisfy current outputs.

## Ordinary checks and optional UI

Required checks outside a selected kit use `register_verification_obligation`
before execution. Register the source document/version/section or approved-plan
citation, description and immutable exercise contract. An ordinary
`observation` contract can use an explicit `observation_procedure` and required
typed outputs instead of argv. A missing demonstrated replay basis defaults
to `operator_action_required`. Coverage must map governing instructions and
done criteria to selected exercises or ordinary obligations, including a
justified empty set when appropriate. See the execution playbook for the
registration and coverage wire fields (VK-5.1).

Script-only kits need no UI. An optional `ui` declares argv, a loopback port
from 1 through 65535 and kit-relative assets. `--ui` launches it alongside the
exercise with interaction permission; it receives `CONVEYOR_KIT_UI_HOST` and
`CONVEYOR_KIT_UI_PORT`. The UI must bind to that loopback address. The runner
does not rewrite arbitrary server code to enforce its bind address.

Show repository, kit name/version and run context in the header. Put the
Conveyor name and exact governing document links in a Governance section.
Use consistent labels, spacing, disabled/busy/error states and keyboard-accessible
controls. Exercise implemented APIs through approved bindings and show their
observed exchanges. Do not present a requirement-implementation or acceptance
status dashboard. The loopback relay checks host, origin and nonce; never
store factory tokens in URLs, assets or browser storage. A button click is
neither an authenticated operator observation nor proof of API success (VK-9).

## Validate and deliver

Run `conveyor kit validate <repository-or-manifest-path> --stage verify` with
repeatable `--pin requirement:<id>:<version>` and
`--pin system_design:<id>:<version>`. This offline command checks structure,
paths, committed kit content and selection against caller-supplied pins. It
does not verify their authority, execute code or contact the server. Draft
manifest identity is `working-tree:sha256:<hash>`; source revision is separate.
Uncommitted or ignored kit files cannot produce a committed content digest.

Complete the repository's required Make gates and submit the implementation.
The verifier later runs the submitted revision under its own claim. Preserve
both operator gates and independent review.
