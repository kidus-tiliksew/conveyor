# Codex resume-fidelity experiment 20260710T142750Z

Run: 2026-07-10 14:27:50Z to 2026-07-10 14:30:08Z  
Versions: `0.142.0` (`conveyor-base:dev`) → `0.143.0` (`conveyor-base:codex-0.143.0`)

Host boundary: The fresh-host condition is a host-equivalent restore boundary on one Docker daemon: the original container is destroyed and session state is copied to a distinct host directory before a fresh container starts. No auth material leaves the local machine.

## Results

| Scenario | Resume recall | Cold recall | Resume effective tokens | Cold effective tokens | Ratio | Gate |
|---|---:|---:|---:|---:|---:|---|
| same_sandbox | 5/5 | 4/5 | 19808 | 9360 | 2.12 | cold |
| fresh_sandbox | 5/5 | 4/5 | 19518 | 144 | 135.54 | cold |
| version_bump | 5/5 | 4/5 | 20251 | 9695 | 2.09 | cold |

Effective tokens are `max(input - cached_input, 0) + output`; this is a token-cost proxy, not a dollar estimate for subscription-auth runs. Resume qualifies only when it preserves core recall, recalls the native-only marker, and costs no more than 125% of cold start.

### same_sandbox

SIGKILL Codex and resume its session in the still-running container.

- Crash observed: `true`
- Resume answer: decision `use lease_epoch as the worker-claim mechanism`; marker `amber-orchid-27`; core `4/4`; duration `9.4s`
- Cold answer: decision `use lease_epoch as the worker-claim mechanism`; marker `unknown`; core `4/4`; duration `17.2s`
- Resume tokens: input `35657` (`16000` cached), output `151`, effective `19808`
- Cold tokens: input `11722` (`2432` cached), output `70`, effective `9360`
- Recommendation: prefer snapshot cold start (resume did not meet recall+cost gate; effective-token ratio 2.12)
- Evidence: `same_sandbox/seed.events.jsonl`, `same_sandbox/crash.events.jsonl`, `same_sandbox/resume.events.jsonl`, `same_sandbox/cold.events.jsonl`

### fresh_sandbox

Kill the container, copy the persisted CODEX_HOME into a new host root, and resume in a fresh container at identical in-container paths.

- Crash observed: `true`
- Resume answer: decision `use lease_epoch as the worker-claim mechanism`; marker `amber-orchid-27`; core `4/4`; duration `11.9s`
- Cold answer: decision `use lease_epoch as the worker-claim mechanism`; marker `unknown`; core `4/4`; duration `14.5s`
- Resume tokens: input `35365` (`16000` cached), output `153`, effective `19518`
- Cold tokens: input `11722` (`11648` cached), output `70`, effective `144`
- Recommendation: prefer snapshot cold start (resume did not meet recall+cost gate; effective-token ratio 135.54)
- Evidence: `fresh_sandbox/seed.events.jsonl`, `fresh_sandbox/crash.events.jsonl`, `fresh_sandbox/resume.events.jsonl`, `fresh_sandbox/cold.events.jsonl`

### version_bump

Seed with the pinned CLI, kill the container, restore CODEX_HOME, and resume with the next minor CLI image.

- Crash observed: `true`
- Resume answer: decision `use lease_epoch as the worker-claim mechanism`; marker `amber-orchid-27`; core `4/4`; duration `10.6s`
- Cold answer: decision `use lease_epoch as the worker-claim mechanism`; marker `unknown`; core `4/4`; duration `21.5s`
- Resume tokens: input `47706` (`27648` cached), output `193`, effective `20251`
- Cold tokens: input `12019` (`2432` cached), output `108`, effective `9695`
- Recommendation: prefer snapshot cold start (resume did not meet recall+cost gate; effective-token ratio 2.09)
- Evidence: `version_bump/seed.events.jsonl`, `version_bump/crash.events.jsonl`, `version_bump/resume.events.jsonl`, `version_bump/cold.events.jsonl`

## Calibrated routing default

- Matching CLI version: `snapshot_cold_start`
- Cross-version restore: `snapshot_cold_start`
- Snapshot fallback: `always_required`
- Basis: The deterministic gate requires preserved core recall, native-marker recall, and resume effective tokens at or below 125% of cold start. same=false fresh=false version_bump=false.

This is a one-run calibration, not a latency or cache distribution. Rerun after a pinned CLI or model change. Large cache-ratio swings remain visible in the raw counts above rather than being averaged away.
