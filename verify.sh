#!/bin/sh
# Checks every file against SHA256SUMS, checks each complete log against the
# sha256 its manifest recorded, and prints each run's command, head, outcome,
# fixture teardown, resource cleanup, test results, and the activity-markers
# measurement.
set -eu
cd "$(dirname "$0")"
sha256sum -c SHA256SUMS
for run in r1 r2 r3 r4 r5 r6 r7; do
  echo "== $run"
  python3 - "$run" <<'PY'
import hashlib, json, sys
run = sys.argv[1]
record = json.load(open(run + "-manifest.json"))["record"]
log = open(run + "-command.log", "rb").read()
assert hashlib.sha256(log).hexdigest() == record["log"]["sha256"], "log digest differs from manifest"
fixture = record.get("fixture") or {}
print("command:", " ".join(record["policy"]["command"]))
print("head:", ((record.get("before") or {}).get("git") or {}).get("head"), "after:", ((record.get("after") or {}).get("git") or {}).get("head"))
print("outcome:", record["outcome"], "exit:", record["exit_status"], "log:", record["log"]["completeness"])
print("fixture:", fixture.get("database"), "teardown:", fixture.get("teardown_outcome"),
      "resource cleanup:", (record.get("resource_cleanup") or {}).get("outcome"), "fixture error:", record.get("fixture_error"))
for line in log.decode().splitlines():
    if line.startswith("READ_EGRESS_MEASUREMENT ") and '"case":"activity-markers"' in line:
        m = json.loads(line[len("READ_EGRESS_MEASUREMENT "):])
        s = m["summary"]
        print("activity-markers:", m["backend"], "candidate", m["candidate_revision"], "baseline", m["baseline_revision"],
              "work_equal", m["work_equal"], "fixture", m["fixture"])
        for counter in ("returned_bytes", "rows", "statements"):
            print("  %s median: baseline %d, candidate %d" % (counter, s["baseline"][counter]["median"], s["candidate"][counter]["median"]))
PY
  grep -E -- '^(--- |    --- |        --- )(PASS|FAIL|SKIP)|^(ok|FAIL)[[:space:]]' "$run-command.log" | grep -E 'ActivityMarkerReviewRounds|^(ok|FAIL)|HotReadEgress' || true
done
