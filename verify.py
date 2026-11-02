#!/usr/bin/env python3
"""Check the retained evidence for Conveyor task 261101-9c939d.

Run from the root of a checkout of the evidence/261101-9c939d branch:
    python3 -I verify.py
Standard library only. It recomputes every SHA256SUMS entry, then checks for
each accepted run that the manifest records a successful, complete fresh
execution of the exact command at the reviewed head, that the complete log
matches the manifest's digest and size and reports 300 passed with no failure,
and that the head binding names the same head and this manifest's digest.
"""
import hashlib
import json
import re
import sys
from pathlib import Path

HEAD = "3f2d7a504eebb66688d149f95a5836aee37bd956"
ACCEPTED = {
    "runs/attempt-20261102T041733Z-89938-d0f9bccd4e47": (
        "policies/repeat20-workers2.json",
        ["make", "test-web", "PLAYWRIGHT_ARGS=tests/board-filters.spec.ts --repeat-each=20"],
    ),
    "runs/attempt-20261102T041424Z-77911-4336e6b19e08": (
        "policies/repeat20-workers4.json",
        ["make", "test-web", "PLAYWRIGHT_WORKERS=4", "PLAYWRIGHT_ARGS=tests/board-filters.spec.ts --repeat-each=20"],
    ),
}
root = Path(__file__).resolve().parent
failures = []


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def need(ok, message):
    print(("ok    " if ok else "FAIL  ") + message)
    if not ok:
        failures.append(message)


for line in (root / "SHA256SUMS").read_text().splitlines():
    digest, name = line.split("  ", 1)
    need((root / name).is_file() and sha(root / name) == digest, f"sha256 {name}")

for run, (policy_name, command) in ACCEPTED.items():
    folder = root / run
    record = json.loads((folder / "manifest.json").read_text())["record"]
    log = folder / "command.log"
    text = log.read_text(errors="replace")
    need(record["kind"] == "fresh-execution" and record["outcome"] == "success", f"{run}: fresh execution, outcome success")
    need(record["exit_status"] == 0 and record["snapshot_error"] is None, f"{run}: exit status 0, no snapshot error")
    need(record["policy"]["command"] == command, f"{run}: command {command}")
    need(record["policy"] == json.loads((root / policy_name).read_text()), f"{run}: policy equals {policy_name}")
    need(record["before"]["git"]["head"] == HEAD and record["after"]["git"]["head"] == HEAD, f"{run}: before and after head {HEAD}")
    need(record["before"] == record["after"], f"{run}: inputs unchanged before and after")
    need(record["log"]["completeness"] == "complete" and record["log"]["sha256"] == sha(log)
         and record["log"]["bytes"] == log.stat().st_size, f"{run}: complete log matches manifest digest and size")
    need(re.search(r"^\s+300 passed \(", text, re.M) is not None and re.search(r"^\s+\d+ (failed|flaky)", text, re.M) is None,
         f"{run}: log reports 300 passed, none failed or flaky")
    bindings = list(folder.glob("binding-*.json"))
    binding = json.loads(bindings[0].read_text())["record"] if len(bindings) == 1 else {}
    need(binding.get("head") == HEAD and binding.get("branch") == "conveyor/task-261101-9c939d"
         and binding.get("manifest_sha256") == sha(folder / "manifest.json"), f"{run}: binding names head and manifest digest")

print("PASS" if not failures else f"FAIL ({len(failures)})")
sys.exit(1 if failures else 0)
