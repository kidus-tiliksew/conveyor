#!/bin/sh
# Checks file integrity and the recorded outcomes of both runs.
set -eu
sha256sum -c SHA256SUMS
grep -q -- '--- FAIL: TestMemoryConformance/Verification/EvidenceBinding/BindingMismatch/workspace ' fails-before-fix.log
grep -q '^exit=1$' fails-before-fix.log
grep -q -- '--- PASS: TestMemoryConformance/Verification/EvidenceBinding/BindingMismatch/workspace ' passes-after-fix.log
grep -q -- '--- PASS: TestVerificationEvidenceBindingRefusals' passes-after-fix.log
test "$(grep -c '^exit=0$' passes-after-fix.log)" -eq 2
echo "evidence verified"
