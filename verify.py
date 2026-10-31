"""Offline verification of the retained evidence bundles on this branch.

Usage: python3 -I verify.py --repo <checkout of conveyor at e1d3aded or later>

For each record it uses the repository's own scripts/validation_evidence.py
read_record to verify the manifest's HMAC with the bundle key, recomputes the
complete log's size and sha256 against the manifest, compares the before and
after input snapshots field by field, and prints the recorded outcome, exit
status, head, and command. For the repeat-20 record it also verifies the head
binding's HMAC and that its manifest_sha256 equals the published manifest.
It reruns nothing and needs no network.
"""
import argparse, hashlib, importlib.util, json, sys
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("--repo", required=True)
args = parser.parse_args()
spec = importlib.util.spec_from_file_location("validation_evidence", Path(args.repo) / "scripts/validation_evidence.py")
sys.path.insert(0, str(Path(args.repo) / "scripts"))
ve = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ve)
here = Path(__file__).resolve().parent
ok = True
for name in ("repeat20", "negative-decisions", "negative-inbox"):
    [attempt] = sorted((here / name).glob("attempt-*"))
    key = (attempt / "key").read_bytes()
    record = ve.read_record(attempt / "manifest.json", key)  # raises on HMAC mismatch
    log = (attempt / "command.log").read_bytes()
    log_ok = len(log) == record["log"]["bytes"] and hashlib.sha256(log).hexdigest() == record["log"]["sha256"]
    same = {field: record["before"][field] == record["after"][field] for field in record["before"]}
    policy = json.loads((here / name / f"policy-{name}.json").read_bytes())
    policy_ok = ve.policy_read(here / name / f"policy-{name}.json") == record["policy"] if hasattr(ve, "policy_read") else policy == record["policy"]
    print(f"{name}: hmac=valid log_matches_manifest={log_ok} policy_matches_manifest={policy_ok} "
          f"outcome={record['outcome']} exit_status={record['exit_status']} log_completeness={record['log']['completeness']} "
          f"head={record['before']['git']['head']} before_equals_after={same} "
          f"input_files={len(record['before']['files'])} command={record['policy']['command']}")
    ok = ok and log_ok and policy_ok and all(same.values())
    for binding in sorted(attempt.glob("binding-*.json")):
        bound = ve.read_record(binding, key)
        manifest_sha = hashlib.sha256((attempt / "manifest.json").read_bytes()).hexdigest()
        print(f"  binding: hmac=valid kind={bound['kind']} branch={bound['branch']} head={bound['head']} "
              f"manifest_sha256_matches={bound['manifest_sha256'] == manifest_sha}")
        ok = ok and bound["manifest_sha256"] == manifest_sha
# A negative control's manifest hashes its temporarily edited source file; it
# must differ from the repeat-20 record's hash of the same file, and every
# other worktree file must match.
records = {}
for name in ("repeat20", "negative-decisions", "negative-inbox"):
    [attempt] = sorted((here / name).glob("attempt-*"))
    records[name] = ve.read_record(attempt / "manifest.json", (attempt / "key").read_bytes())["before"]["files"]
edited = {"negative-decisions": "repo/web/src/components/app-shell.tsx",
          "negative-inbox": "repo/web/src/components/inbox/context-proposal-rows.tsx"}
for name, path in edited.items():
    differ = sorted(k for k in set(records[name]) | set(records["repeat20"]) if records[name].get(k) != records["repeat20"].get(k))
    print(f"{name}: files differing from repeat20 = {differ}")
    ok = ok and differ == [path]
print("all bundle checks passed" if ok else "BUNDLE CHECK FAILED")
sys.exit(0 if ok else 1)
