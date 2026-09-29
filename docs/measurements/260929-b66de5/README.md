# Authored YAML update evidence

This directory exposes the retained evidence requested in review round 1 of task `260929-b66de5`. It is an evidence copy, not a replacement for the factory's document corpus. The implementation remains commit `e085737832d994ae1edb682099c37f54dedc5377`; this follow-up changes no Go source, tests, policy, or gates.

## Complete aggregate records

Each directory contains the complete original `command.log` bytes as `command.txt` and the complete original manifest compressed without modification as `manifest.json.gz`. The original bundles, including their private integrity keys, remain under `/Users/kidusteshome/.local/state/conveyor/260929-b66de5/`. Keys are not published.

| Attempt | Command | Recorded result |
| --- | --- | --- |
| [14:40 aggregate](attempt-20260929T144020Z-70251-cbead7c3e4de/command.txt) | `make validate web-typecheck` | Exit 2. The existing `TestDetectLocalHarnessesOffersOnlyHealthyPresentTemplates` probe failed after 10 seconds. This failed record is retained unchanged. |
| [14:49 aggregate](attempt-20260929T144928Z-84844-48d3914a2c02/command.txt) | `make validate web-typecheck` | Exit 0. Build, vet, formatting, ordinary Go tests, Biome, 494 Playwright tests, and capability typecheck passed. |
| [15:06 review follow-up](attempt-20260929T150611Z-1318-fcea8d8e2154/command.txt) | `make validate web-typecheck` | Exit 0. Fresh follow-up run passed the same complete gate and all 494 Playwright tests. |

`validate` depends on `build vet fmt-check test` in the Makefile. Formatting is silent on success; both historical logs advance past that prerequisite. PostgreSQL diagnostic failures inside validation-helper unit tests are deliberate fixture assertions, not live database runs. This task changes client-local YAML persistence, not SQL or database bindings, and makes no database integration or deployed-system claim.

[Detection reruns](detection-reruns-transcript.txt) preserves the predecessor's tool invocations and complete captured responses. It includes the malformed `make --eval` attempt and the corrected Make target that passed the detection test three times in 8.345 seconds. This is explicitly transcript evidence; the aggregate `command.txt` files above are complete original retained logs.

The historical success manifest's before and after hashes match the three delivered implementation files. Its policy declares incomplete external-input inventory and Git-dependent execution. These records therefore prove their recorded fresh executions, not reusable evidence or an exact-head CI result. No `check` or `bind` reuse is claimed.

The 15:06 run executed at implementation head `e085737832d994ae1edb682099c37f54dedc5377` while this evidence directory was being assembled. Its source files were unchanged; evidence-file additions changed the input inventory during execution. It is retained as a successful fresh run, not an equivalent-input reuse or validation of the later evidence-only commit's Git metadata. `git diff --check` passed before delivery. The original durable bundles remain intact; disposable task caches were preserved rather than recursively deleting paths outside the active worktree.

## Design revision

[component-harness-execution v11](component-harness-execution-v11.md) contains the complete proposed document, reconstructed from immutable v10 in the current contract and the exact replacement strings from the predecessor's recorded proposal call. It describes authored-node mutation, aliases and omissions, targeted projections, and preservation of nonempty review fallbacks.

[Proposal receipt](proposal-receipt.json) copies the current `get_work_order` snapshot's proposal metadata: document `component-harness-execution`, version 11, proposal event 562766, originating task `260929-b66de5`, and `confirmed: true`. The original PR described the proposal as pending; the current snapshot reports it confirmed. No new proposal or operator act was performed during this follow-up. The executor's `get_document` call was denied because that tool requires operator credentials, so this copy exposes the content to reviewers without changing credential scope.

## Contract coverage

- Bare and named setters share the authored-node writer in `cmd/conveyor/execution_setup.go` and `cmd/conveyor/named_execution_setup.go`, addressing AC-10.2 and AC-10.6.
- `TestConfigSetRoundTripIsLosslessForMultiSetupFile` compares complete raw YAML after each harness/model/effort edit for both addressing forms.
- `TestConfigSetReviewPreservesFallbacks` and `TestConfigSetReviewAllowsEmptyUnusedFallback` cover distinct nonempty fallbacks and valid empty fallbacks.
- Default-switch, wizard, and seat-mutation tests compare complete authored content; alias, merge, omission, singleton, permission, and failed-publication cases extend coverage.
- Existing named-setup, unrelated-configuration, wizard-twin, and persistence tests remain present and run in the complete ordinary Go suite.
- The existing design revision records the changed mechanism. Normalization, no-file bootstrap, probe-before-save, validation, atomic 0600 publication, and operator gates retain their implementation contracts.

## Checking the copies

From this directory, this read-only command verifies every complete log against its retained manifest. The decompressed JSON is the original manifest, including its policy, snapshots, timestamps, and outcome.

```sh
python3 - <<'PY'
import gzip, hashlib, json
from pathlib import Path
for path in sorted(Path('.').glob('attempt-*/manifest.json.gz')):
    envelope = json.loads(gzip.decompress(path.read_bytes()))
    record = envelope['record']
    data = (path.parent / 'command.txt').read_bytes()
    assert record['state'] == 'complete'
    assert record['log']['completeness'] == 'complete'
    assert len(data) == record['log']['bytes']
    assert hashlib.sha256(data).hexdigest() == record['log']['sha256']
    print(path.parent.name, record['policy']['command'], record['exit_status'])
PY
```

The publisher also verified the original manifests' HMACs against their retained private keys before copying. That detects local corruption; it is not independent attestation. Independent review and required CI remain separate acceptance boundaries under `component-verification-strategy` v16 and DEC-29.
