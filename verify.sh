#!/bin/sh
# Verify the retained local-only evidence for task 261107-885029.
# Run from a checkout of the evidence/261107-885029 branch.
set -eu
sha256sum -c SHA256SUMS
fail=0
for f in mutation-*.log; do
  grep -q '^# exit: 1$' "$f" && grep -q '^--- FAIL: TestVerificationSuccessGapsAgreeWithEvaluator' "$f" \
    && echo "$f: agreement test fails under the mutation" \
    || { echo "$f: expected a failing agreement test"; fail=1; }
done
n=$(grep -c '^# exit: 0$' item5-tests-pass.log)
[ "$n" -eq 3 ] && ! grep -q -- '--- FAIL' item5-tests-pass.log \
  && echo "item5-tests-pass.log: all three runs pass" \
  || { echo "item5-tests-pass.log: expected three passing runs"; fail=1; }
exit $fail
