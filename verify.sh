#!/bin/sh
# Checks every file against SHA256SUMS, checks each complete log against the
# sha256 its manifest recorded, and prints each run's outcome, fixture
# teardown, resource cleanup, and test results.
set -eu
cd "$(dirname "$0")"
sha256sum -c SHA256SUMS
for run in r1 r2 r3 r4; do
  echo "== $run"
  python3 - "$run" <<'EOF'
import hashlib, json, sys
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
EOF
  grep -E -- '^(--- |    --- )(PASS|FAIL|SKIP)|^(ok|FAIL)[[:space:]]|median=' "$run-command.log" || true
done
