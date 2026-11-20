#!/bin/sh
# Checks every file against SHA256SUMS, checks each complete log against the
# sha256 its manifest recorded, and prints each run's command, head, outcome,
# guard result, test results, and benchmark lines.
set -eu
cd "$(dirname "$0")"
sha256sum -c SHA256SUMS
for run in r1 r2-interrupted r2 r3 r4 r5; do
  echo "== $run"
  python3 - "$run" <<'PY'
import hashlib, json, sys
run = sys.argv[1]
record = json.load(open(run + "-manifest.json"))["record"]
log = open(run + "-command.log", "rb").read()
assert hashlib.sha256(log).hexdigest() == record["log"]["sha256"], "log digest differs from manifest"
print("go command:", record["policy"]["command"][-1].split("=", 1)[1])
print("head:", record["before"]["git"]["head"], "after:", record["after"]["git"]["head"])
print("outcome:", record["outcome"], "exit:", record["exit_status"], "log:", record["log"]["completeness"])
PY
  grep -E -- '^--- (PASS|FAIL)|^(ok|FAIL)[[:space:]]|allocations per pass|^BenchmarkRedactJSON' "$run-command.log" || true
done
