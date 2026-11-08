#!/bin/sh
# Checks the retained logs against SHA256SUMS, then counts each regression
# test's PASS and FAIL lines in every log.
set -eu
cd "$(dirname "$0")"
sha256sum -c SHA256SUMS
for log in unfixed-count50-race.log fixed-count50-race.log fixed-count50-race-loaded.log; do
  echo "== $log"
  sed -n '1p;/^# exit status/p' "$log"
  for name in HangupDuringCheckoutCheck HangupDuringLiveCheck TransientRenewalFailure SourceChange RefusedRenewal; do
    pass=$(grep -c -- "--- PASS: TestKitRelayCancel$name " "$log" || true)
    fail=$(grep -c -- "--- FAIL: TestKitRelayCancel$name " "$log" || true)
    echo "TestKitRelayCancel$name pass=$pass fail=$fail"
  done
done
