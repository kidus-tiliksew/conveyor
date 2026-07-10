# Resume-fidelity experiment

This is the Phase 1 calibration required by spec §20.2. It compares
Codex native resume plus the mandatory handoff snapshot against a fresh
session briefed with that same snapshot.

## Matrix

Each scenario gets an independent seed session, a real mid-tool-call
SIGKILL, and concurrent resume/cold probes:

| Scenario | Failure boundary | Probe CLI |
|---|---|---|
| `same_sandbox` | Kill the Codex process tree; keep the container | pinned |
| `fresh_sandbox` | Kill/remove the container; copy `CODEX_HOME` to a new host root; start a fresh container with identical sandbox paths | pinned |
| `version_bump` | Same restore boundary as fresh sandbox | next minor |

The fresh-sandbox case is a host-equivalent restore proxy on one Docker
daemon. It destroys all container state and changes the host-side session
root, which exercises the session serialization and deterministic-path
contract without copying subscription credentials to another machine. It
does not claim to test Docker-daemon, kernel, or CPU-architecture
portability.

## Recall and cost scoring

The seed turn receives a worker-lease decision and a private continuity
marker. The handoff snapshot contains the decision and rationale but omits
the marker. The structured probe awards:

- one point for recalling `lease_epoch`;
- one for stale-worker fencing;
- one for deterministic replay;
- one for rejecting `heartbeat_only` under partitions;
- one native-context point for the exact private marker.

Codex `turn.completed` events supply input, cached-input, output, and
reasoning token counts. The comparison uses
`max(input - cached_input, 0) + output` as an effective-token proxy. This
is not a dollar estimate for ChatGPT subscription-auth runs.

Resume qualifies as the routing default only if it preserves core recall,
recalls the native-only marker, and uses no more than 125% of the cold
start's effective tokens. Matching-version routing requires both same- and
fresh-sandbox probes to qualify; cross-version routing requires the
version-bump probe to qualify. A handoff snapshot remains mandatory in all
cases (spec §8.3).

## Run

Prerequisites are Docker and a logged-in host Codex CLI. The runner stages
only `~/.codex/auth.json`, mounts the fixture read-only, and deletes its
temporary session homes by default.

```sh
make resume-experiment
```

Results land under `results/<UTC run ID>/` as `result.json`, `report.md`,
and raw Codex JSONL evidence for every seed, crash checkpoint, and probe.
Rebuild both images before interpreting a rerun; the target does this
automatically.

## Latest calibration

The live run on 2026-07-10 used Codex 0.142.0 and 0.143.0. All three
native resumes survived the forced crash and scored 5/5, including the
native-only marker. Snapshot-briefed cold starts scored 4/5: full
rationale recall with the expected unknown marker.

Cold start nevertheless won the routing gate in all three scenarios.
Resume used 2.12× and 2.09× the effective tokens in the same-sandbox
and version-bump comparisons. The fresh-sandbox cold probe hit an
unusually warm prompt cache (11,648 of 11,722 input tokens), making that
single effective-token ratio 135.54×; total input plus output still
favored cold start by about 3×. The calibrated Codex default is therefore
`snapshot_cold_start` for matching and cross-version continuations, with
the snapshot always required. Native resume remains a supported
optimization and is still valuable for eliciting the snapshot from the
just-completed job; it is not the default successor-continuation route.

Evidence: [20260710T142750Z report](results/20260710T142750Z/report.md)
and [machine result](results/20260710T142750Z/result.json).

## Operational implications

What the calibration means for how the factory runs, beyond the routing
default itself:

1. **Iteration cost.** Review bounces and redirects — the factory's
   normal operating mode — continue at roughly half the token cost of
   resume-based continuation. Under subscription auth, tokens are
   rate-limit headroom rather than dollars, so per-round savings
   translate directly into tasks-per-day throughput. The known trade:
   resume was ~2× faster in wall clock (~10s vs ~15–20s); in a
   token-limited factory, headroom wins.
2. **The handoff snapshot is load-bearing.** Cold start by default makes
   the snapshot the only reasoning continuity between jobs. Elicitation
   quality degrades silently (successors get worse briefings; bounce
   counts creep) rather than loudly, so the elicitation prompt deserves
   pack-level tuning attention, and "handoff present and non-trivial"
   is a health signal worth monitoring.
3. **Session portability is deprioritized.** Sessions now only need to
   survive long enough for end-of-job elicitation inside the same
   container. Cross-host session archive/restore and the session-keyed
   part of the deterministic-path contract (spec §8.3 notes 2–3) drop
   from necessity to optimization. Resume's one retained job is
   eliciting the snapshot from the just-finished, still-warm session.
4. **Design stance made explicit.** What cold start forgets is exactly
   what the marker measured: context the agent never externalized.
   Continuations therefore rely only on code, commits, and the handoff
   — "disposable compute, persistent state" applied to agent memory,
   now backed by measurement rather than assumption.

Scope limits: one run per cell (the cache-warmth incident shows single
ratios can swing wildly, so 2.1× is directional, not precise), and
Codex-specific — rerun the matrix when the Claude Code adapter lands,
after any CLI pin bump, and on model changes. The routing default is
calibrated data the router consults, not hardcoded truth (spec §8.3:
routing policy follows the data).
