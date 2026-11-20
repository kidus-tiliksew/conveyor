# Engine-cost defect proofs, review round 3: original recorder output

Do not merge this branch. It holds only evidence for task 261120-64ffa9 (PR #1717).

Each directory holds the files that `scripts/validation_evidence.py run --policy policy.json --timeout 1800` wrote for one run. The files are the originals, byte for byte. They were not edited or assembled after the run:

- `attempt-*/manifest.json`: the original execution manifest, in its HMAC envelope.
- `attempt-*/command.log`: the complete redacted log of the Make command. The manifest's `log.sha256` is the SHA-256 of this file.
- `attempt-*/fixture/phases.jsonl`: the owned fixture's phase record (prepare, before snapshot, gate, after snapshot, teardown).
- `recorder-output.txt`: what the recorder printed, with its exit status.

`policy.json` is the recorder policy that all three runs used. `run-proof.sh` is the wrapper that switched the worktree to each commit, exported the cache and URL variables, and started the recorder.

Two kinds of file are withheld. The per-run integrity `key` is withheld because it keys the fingerprints of the environment values, and one of those values is the database URL with its password. `fixture/ownership.json` is withheld because it holds a SHA-256 of that URL. Both files stay in the run's durable task state on the machine that ran it.

All three runs used the same command, recorded as argv in each manifest:

```
make test-integration-singlestore-ci SINGLESTORE_INTEGRATION_SHARD=packages \
  SINGLESTORE_INTEGRATION_PACKAGES_packages=./internal/httpapi \
  'GO_INTEGRATION_FLAGS=-run=^TestTaskPollActivityEngineCostIntegration$'
```

Backend: a local SingleStore 10.9.0 server from the pinned CI image `ghcr.io/singlestore-labs/singlestoredb-dev:0.2.90@sha256:2f181587a74b63a83e7dbd46b02e9dce031b8cacf1f5651bff297b2d8c87df37`, run with `--cpus=4 --memory=4g`. For each run, the recorder created and dropped its own `conveyor_<token>_test` database.

| Run | HEAD in manifest (before and after) | Mutation | Median heavy/light | Factor | Outcome |
|---|---|---|---|---|---|
| payload-text-test | `a1c004d5ef364991dbea13b99495957490882dda` | `markerFoldQuery` bounce arm gets `INSTR(e.payload_json,'"user-request-changes"')>0` (the #1714 defect) | 3.960 | 2.5 | failure: ratio assertion, exit 2 |
| every-renewal | `ce5726b8816ec80bafeeffa619ac5bfea3a442e2` | `markerLifecycleQuery` renewal arm admits every renewal of the live-review tasks (the #1715 defect) | 33.150 | 2.5 | failure: ratio assertion, exit 2 |
| clean | `09b025f684376380a777e332063eb338fe52243d` (task branch head) | none | 1.590 | 2.5 | success, exit 0 |

Each proof commit is the task head `09b025f6` plus one changed line in `internal/store/singlestore/marker_events.go`.

The policy sets `inputs_complete: false`, and the recorder classifies every run as `reusable: false`. These are fresh-execution records for the proofs, not reusable gate evidence. CI evidence for the task head is in the pull request.

SHA-256 of every published file:

    bc8cd33b64fd9a2aad8c8e656285cd52cc3521bb1ba607784ebf9d913af791f0  ./clean/attempt-20261120T122126Z-535129-d45386c2e2aa/command.log
    ef19c204819e5d7545dcfff35be6654ff5ab1297e0261696c5377020da37b414  ./clean/attempt-20261120T122126Z-535129-d45386c2e2aa/fixture/phases.jsonl
    d4dfd8dd025e99768a0e280a451c581b973ab87368e871c19f17c19b6c3ee3be  ./clean/attempt-20261120T122126Z-535129-d45386c2e2aa/manifest.json
    75d7f44a77e9acbf4971bc873862abf3f40daeafccdc536b0199394cdcfd7b97  ./clean/recorder-output.txt
    5b303b2610604a4508774bb749b2f8422c7b3d67066fddcb2a515822f70d8861  ./every-renewal/attempt-20261120T121923Z-528851-883b8a2add4e/command.log
    475c45ffce0b0d3094a9e67944addd1c083e1c8a5d6a394af9a5ce23bf448079  ./every-renewal/attempt-20261120T121923Z-528851-883b8a2add4e/fixture/phases.jsonl
    dfb35de9c17aa38a57c116e916ee9208977261c504e99d0bb0cb32ea0ada4638  ./every-renewal/attempt-20261120T121923Z-528851-883b8a2add4e/manifest.json
    0383da85e90659fedbdc1d948a03aef79211694923577ce116e73d0b2e2fa0e4  ./every-renewal/recorder-output.txt
    e882eba150d6215b9ab7ea9bbf155e2870447e0c480015c7768452f17804938d  ./payload-text-test/attempt-20261120T121712Z-522179-8de9465ff9e5/command.log
    16e686043a2c6a332b602e70da7ee29195482b145b6ff91aacced8b8fdaf1edf  ./payload-text-test/attempt-20261120T121712Z-522179-8de9465ff9e5/fixture/phases.jsonl
    7482f9d13098668652abb95f821178a56680930b97a2defe0e394be053f9bcdf  ./payload-text-test/attempt-20261120T121712Z-522179-8de9465ff9e5/manifest.json
    0ebb23b2b0f4ed9fb3f78b9ffbeb1c9c11e45bd19bc7aac37d58ad3983d40997  ./payload-text-test/recorder-output.txt
    5e4528778495e20a122b4bdfa7f355bcf974e9b1d4065d31f18cb607114f54f9  ./policy.json
    f0f1595cd65db743571a1afa7405ebd6ba900638dfe3d6f9d097732096f99722  ./run-proof.sh
