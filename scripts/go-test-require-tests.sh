#!/usr/bin/env bash
# Runs go test with the given arguments and fails when any selected package ran
# no test. A CI shard whose package list or -run/-skip selection matches
# nothing otherwise passes with "[no tests to run]" or "[no test files]"
# (component-verification-strategy, "CI shards").
set -uo pipefail

log=$(mktemp "${TMPDIR:-/tmp}/go-test-require-tests.XXXXXX")
trap 'rm -f "$log"' EXIT

go test "$@" 2>&1 | tee "$log"
status=${PIPESTATUS[0]}

empty=$(grep -E '^(ok|\?)[[:space:]].*\[no test(s to run| files)\]$' "$log" | awk '{print $2}')
if [ -n "$empty" ]; then
  printf 'go test selected no test in:\n%s\n' "$empty" >&2
  if [ "$status" -eq 0 ]; then
    status=1
  fi
fi
exit "$status"
