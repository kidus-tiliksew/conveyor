# Retained validation evidence: Conveyor task 261120-db3057, pull request 1716

These records come from `scripts/validation_evidence.py run`, as `docs/validation-evidence.md` describes.
The helper ran in the task worktree at the reviewed head `f1a62c98df0db817e50b407d94e2a7141d2ff86a`, with a clean tree before and after each run.
Each run started under `env -i` with only PATH, HOME, LANG, GOTMPDIR, TMPDIR, and a fresh empty GOCACHE for that run.
The host is a linux/amd64 sandbox (AMD EPYC 9455, GOMAXPROCS 4) with go1.24.0.

Every run uses the same Make command, with only the Go command changed through `RUN_GO_UNIT_TESTS`, as step 5 of the approved plan directs:

```
make -o compose-check -o dashboard-fresh -o test-release -o test-validation -o test-validation-environment -o vk10-runtime test-go RUN_WEB_LINT=: RUN_GO_UNIT_TESTS='<go command>'
```

The six `-o` targets and the web lint do not build or test `internal/redact`. CI runs all of them at the same head.

**Baseline.** The "before" runs (r1, r2) use a Go build overlay, `inputs/overlay.json`, so the worktree stays at the reviewed head. The overlay:
- replaces `internal/redact/redact.go` with `inputs/base/redact.go`, which is `git show dc3fdccca9ce4a0770b0d925f94a20e38f0d059f:internal/redact/redact.go` (sha256 `d6d91227fba76aa31472bfbed6ba8efad78b6d9acf8f124fe0973f60f0bc3336`);
- hides `internal/redact/redact_cache_test.go`, which uses the new clock seam and does not compile against the base file.

The guard and benchmark code in `redact_pass_test.go` is the same on both sides.

**Fixture.** The fixture is one JSON array of 1,000 short strings.
- Secrets are synthetic: 1,700-byte PEM-like keys registered with zero expiry, and installation-token stand-ins registered with a one-hour expiry.
- 6 secrets = 2 keys and 4 tokens. 13 secrets = 3 keys and 10 tokens.
- A `pass` case builds a fresh redactor and runs one `RedactJSON`.
- A `reuse` case uses one redactor for 8 documents and 8 plain `Redact` calls.

The guard compares allocations of a fresh redactor and one `RedactJSON` pass over the 1,000 strings, with and without 3 registered keys. It fails when the keys add more than 200 allocations.

| Run | Purpose | Code | Go command | Head | Outcome | Time (UTC) | Resource cleanup | Complete log |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| r1 | Allocation guard with the fix reverted. Expected to fail. | base `redact.go` (overlay) | `go test -count=1 -overlay=<inputs/overlay.json> -v -run TestRedactJSONBuildsSecretFormsOncePerPass ./internal/redact/` | `f1a62c98df0db817e50b407d94e2a7141d2ff86a` | failure (exit 2) | 2026-11-20T08:47:07Z to 2026-11-20T08:47:37Z | success | 722 bytes, complete, sha256 `01f71688cab0b9a7f4202f1ab289d5d5e1c660412ee8021a55f749bf62fe1bf6` |
| r2-interrupted | First attempt of r2. A sandbox platform restart stopped it with SIGTERM. Kept for completeness and not used. | base `redact.go` (overlay) | `go test -count=1 -overlay=<inputs/overlay.json> -run '^$$' -bench BenchmarkRedactJSON -benchmem -benchtime=10x -count=3 ./internal/redact/` | `f1a62c98df0db817e50b407d94e2a7141d2ff86a` | interrupted (exit -15) | 2026-11-20T08:47:45Z to 2026-11-20T08:48:25Z | success | 1990 bytes, complete, sha256 `9c42c3fc82a580f84672cd0fafcb2a170e0322d0a5bd3c82e3296be7a92c0f1f` |
| r2 | BenchmarkRedactJSON before the fix. | base `redact.go` (overlay) | `go test -count=1 -overlay=<inputs/overlay.json> -run '^$$' -bench BenchmarkRedactJSON -benchmem -benchtime=10x -count=3 ./internal/redact/` | `f1a62c98df0db817e50b407d94e2a7141d2ff86a` | success (exit 0) | 2026-11-20T08:48:50Z to 2026-11-20T08:49:57Z | success | 2288 bytes, complete, sha256 `13c4bad9f2aff5a7f80331ae7474f60614c2e4d1652d0a648d3261c3b2ad3262` |
| r3 | Allocation guard with the fix. | fix | `go test -count=1 -v -run TestRedactJSONBuildsSecretFormsOncePerPass ./internal/redact/` | `f1a62c98df0db817e50b407d94e2a7141d2ff86a` | success (exit 0) | 2026-11-20T08:50:01Z to 2026-11-20T08:50:18Z | success | 375 bytes, complete, sha256 `2854f37fe224cd528520f191132f3b47f23383ff7c03b67eae0e3c9967a6fff3` |
| r4 | BenchmarkRedactJSON after the fix. | fix | `go test -count=1 -run '^$$' -bench BenchmarkRedactJSON -benchmem -benchtime=10x -count=3 ./internal/redact/` | `f1a62c98df0db817e50b407d94e2a7141d2ff86a` | success (exit 0) | 2026-11-20T08:50:19Z to 2026-11-20T08:50:37Z | success | 2202 bytes, complete, sha256 `7af153bc7d5d34fd00a9b17c91d2ed286a07131f8b382f6a22e7e78833ac6cc4` |
| r5 | Whole internal/redact package under the race detector, with the fix. | fix | `go test -count=1 -race -v ./internal/redact/...` | `f1a62c98df0db817e50b407d94e2a7141d2ff86a` | success (exit 0) | 2026-11-20T08:50:41Z to 2026-11-20T08:51:02Z | success | 14532 bytes, complete, sha256 `120ae2b469784acb345c037a8cbd719aa53a4d297f80377e51a40207f87fc94b` |

## Results

- **r1, guard with the fix reverted: failed.** `allocations per pass over 1,000 strings: 59126 with three keys, 5028 without`, so the keys added 54,098 allocations against a limit of 200.
- **r3, guard with the fix: passed.** `3081 with three keys, 3029 without`, so the keys added 52 allocations.
- **r5:** all 19 tests of the package passed under `-race`.

Benchmark medians of the 3 counts in r2 (before) and r4 (after):

| Case | Before ns/op | After ns/op | Before B/op | After B/op | Before allocs/op | After allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| secrets=0/pass | 329,614 | 268,250 | 262,685 | 190,717 | 5,030 | 3,031 |
| secrets=0/reuse | 2,526,993 | 2,526,133 | 2,076,362 | 1,523,928 | 40,239 | 24,229 |
| secrets=6/pass | 63,812,032 | 458,406 | 69,825,095 | 253,412 | 86,180 | 3,107 |
| secrets=6/reuse | 466,995,730 | 2,912,436 | 558,970,603 | 1,572,585 | 689,578 | 24,301 |
| secrets=13/pass | 100,925,032 | 559,682 | 111,387,276 | 301,382 | 163,284 | 3,185 |
| secrets=13/reuse | 842,896,632 | 3,719,277 | 891,509,872 | 1,614,071 | 1,306,488 | 24,377 |

## Files for each run

- `rN-manifest.json`: the helper's manifest. It holds the policy, the before and after input snapshots, the outcome, the exit status, the log digest, and the resource cleanup. Its HMAC key stays on the run machine. The key only detects accidental corruption. The log's sha256 above checks the published log.
- `rN-command.log`: the complete command output.
- `rN-policy.json`: the audited input policy.
- `rN-invocation-inventory.json`: the invocation's owned resources and their removal.
- `inputs/`: the overlay and the base `redact.go` that r1 and r2 consumed.

These records are fresh executions, not `check` or `bind` candidates, and no local record stands in for a CI job.
CI evidence for the same head is CI run 44697712659, attempt 2.

Run `./verify.sh` to check `SHA256SUMS` and each log against the digest its manifest recorded. It then prints each run's command, head, outcome, and results.
