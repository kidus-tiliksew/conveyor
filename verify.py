"""Offline verification of the retained evidence bundles on this branch.

Usage: python3 -I verify.py --repo <checkout of conveyor at 61f566bb or later>

For every attempt directory it uses the repository's own
scripts/validation_evidence.py read_record to verify the manifest's HMAC
with the bundle key, recomputes the complete log's size and sha256 against
the manifest, and prints the recorded outcome, exit status, head, command,
any snapshot error, and the fields whose before and after snapshots differ.
For each Playwright JSON report it prints the per-run AC-3.2 elapsed times.
It reruns nothing and needs no network.
"""
import argparse, hashlib, importlib.util, json, sys
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("--repo", required=True)
args = parser.parse_args()
sys.path.insert(0, str(Path(args.repo) / "scripts"))
spec = importlib.util.spec_from_file_location("validation_evidence", Path(args.repo) / "scripts/validation_evidence.py")
ve = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ve)
here = Path(__file__).resolve().parent
ok = True
for attempt in sorted(here.glob("*/attempt-*")):
    key = (attempt / "key").read_bytes()
    record = ve.read_record(attempt / "manifest.json", key)  # raises on HMAC mismatch
    log = (attempt / "command.log").read_bytes()
    log_ok = len(log) == record["log"]["bytes"] and hashlib.sha256(log).hexdigest() == record["log"]["sha256"]
    before, after = record.get("before") or {}, record.get("after") or {}
    drift = sorted(f for f in before if before.get(f) != after.get(f)) if after else "no after snapshot"
    if isinstance(drift, list) and "files" in drift:
        changed = sorted(k for k in set(before["files"]) | set(after["files"]) if before["files"].get(k) != after["files"].get(k))
        drift = f"{drift} (files: {changed})"
    print(f"{attempt.parent.name}/{attempt.name}: hmac=valid log_matches_manifest={log_ok} outcome={record['outcome']} "
          f"exit_status={record['exit_status']} head={before.get('git', {}).get('head')} snapshot_error={record.get('snapshot_error')} "
          f"drift={drift} command={record['policy']['command']}")
    ok = ok and log_ok
for report in sorted(here.glob("*/ac32-gate-report*.json")):
    data = json.loads(report.read_bytes())
    runs = []
    def walk(suite):
        for spec in suite.get("specs", []):
            for test in spec["tests"]:
                for result in test["results"]:
                    for note in result.get("annotations", []):
                        if "after read" in note["type"]:
                            runs.append((result["status"], int(note["description"])))
        for child in suite.get("suites", []):
            walk(child)
    for suite in data["suites"]:
        walk(suite)
    values = [v for _, v in runs]
    print(f"{report.parent.name}/{report.name}: runs={len(runs)} passed={sum(s == 'passed' for s, _ in runs)} "
          f"over_1200={sum(v > 1200 for v in values)} min={min(values)} max={max(values)} elapsed_ms={values}")
print("all bundle checks passed" if ok else "BUNDLE CHECK FAILED")
sys.exit(0 if ok else 1)
