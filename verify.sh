#!/bin/sh
# Checks every file against SHA256SUMS, checks each complete log against the
# sha256 its manifest recorded, and prints each run's command, head, outcome,
# fixture teardown, resource cleanup, test result, and the medians of the
# requirement-list-page measurement.
set -eu
cd "$(dirname "$0")"
sha256sum -c SHA256SUMS
for run in r1 r2; do
  echo "== $run"
  python3 - "$run" <<'PY'
import hashlib, json, statistics, sys
run = sys.argv[1]
record = json.load(open(run + "-manifest.json"))["record"]
log = open(run + "-command.log", "rb").read()
assert hashlib.sha256(log).hexdigest() == record["log"]["sha256"], "log digest differs from manifest"
fixture = record["fixture"]
print("command:", " ".join(record["policy"]["command"]))
print("head:", record["before"]["git"]["head"], "after:", record["after"]["git"]["head"])
print("outcome:", record["outcome"], "exit:", record["exit_status"], "log:", record["log"]["completeness"])
print("fixture:", fixture["database"], "teardown:", fixture["teardown_outcome"],
      "resource cleanup:", record["resource_cleanup"]["outcome"])
for line in log.decode().splitlines():
    if line.startswith("READ_EGRESS_MEASUREMENT ") and '"case":"requirement-list-page"' in line:
        m = json.loads(line[len("READ_EGRESS_MEASUREMENT "):])
        print("requirement-list-page:", m["backend"], "candidate", m["candidate_revision"], "baseline", m["baseline_revision"],
              "work_equal", m["work_equal"], "fixture", m["fixture"])
        for counter in ("returned_bytes", "rows", "statements"):
            med = {v: statistics.median(t[counter] for t in m["trials"] if t["variant"] == v) for v in ("baseline", "candidate")}
            print("  %s median: baseline %d, candidate %d" % (counter, med["baseline"], med["candidate"]))
PY
  grep -E -- '^(--- |    --- )(PASS|FAIL|SKIP)|^(ok|FAIL)[[:space:]]' "$run-command.log" || true
done
