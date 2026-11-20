#!/usr/bin/env bash
# Usage: run-proof.sh <label> <git-ref-to-switch-to>
set -euo pipefail
label=$1 ref=$2
D=$HOME/.local/state/conveyor/261120-64ffa9/implement-3
cd /home/user/conveyor-worktrees/conveyor-task-261120-64ffa9
test -z "$(git status --porcelain --untracked-files=all)"
if [ "$ref" = conveyor/task-261120-64ffa9 ]; then git switch -q "$ref"; else git switch -q --detach "$ref"; fi
export CONVEYOR_TASK_CACHE=$HOME/.cache/conveyor/261120-64ffa9
export GOCACHE=$CONVEYOR_TASK_CACHE/go-build GOTMPDIR=$CONVEYOR_TASK_CACHE/go-tmp TMPDIR=$CONVEYOR_TASK_CACHE/tmp
pw=$(sed -n 's/^ROOT_PASSWORD=//p' "$HOME/.config/conveyor-s2/env")
export CONVEYOR_TEST_SINGLESTORE_URL="root:${pw}@tcp(127.0.0.1:3306)/conveyor_test"
unset pw CONVEYOR_TEST_DATABASE_URL CONVEYOR_TASK_POLL_REPORT_ONLY GOFLAGS
echo "label=$label ref=$ref head=$(git rev-parse HEAD) started=$(date -u +%FT%TZ)"
set +e
python3 scripts/validation_evidence.py run --policy "$D/policy.json" --timeout 1800 2>&1 | tee "$D/$label.recorder.out"
status=${PIPESTATUS[0]}
echo "recorder exit=$status finished=$(date -u +%FT%TZ)" | tee -a "$D/$label.recorder.out"
