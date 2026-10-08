"""Deterministic evidence and actual Make-graph tests; no network or databases."""
import copy
import functools
from io import StringIO
import itertools
import json
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import validation_evidence as evidence
import validation_resources
from test_validation_resources import FixtureProc

REPO = Path(__file__).resolve().parents[1]


def run(root, *args, env=None):
    return subprocess.run(args, cwd=root, env=env, capture_output=True, text=True, check=True).stdout


# Live cache-user detection and group-member verification read Linux /proc or
# macOS libproc; hosts with neither refuse instead (component-verification-strategy).
HAS_BACKEND = validation_resources.process_backend() != validation_resources.UNAVAILABLE
NO_BACKEND = "no process backend on this host (Linux /proc or macOS libproc)"


def _child_pids(parent_pid):
    """Return the ps listing and the PIDs whose parent is parent_pid, without /proc."""
    listing = subprocess.run(["ps", "-A", "-o", "pid=,ppid="], capture_output=True, text=True, check=True).stdout
    children = []
    for line in listing.splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[1] == str(parent_pid):
            children.append(int(fields[0]))
    return listing, children


def _pid_alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    # An exited, unreaped process is not alive.
    if validation_resources.process_backend() == validation_resources.DARWIN_BACKEND:
        return validation_resources.darwin().state(pid) != "zombie"
    try:
        return Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[0] != "Z"
    except OSError:
        return True


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        # The helper resolves output paths; macOS temp dirs live behind /var -> /private/var.
        self.base = Path(self.tmp.name).resolve()
        self.root = self.base / "repo"
        self.root.mkdir()
        run(self.root, "git", "init", "-b", "task")
        run(self.root, "git", "config", "user.name", "Fixture")
        run(self.root, "git", "config", "user.email", "fixture@example.invalid")
        (self.root / "Makefile").write_text(".PHONY: check\ncheck:\n\t@cat source\n")
        (self.root / "source").write_text("before\n")
        (self.root / "generated").write_text("bundle\n")
        (self.root / "package-lock.json").write_text("resolved dependency\n")
        (self.root / ".gitignore").write_text("ignored-input\n")
        self.commit()
        (self.root / "source").write_text("tested\n")
        (self.root / "new-fixture").write_text("untracked tested input\n")
        (self.root / "ignored-input").write_text("ignored but relevant\n")
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.base), "LC_ALL": "C", "CONFIG": "first", "API_TOKEN": "private-value"}
        self.patch = patch.dict(os.environ, self.env, clear=True)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        # Invocation temporary children follow HOME's cache; accept a RAM-backed
        # test host explicitly. This is helper metadata, not a policy input.
        os.environ[validation_resources.ALLOW_RAM_TMP] = "1"
        self.policy = {"schema": 1, "task": "fixture-task", "layer": "local", "command": ["make", "check"],
                       "environment": list(self.env), "tools": {"make": ["make", "--version"], "git": ["git", "--version"],
                       "python3": ["python3", "--version"], "sh": ["sh", "-c", "printf POSIX-shell"]},
                       "external_inputs": [], "exclude": {},
                       "audit": {"inputs_complete": True, "input_rationale": "Fixture only cats source; all files captured; shell and cat runtime inventoried by test host.",
                                 "git_metadata": "independent", "git_rationale": "Fixture recipe only cats source and never consults Git."}, "backend": None}
        self.output = self.base / "durable"

    def commit(self, message="fixture"):
        run(self.root, "git", "add", ".")
        run(self.root, "git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", message)

    def record(self):
        self.assertEqual(evidence.record(self.root, self.policy, self.output), 0)

    def write_policy(self):
        path = self.base / "policy.json"
        path.write_text(json.dumps(self.policy))
        return path

    def refused(self, reason=None):
        with self.assertRaisesRegex(evidence.Refused, reason or "."):
            evidence.check(self.root, self.policy, self.output)

    def test_commit_identical_inputs_bind_actual_pushed_head(self):
        remote = self.base / "remote.git"
        run(self.root, "git", "init", "--bare", str(remote))
        run(self.root, "git", "remote", "add", "origin", str(remote))
        self.record()
        self.commit("commit tested content")
        evidence.check(self.root, self.policy, self.output)
        with self.assertRaises(evidence.Refused):
            evidence.bind(self.root, self.policy, self.output, "origin", "task")
        run(self.root, "git", "push", "origin", "task")
        head = evidence.bind(self.root, self.policy, self.output, "origin", "task")
        binding = evidence.read_record(self.output / ("binding-" + head + ".json"), (self.output / "key").read_bytes())
        self.assertEqual(binding["kind"], "reused-execution")
        self.assertIn("neither CI nor review", binding["reason"])
        self.assertEqual((self.output / "command.log").read_text(), "tested\n")

    def test_git_dependent_commit_invalidates_version_and_history(self):
        (self.root / "Makefile").write_text("VERSION := $(shell git describe --tags --always --dirty)\ncheck:\n\t@echo $(VERSION)\n")
        self.policy["audit"]["git_metadata"] = "dependent"
        self.record()
        self.commit()
        self.refused("Git metadata")

    def test_changed_files(self):
        self.record()
        for filename in ("source", "generated", "new-fixture", "ignored-input", "package-lock.json"):
            with self.subTest(filename=filename):
                path = self.root / filename
                previous = path.read_bytes()
                path.write_bytes(previous + b"changed")
                self.refused("files")
                path.write_bytes(previous)
        (self.root / "new-input").write_text("new")
        self.refused("files")

    def test_environment_command_tool_policy_and_external_config(self):
        external = self.base / "runtime-config"
        external.write_text("config")
        self.policy["external_inputs"] = [str(external)]
        self.record()
        os.environ["CONFIG"] = "changed"
        self.refused("environment")
        os.environ["CONFIG"] = "first"
        self.policy["command"].append("FLAG=changed")
        self.refused("policy")
        self.policy["command"].pop()
        self.policy["task"] = "another-task"
        self.refused("policy")
        self.policy["task"] = "fixture-task"
        external.write_text("changed config")
        self.refused("files")

    def test_missing_log_key_snapshot_and_mode_changes(self):
        self.record()
        source = self.root / "source"
        source.chmod(0o755)
        self.refused("files")
        source.chmod(0o644)
        log = self.output / "command.log"
        data = log.read_bytes()
        log.unlink()
        with self.assertRaisesRegex(evidence.Refused, "missing durable log"):
            evidence.check(self.root, self.policy, self.output)
        log.write_bytes(data)
        key = (self.output / "key").read_bytes()
        manifest = self.output / "manifest.json"
        original = evidence.read_record(manifest, key)
        for field in ("before", "after"):
            changed = copy.deepcopy(original)
            changed[field] = None
            manifest.unlink()
            evidence.write_record(manifest, changed, key)
            self.refused("snapshot")
        (self.output / "key").unlink()
        with self.assertRaises(OSError):
            evidence.check(self.root, self.policy, self.output)

    def test_backend_probe_detects_identity_version_and_configuration_changes(self):
        state = self.base / "database-state.json"
        values = dict(identity="server", version="v1", configuration="isolated", instance="fresh")
        state.write_text(json.dumps(values))
        self.policy["layer"] = "postgres"
        self.policy["backend"] = {"isolation": "disposable-per-run", "probe": ["python3", "-c",
            "from pathlib import Path; print(Path(" + repr(str(state)) + ").read_text())"]}
        self.record()
        for field in values:
            with self.subTest(field=field):
                changed = dict(values, **{field: "changed"})
                state.write_text(json.dumps(changed))
                self.refused("changed backend")
        state.write_text(json.dumps(values))

    def test_replaced_resolved_tool(self):
        tool = self.base / "tool"
        tool.mkdir()
        script = tool / "probe"
        script.write_text("#!/bin/sh\necho v1\n")
        script.chmod(0o755)
        os.environ["PATH"] = str(tool) + os.pathsep + os.environ["PATH"]
        self.policy["tools"]["probe"] = ["probe"]
        self.record()
        script.write_text("#!/bin/sh\necho v2\n")
        self.refused("tools")

    def test_before_after_drift(self):
        (self.root / "Makefile").write_text("check:\n\t@echo changed > generated\n")
        self.record()
        self.refused("files")

    def test_failure_and_corruption_and_missing_snapshots(self):
        (self.root / "Makefile").write_text("check:\n\t@exit 9\n")
        self.assertNotEqual(evidence.record(self.root, self.policy, self.output), 0)
        self.refused("failed")
        key = (self.output / "key").read_bytes()
        manifest = self.output / "manifest.json"
        r = evidence.read_record(manifest, key)
        self.assertEqual(r["outcome"], "failure")
        self.assertEqual(r["log"]["completeness"], "complete")
        for field in ("before", "after", "exit_status", "log"):
            with self.subTest(field=field):
                changed = copy.deepcopy(r)
                del changed[field]
                manifest.unlink()
                evidence.write_record(manifest, changed, key)
                self.refused("incomplete")
        manifest.write_text('{"record":{},"hmac_sha256":"bad"}')
        self.refused("corrupt")

    def test_corrupt_log(self):
        self.record()
        log = self.output / "command.log"
        contents = log.read_bytes()
        log.write_bytes(contents[:-1])
        self.refused("truncated durable log")
        log.write_bytes(b"x" * len(contents))
        self.refused("corrupt durable log")

    def test_default_output_is_unique_and_prints_retained_references_for_each_outcome(self):
        policy = self.base / "policy.json"
        policy.write_text(json.dumps(self.policy))
        os.environ["XDG_STATE_HOME"] = str(self.base / "state")
        outputs = []
        for expected in (0, 2):
            if expected:
                (self.root / "Makefile").write_text("check:\n\t@exit 9\n")
            stdout = StringIO()
            with patch.object(sys, "argv", ["validation_evidence.py", "run", "--policy", str(policy)]), \
                    patch("sys.stdout", stdout), patch("pathlib.Path.cwd", return_value=self.root):
                self.assertEqual(evidence.main(), expected)
            message = stdout.getvalue().strip()
            self.assertIn("manifest=", message)
            self.assertIn("log=", message)
            self.assertIn("outcome=" + ("success" if expected == 0 else "failure"), message)
            manifest = Path(message.split("manifest=", 1)[1].split(" log=", 1)[0])
            outputs.append(manifest.parent)
            self.assertTrue(manifest.is_file())
            self.assertEqual(manifest.parent.parent, self.base / "state" / "conveyor" / "fixture-task")
        self.assertNotEqual(outputs[0], outputs[1])
        task_cache = self.base / ".cache" / "conveyor" / "fixture-task"
        (task_cache / "tmp").mkdir(parents=True)
        (task_cache / "tmp" / "scratch").write_text("disposable")
        with patch.object(evidence, "active_cache_users", return_value=[]):
            self.assertEqual(evidence.cleanup_cache("fixture-task", task_cache,
                                                   [path / "command.log" for path in outputs]), ["tmp"])
        for output in outputs:
            self.assertTrue((output / "manifest.json").is_file())
            self.assertTrue((output / "command.log").is_file())

    def test_cleanup_refuses_siblings_symlinks_references_and_active_users(self):
        os.environ["XDG_CACHE_HOME"] = str(self.base / "cache-home")
        task_cache = self.base / "cache-home" / "conveyor" / "fixture-task"
        child = task_cache / "tmp"
        child.mkdir(parents=True)
        retained = child / "claimed.log"
        retained.write_text("keep")
        sibling = task_cache.parent / "sibling-task"
        sibling.mkdir()
        with self.assertRaisesRegex(evidence.Refused, "current task child"):
            evidence.cleanup_cache("fixture-task", sibling, [])
        with self.assertRaisesRegex(evidence.Refused, "referenced evidence"):
            evidence.cleanup_cache("fixture-task", task_cache, [retained])
        self.assertTrue(retained.exists())

        process = subprocess.Popen(["sleep", "30"], cwd=child)
        try:
            expected = "disposable cache child is active: tmp" if HAS_BACKEND else "requires /proc"
            with self.assertRaisesRegex(evidence.Refused, expected):
                evidence.cleanup_cache("fixture-task", task_cache, [])
        finally:
            process.terminate()
            process.wait(timeout=5)
        with patch.object(evidence, "active_cache_users", return_value=[]):
            self.assertEqual(evidence.cleanup_cache("fixture-task", task_cache, []), ["tmp"])
        self.assertFalse(child.exists())

        target = self.base / "elsewhere"
        target.mkdir()
        unknown = task_cache / "retained-unknown-child"
        unknown.mkdir()
        guarded = task_cache / "go-build"
        guarded.mkdir()
        (task_cache / "tmp").symlink_to(target, target_is_directory=True)
        with patch.object(evidence, "active_cache_users", return_value=[]):
            with self.assertRaisesRegex(evidence.Refused, "symlink"):
                evidence.cleanup_cache("fixture-task", task_cache, [])
        self.assertTrue(target.exists())
        self.assertTrue(unknown.exists())
        self.assertTrue(guarded.exists())

    @unittest.skipUnless(HAS_BACKEND, NO_BACKEND + "; cleanup refuses without it")
    def test_cleanup_refuses_environment_only_live_cache_user(self):
        os.environ["XDG_CACHE_HOME"] = str(self.base / "cache-home")
        task_cache = self.base / "cache-home" / "conveyor" / "fixture-task"
        child = task_cache / "go-build"
        child.mkdir(parents=True)
        # Python, not /bin/sleep: macOS withholds a platform binary's environment.
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"], cwd=self.base,
                                   env=dict(os.environ, GOCACHE=str(child)))
        try:
            users = evidence.active_cache_users(child)
            self.assertIn(str(process.pid) + ":env:GOCACHE", users)
            with self.assertRaisesRegex(evidence.Refused, "env:GOCACHE"):
                evidence.cleanup_cache("fixture-task", task_cache, [])
        finally:
            process.terminate()
            process.wait(timeout=5)
        with patch.object(evidence, "active_cache_users", return_value=[]):
            self.assertEqual(evidence.cleanup_cache("fixture-task", task_cache, []), ["go-build"])

    def test_active_cache_users_reports_inaccessible_proc_inspection(self):
        proc = self.base / "proc"
        process = proc / "4242"
        (process / "fd").mkdir(parents=True)
        (process / "cwd").symlink_to(self.base, target_is_directory=True)
        (process / "root").symlink_to(Path("/"), target_is_directory=True)
        # A directory cannot be read as environ and models an unreadable proc entry.
        (process / "environ").mkdir()
        users = evidence.active_cache_users(self.base / "cache", proc)
        self.assertIn("4242:ambiguous:environ", users)

    def test_cache_ownership_inspection_refuses_without_proc(self):
        absent = self.base / "absent-proc"
        with self.assertRaisesRegex(evidence.Refused, "requires /proc"):
            evidence.active_cache_users(self.base / "cache", absent)
        unavailable = validation_resources.UNAVAILABLE
        with self.assertRaisesRegex(evidence.Refused, "requires /proc or macOS libproc"):
            evidence.active_cache_users(self.base / "cache", backend=unavailable)
        os.environ["XDG_CACHE_HOME"] = str(self.base / "cache-home")
        task_cache = self.base / "cache-home" / "conveyor" / "fixture-task"
        child = task_cache / "tmp"
        child.mkdir(parents=True)
        without_backend = functools.partial(evidence.active_cache_users, backend=unavailable)
        with patch.object(evidence, "active_cache_users", without_backend):
            with self.assertRaisesRegex(evidence.Refused, "requires /proc"):
                evidence.cleanup_cache("fixture-task", task_cache, [])
        self.assertTrue(child.is_dir())

    def test_authored_conflict_resolution_even_when_content_matches(self):
        self.record()
        self.commit()
        run(self.root, "git", "checkout", "-b", "sibling", "HEAD~1")
        (self.root / "source").write_text("conflicting sibling")
        self.commit("sibling")
        run(self.root, "git", "checkout", "task")
        result = subprocess.run(["git", "merge", "sibling"], cwd=self.root, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.refused("conflict")
        (self.root / "source").write_text("tested\n")
        self.commit("authored resolution")
        self.refused("conflict|history")

    def test_missing_audit_and_unknown_fields(self):
        path = self.base / "policy.json"
        for field in ("tools", "audit", "environment", "external_inputs", "backend"):
            p = copy.deepcopy(self.policy)
            del p[field]
            path.write_text(json.dumps(p))
            with self.assertRaises(evidence.Refused):
                evidence.policy_read(path)
        self.policy["audit"]["inputs_complete"] = False
        path.write_text(json.dumps(self.policy))
        evidence.policy_read(path)
        self.record()
        self.refused("unknown inputs")

    def test_secret_redaction_and_cache_cleanup_durability(self):
        (self.root / "Makefile").write_text("check:\n\t@printf '%s' \"$$API_TOKEN\"\n")
        self.record()
        for path in self.output.glob("*.json"):
            self.assertNotIn("private-value", path.read_text())
        self.assertNotIn("private-value", (self.output / "command.log").read_text())
        cache = self.base / "cache"
        cache.mkdir()
        os.environ["CONVEYOR_TASK_CACHE"] = str(cache)
        with self.assertRaises(evidence.Refused):
            evidence.record(self.root, self.policy, cache / "evidence")
        shutil.rmtree(cache)
        evidence.check(self.root, self.policy, self.output)

    def test_chunked_secret_redaction_is_bounded(self):
        redactor = evidence.Redactor([b"private-value", b"second-secret"])
        retained = bytearray()
        for chunk in (b"prefix private-", b"val", b"ue middle second-", b"secret suffix"):
            retained.extend(redactor.feed(chunk))
            self.assertLessEqual(len(redactor.pending), len(b"private-value") - 1)
        retained.extend(redactor.finish())
        self.assertEqual(bytes(retained), b"prefix [REDACTED] middle [REDACTED] suffix")
        self.assertNotIn(b"private-value", retained)

    def test_snapshot_failure_is_final_and_command_does_not_run(self):
        marker = self.base / "command-ran"
        (self.root / "Makefile").write_text("check:\n\t@touch " + str(marker) + "\n")
        with patch.object(evidence, "snapshot", side_effect=evidence.Refused("fixture snapshot failed")):
            self.assertEqual(evidence.record(self.root, self.policy, self.output), 2)
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["state"], "complete")
        self.assertEqual(record["outcome"], "snapshot-failure")
        self.assertIn("before: fixture snapshot failed", record["snapshot_error"])
        self.assertFalse(marker.exists())
        self.refused("invalid execution outcome")

    def test_after_snapshot_failure_stays_distinct_from_command_success(self):
        real_snapshot = evidence.snapshot
        calls = itertools.count()

        def snapshot(root, policy, key, runtime_env=None):
            if next(calls) == 0:
                return real_snapshot(root, policy, key)
            raise evidence.Refused("after snapshot failed")

        with patch.object(evidence, "snapshot", side_effect=snapshot):
            self.assertEqual(evidence.record(self.root, self.policy, self.output), 2)
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["state"], "complete")
        self.assertEqual(record["outcome"], "snapshot-failure")
        self.assertEqual(record["exit_status"], 0)
        self.assertIn("after: after snapshot failed", record["snapshot_error"])
        self.refused("invalid execution outcome")

    def test_fixture_lifecycle_wraps_snapshots_and_gate_then_tears_down(self):
        self.policy["layer"] = "postgres"
        self.policy["environment"].append("CONVEYOR_TEST_DATABASE_URL")
        self.policy["backend"] = {"isolation": "disposable-per-run", "probe": ["python3", "-c", 'print("{}")']}
        self.policy["fixture"] = {
            "backend": "postgres", "url_env": "CONVEYOR_TEST_DATABASE_URL",
            "prepared_url_env": "CONVEYOR_TEST_DATABASE_URL",
            "external_network_env": "CONVEYOR_TEST_EXTERNAL_NETWORK",
            "database_prefix": "conveyor", "minimum_free_bytes": 1, "timeout": "1s",
        }
        os.environ["CONVEYOR_TEST_DATABASE_URL"] = "postgres://root:secret@db/conveyor_test"
        order = []
        ownership = {"backend": "postgres", "database": "conveyor_a1_test", "endpoint": "db:5432"}

        def prepare(*_args, **_kwargs):
            order.append("prepare")
            return ownership, evidence.environment(self.policy)

        def snapshot(*_args):
            order.append("snapshot")
            return {"files": {}, "environment": {}, "tools": {}, "backend": {}, "git": {}, "runtime": {}}

        def teardown(*_args, **_kwargs):
            order.append("teardown")

        with patch.object(evidence.validation_fixtures, "prepare", side_effect=prepare), \
             patch.object(evidence.validation_fixtures, "teardown", side_effect=teardown), \
             patch.object(evidence, "snapshot", side_effect=snapshot):
            self.assertEqual(evidence.record(self.root, self.policy, self.output), 0)
        self.assertEqual(order, ["prepare", "snapshot", "snapshot", "teardown"])
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["outcome"], "success")
        self.assertEqual(record["fixture"]["before_snapshot_outcome"], "success")
        self.assertEqual(record["fixture"]["after_snapshot_outcome"], "success")
        self.assertEqual(record["fixture"]["teardown_outcome"], "success")

    def test_fixture_metadata_is_not_redacted_from_the_retained_log(self):
        self.policy["layer"] = "postgres"
        self.policy["environment"].append("CONVEYOR_TEST_DATABASE_URL")
        self.policy["backend"] = {"isolation": "disposable-per-run", "probe": ["python3", "-c", 'print("{}")']}
        self.policy["fixture"] = {"backend": "postgres", "url_env": "CONVEYOR_TEST_DATABASE_URL",
                                  "prepared_url_env": "CONVEYOR_TEST_DATABASE_URL",
                                  "external_network_env": "CONVEYOR_TEST_EXTERNAL_NETWORK",
                                  "database_prefix": "conveyor", "minimum_free_bytes": 1, "timeout": "1s"}
        os.environ["CONVEYOR_TEST_DATABASE_URL"] = "postgres://root:secret-value@db/conveyor_test"
        (self.root / "Makefile").write_text('check:\n\t@echo "ok 1.10s $$CONVEYOR_TEST_DATABASE_URL"\n')
        prepared = dict(evidence.environment(self.policy), CONVEYOR_FIXTURE_PREPARED="1",
                        CONVEYOR_FIXTURE_OWNERSHIP=str(self.base / "ownership.json"))
        ownership = {"backend": "postgres", "database": "conveyor_a1_test", "endpoint": "db:5432"}
        with patch.object(evidence.validation_fixtures, "prepare", return_value=(ownership, prepared)), \
             patch.object(evidence.validation_fixtures, "teardown"), \
             patch.object(evidence, "snapshot", return_value={"files": {}, "environment": {}, "tools": {},
                                                              "backend": {}, "git": {}, "runtime": {}}):
            self.assertEqual(evidence.record(self.root, self.policy, self.output), 0)
        retained = (self.output / "command.log").read_text()
        self.assertIn("ok 1.10s [REDACTED]", retained)
        self.assertNotIn("secret-value", retained)

    def test_before_snapshot_failure_attempts_after_and_durably_orders_phases(self):
        self.policy["layer"] = "postgres"
        self.policy["fixture"] = {"backend": "postgres"}
        ownership = {"backend": "postgres", "database": "conveyor_a1_test", "endpoint": "db:5432"}
        after = {"files": {}, "environment": {}, "tools": {}, "backend": {"retained": True}, "git": {}, "runtime": {}}
        order = []

        def prepare(*args, **_kwargs):
            evidence.validation_fixtures._phase(args[-1], "prepare", "success")
            order.append("prepare")
            return ownership, evidence.environment(self.policy)

        def snapshot(*args):
            order.append("snapshot")
            if order.count("snapshot") == 1:
                raise evidence.Refused("injected before failure")
            return after

        def teardown(*args, **_kwargs):
            order.append("teardown")
            evidence.validation_fixtures._phase(args[-1], "teardown", "success")

        marker = self.base / "gate-ran"
        (self.root / "Makefile").write_text("check:\n\t@touch " + str(marker) + "\n")
        with patch.object(evidence.validation_fixtures, "prepare", side_effect=prepare), \
             patch.object(evidence.validation_fixtures, "teardown", side_effect=teardown), \
             patch.object(evidence, "snapshot", side_effect=snapshot):
            self.assertEqual(evidence.record(self.root, self.policy, self.output), 2)
        self.assertFalse(marker.exists())
        self.assertEqual(order, ["prepare", "snapshot", "snapshot", "teardown"])
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["outcome"], "snapshot-failure")
        self.assertIn("before: injected before failure", record["snapshot_error"])
        self.assertEqual(record["after"], after)
        self.assertEqual(record["fixture"]["after_snapshot_outcome"], "success")
        phases = [json.loads(line) for line in (self.output / "fixture" / "phases.jsonl").read_text().splitlines()]
        self.assertEqual([(p["phase"], p["outcome"]) for p in phases], [
            ("prepare", "success"), ("before-snapshot", "started"), ("before-snapshot", "failure"),
            ("gate", "not-run"), ("after-snapshot", "started"), ("after-snapshot", "success"),
            ("teardown", "success"),
        ])

    def test_fixture_failure_is_not_command_failure_or_skipped_coverage(self):
        marker = self.base / "command-ran"
        (self.root / "Makefile").write_text("check:\n\t@touch " + str(marker) + "\n")
        self.policy["layer"] = "postgres"
        self.policy["environment"].append("CONVEYOR_TEST_DATABASE_URL")
        self.policy["backend"] = {"isolation": "disposable-per-run", "probe": ["python3", "-c", 'print("{}")']}
        self.policy["fixture"] = {
            "backend": "postgres", "url_env": "CONVEYOR_TEST_DATABASE_URL",
            "prepared_url_env": "CONVEYOR_TEST_DATABASE_URL",
            "external_network_env": "CONVEYOR_TEST_EXTERNAL_NETWORK",
            "database_prefix": "conveyor", "minimum_free_bytes": 1, "timeout": "1s",
        }
        os.environ["CONVEYOR_TEST_DATABASE_URL"] = "postgres://db/conveyor_test"
        with patch.object(evidence.validation_fixtures, "prepare", side_effect=evidence.validation_fixtures.FixtureError("capacity unavailable")):
            self.assertEqual(evidence.record(self.root, self.policy, self.output), 2)
        self.assertFalse(marker.exists())
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["outcome"], "fixture-failure")
        self.assertIn("capacity unavailable", record["fixture_error"])

    def test_teardown_failure_keeps_gate_outcome_but_fails_the_run(self):
        self.policy["layer"] = "postgres"
        self.policy["environment"].append("CONVEYOR_TEST_DATABASE_URL")
        self.policy["backend"] = {"isolation": "disposable-per-run", "probe": ["python3", "-c", 'print("{}")']}
        self.policy["fixture"] = {
            "backend": "postgres", "url_env": "CONVEYOR_TEST_DATABASE_URL",
            "prepared_url_env": "CONVEYOR_TEST_DATABASE_URL",
            "external_network_env": "CONVEYOR_TEST_EXTERNAL_NETWORK",
            "database_prefix": "conveyor", "minimum_free_bytes": 1, "timeout": "1s",
        }
        os.environ["CONVEYOR_TEST_DATABASE_URL"] = "postgres://db/conveyor_test"
        ownership = {"backend": "postgres", "database": "conveyor_a1_test", "endpoint": "db:5432"}
        with patch.object(evidence.validation_fixtures, "prepare", return_value=(ownership, evidence.environment(self.policy))), \
             patch.object(evidence.validation_fixtures, "teardown", side_effect=evidence.validation_fixtures.FixtureError("owned drop failed")), \
             patch.object(evidence, "snapshot", return_value={"files": {}, "environment": {}, "tools": {}, "backend": {}, "git": {}, "runtime": {}}):
            self.assertEqual(evidence.record(self.root, self.policy, self.output), 2)
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["outcome"], "success")
        self.assertEqual(record["fixture"]["teardown_outcome"], "failure")
        self.assertEqual(evidence.inspect_record(self.root, self.output)["classification"], "teardown-failure-after-success")

    def test_inspect_reports_crash_left_incomplete_without_replay(self):
        (self.root / "Makefile").write_text("check:\n\t@sleep 30\n")
        policy = self.write_policy()
        command = [sys.executable, str(REPO / "scripts" / "validation_evidence.py"), "run",
                   "--policy", str(policy), "--output", str(self.output)]
        process = subprocess.Popen(command, cwd=self.root, env=dict(os.environ),
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        deadline = time.time() + 5
        child_group = None
        last_record = None
        last_children = None
        while time.time() < deadline:
            if (self.output / "manifest.json").is_file():
                key = (self.output / "key").read_bytes()
                last_record = evidence.read_record(self.output / "manifest.json", key)
            if last_record and last_record["state"] == "running":
                last_children, children = _child_pids(process.pid)
                if children:
                    candidate = children[0]
                    try:
                        if os.getpgid(candidate) == candidate:
                            child_group = candidate
                            break
                    except ProcessLookupError:
                        pass
            time.sleep(0.02)
        if child_group is None:
            process.kill()
            stdout, stderr = process.communicate(timeout=5)
            self.fail("runner did not launch a ready child process group within 5 seconds; "
                      f"record={last_record!r}, children={last_children!r}, "
                      f"stdout={stdout!r}, stderr={stderr!r}, returncode={process.returncode}")
        process.kill()
        process.communicate(timeout=5)
        inspected = evidence.inspect_record(self.root, self.output)
        self.assertEqual(inspected["classification"], "abandoned-or-incomplete")
        self.assertFalse(inspected["reusable"])
        self.refused("abandoned/incomplete")
        # SIGKILL cannot run the runner's group teardown. Explicit recovery of
        # the named invocation stops the orphaned group and leaves the
        # evidence incomplete and nonreusable.
        self.addCleanup(lambda: _pid_alive(child_group) and os.killpg(child_group, signal.SIGKILL))
        invocation = Path(last_record["invocation"])
        self.assertEqual({r["classification"] for r in validation_resources.inspect_invocation(invocation)["resources"]},
                         {"abandoned"})
        complete, actions = validation_resources.recover(invocation, [str(self.output)], grace=0.3)
        self.assertTrue(complete, actions)
        deadline = time.time() + 2
        while time.time() < deadline and _pid_alive(child_group):
            time.sleep(0.02)
        self.assertFalse(_pid_alive(child_group), "recovery did not stop the orphaned gate")
        self.assertEqual(evidence.inspect_record(self.root, self.output)["classification"], "abandoned-or-incomplete")
        self.assertTrue((self.output / "command.log").is_file())
        self.refused("abandoned/incomplete")

    def joined_owner(self):
        """An owner with a custom disk temporary root and a runner bound to it."""
        os.environ[validation_resources.TMP_ROOT] = str(self.base / "custom-disposable")
        owner = validation_resources.Invocation.create("fixture-task", self.root, ["make", "check"])
        self.addCleanup(lambda: owner.owner and owner.finish())
        os.environ[validation_resources.BINDING] = str(owner.path)
        return owner

    def test_evidence_inside_any_disposable_root_is_refused_before_creation(self):
        owner = self.joined_owner()
        tmp = Path(owner.managed_env()["TMPDIR"])
        # The configured root is known to this environment.
        with self.assertRaisesRegex(evidence.Refused, "disposable validation temporary root"):
            evidence.record(self.root, self.policy, tmp / "retained-evidence")
        # A joined runner may not know the owner's root; the bound inventory does.
        del os.environ[validation_resources.TMP_ROOT]
        with self.assertRaisesRegex(evidence.Refused, "cannot live in disposable path"):
            evidence.record(self.root, self.policy, tmp / "retained-evidence")
        self.assertFalse((tmp / "retained-evidence").exists())
        inventory = validation_resources.load_inventory(owner.path)
        self.assertEqual(inventory["references"], [])
        self.assertTrue(validation_resources.owner_active(owner.path, inventory), "a joined refusal ended its owner")
        self.assertEqual(owner.finish(), [])
        self.assertFalse(tmp.exists())

    def test_joined_runner_records_its_reference_and_owner_cleanup_keeps_evidence(self):
        owner = self.joined_owner()
        self.record()
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["invocation"], str(owner.path))
        self.assertEqual(validation_resources.load_inventory(owner.path)["references"], [str(self.output)])
        retained = {name: (self.output / name).read_bytes() for name in ("manifest.json", "command.log", "key")}
        self.assertEqual(owner.finish(outcome="success"), [])
        self.assertEqual({name: (self.output / name).read_bytes() for name in retained}, retained)
        self.assertFalse(Path(owner.managed_env()["TMPDIR"]).exists())
        evidence.check(self.root, self.policy, self.output)

    def test_default_recovery_after_owner_sigkill_keeps_joined_incomplete_evidence(self):
        (self.root / "Makefile").write_text(".PHONY: check\ncheck:\n\t@sleep 30\n")
        policy = self.write_policy()
        info = self.base / "owner.json"
        os.environ[validation_resources.TMP_ROOT] = str(self.base / "custom-disposable")
        # The owner and its joined evidence runner share one process, as in a
        # wrapper that binds the runner; SIGKILL ends both without cleanup.
        child = "\n".join([
            "import json, os, pathlib, sys",
            "sys.path.insert(0, sys.argv[1])",
            "import validation_evidence as e, validation_resources as r",
            "root, policy, output, info = (pathlib.Path(value) for value in sys.argv[2:6])",
            "owner = r.Invocation.create('fixture-task', root, ['make', 'check'])",
            "os.environ[r.BINDING] = str(owner.path)",
            "info.write_text(json.dumps({'invocation': str(owner.path), 'tmp': owner.managed_env()['TMPDIR']}))",
            "sys.exit(e.record(root, json.loads(policy.read_text()), output))",
        ])
        process = subprocess.Popen([sys.executable, "-c", child, str(REPO / "scripts"), str(self.root), str(policy),
                                    str(self.output), str(info)], cwd=self.root, env=dict(os.environ),
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: process.poll() is None and process.kill())
        deadline = time.time() + 10
        group = None
        while time.time() < deadline and group is None:
            if (self.output / "manifest.json").is_file():
                record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
                inventory = validation_resources.load_inventory(Path(record["invocation"]))
                groups = [entry["identity"]["pgid"] for entry in inventory["resources"]
                          if entry["kind"] == "process-group" and entry["state"] == "sealed"]
                if record["state"] == "running" and groups:
                    group = groups[0]
            time.sleep(0.02)
        self.assertIsNotNone(group, "evidence gate did not reach a sealed running group")
        self.addCleanup(lambda: _pid_alive(group) and os.killpg(group, signal.SIGKILL))
        process.kill()
        process.wait(timeout=5)
        details = json.loads(info.read_text())
        invocation = Path(details["invocation"])
        self.assertEqual(validation_resources.load_inventory(invocation)["references"], [str(self.output)])
        retained = {name: (self.output / name).read_bytes() for name in ("manifest.json", "command.log", "key")}
        # The default recovery command names only the invocation.
        result = subprocess.run([sys.executable, str(REPO / "scripts" / "validation_resources.py"), "recover",
                                 "--invocation", str(invocation), "--grace", "0.3"],
                                env=dict(os.environ), capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        deadline = time.time() + 2
        while time.time() < deadline and _pid_alive(group):
            time.sleep(0.02)
        self.assertFalse(_pid_alive(group), "recovery did not stop the orphaned gate")
        self.assertFalse(Path(details["tmp"]).exists(), "recovery left the verified temporary child")
        self.assertEqual({name: (self.output / name).read_bytes() for name in retained}, retained)
        self.assertEqual(evidence.inspect_record(self.root, self.output)["classification"], "abandoned-or-incomplete")
        self.assertFalse(evidence.inspect_record(self.root, self.output)["reusable"])
        self.refused("abandoned/incomplete")

    def test_catchable_cancellation_reaps_descendant_and_retains_redacted_output(self):
        pid_file = self.base / "descendant.pid"
        child = self.root / "child.py"
        child.write_text("""import pathlib, subprocess, sys, time
p = subprocess.Popen([\"sleep\", \"30\"])
pathlib.Path(sys.argv[1]).write_text(str(p.pid))
sys.stdout.write(\"private-\"); sys.stdout.flush()
time.sleep(0.2)
sys.stdout.write(\"value\\n\"); sys.stdout.flush()
time.sleep(30)
""")
        (self.root / "Makefile").write_text("check:\n\t@python3 child.py " + str(pid_file) + "\n")
        self.policy["environment"].append("PID_FILE")
        policy = self.write_policy()
        command = [sys.executable, str(REPO / "scripts" / "validation_evidence.py"), "run",
                   "--policy", str(policy), "--output", str(self.output)]
        process = subprocess.Popen(command, cwd=self.root, env=dict(os.environ),
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        deadline = time.time() + 5
        while time.time() < deadline and not pid_file.is_file():
            time.sleep(0.02)
        self.assertTrue(pid_file.is_file(), "descendant was not launched")
        time.sleep(0.3)
        os.kill(process.pid, signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=8)
        self.assertEqual(process.returncode, 128 + signal.SIGTERM, stderr)
        descendant = int(pid_file.read_text())
        deadline = time.time() + 2
        while time.time() < deadline and _pid_alive(descendant):
            time.sleep(0.02)
        self.assertFalse(_pid_alive(descendant), "descendant was not reaped")
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["outcome"], "interrupted")
        self.assertEqual(record["interruption"], {"signal": signal.SIGTERM})
        retained = (self.output / "command.log").read_text()
        self.assertNotIn("private-value", retained)
        self.assertIn("[REDACTED]", retained)
        self.assertIn("outcome=interrupted", stdout)
        self.refused("invalid execution outcome")

    def test_timeout_stops_the_supervised_group_and_is_not_reusable(self):
        (self.root / "Makefile").write_text("check:\n\t@echo started; sleep 30\n")
        started = time.monotonic()
        self.assertEqual(evidence.record(self.root, self.policy, self.output, timeout=0.5), 124)
        self.assertLess(time.monotonic() - started, 20)
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual((record["outcome"], record["timeout"]), ("timeout", 0.5))
        self.assertEqual(record["resource_cleanup"]["outcome"], "success")
        self.assertIn("started", (self.output / "command.log").read_text())
        self.assertEqual(evidence.inspect_record(self.root, self.output)["classification"], "timeout")
        self.refused("invalid execution outcome")

    def _run_bounded(self, *extra, limit=30):
        """Run the evidence entrypoint; a supervision regression fails instead of hanging the suite."""
        policy = self.write_policy()
        command = [sys.executable, str(REPO / "scripts" / "validation_evidence.py"), "run",
                   "--policy", str(policy), "--output", str(self.output), *extra]
        started = time.monotonic()
        process = subprocess.Popen(command, cwd=self.root, env=dict(os.environ),
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            stdout, stderr = process.communicate(timeout=limit)
        except subprocess.TimeoutExpired:
            process.kill()
            stdout, stderr = process.communicate()
            self.fail(f"evidence run did not finish within {limit}s: {stdout}{stderr}")
        return process.returncode, stdout, stderr, time.monotonic() - started

    def _descendant_holds_output(self, exit_code):
        """make check exits while a descendant that inherited stdout keeps running."""
        pid_file = self.base / "descendant.pid"
        (self.root / "child.py").write_text(f"""import pathlib, subprocess, sys
p = subprocess.Popen([\"sleep\", \"60\"])
pathlib.Path(sys.argv[1]).write_text(str(p.pid))
print(\"direct command done\", flush=True)
sys.exit({exit_code})
""")
        (self.root / "Makefile").write_text("check:\n\t@python3 child.py " + str(pid_file) + "\n")
        self.addCleanup(lambda: pid_file.is_file() and _pid_alive(int(pid_file.read_text()))
                        and os.kill(int(pid_file.read_text()), signal.SIGKILL))
        return pid_file

    def _assert_reaped(self, pid_file):
        self.assertTrue(pid_file.is_file(), "descendant was not launched")
        self.assertFalse(_pid_alive(int(pid_file.read_text())), "descendant holding the output survived")

    @unittest.skipUnless(HAS_BACKEND, NO_BACKEND + "; without it the supervisor refuses to signal survivors")
    def test_successful_command_reaps_descendant_holding_output_and_keeps_status(self):
        pid_file = self._descendant_holds_output(0)
        status, stdout, stderr, elapsed = self._run_bounded()
        self.assertEqual(status, 0, stderr)
        self.assertLess(elapsed, 25)
        self._assert_reaped(pid_file)
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual((record["outcome"], record["exit_status"], record["interruption"]), ("success", 0, None))
        self.assertEqual(record["resource_cleanup"]["outcome"], "success")
        self.assertEqual(record["log"]["completeness"], "complete")
        self.assertIn("direct command done", (self.output / "command.log").read_text())
        self.assertIn("outcome=success", stdout)
        evidence.check(self.root, self.policy, self.output)

    @unittest.skipUnless(HAS_BACKEND, NO_BACKEND + "; without it the supervisor refuses to signal survivors")
    def test_failed_command_reaps_descendant_holding_output_and_keeps_status(self):
        pid_file = self._descendant_holds_output(3)
        status, stdout, stderr, elapsed = self._run_bounded()
        self.assertEqual(status, 2, stderr)  # Make's own status for a failed recipe.
        self.assertLess(elapsed, 25)
        self._assert_reaped(pid_file)
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual((record["outcome"], record["exit_status"]), ("failure", 2))
        self.assertEqual(record["resource_cleanup"]["outcome"], "success")
        self.assertIn("direct command done", (self.output / "command.log").read_text())
        self.refused("failed/incomplete execution")

    def test_timeout_is_enforced_while_output_stays_readable(self):
        # The producer outpaces the redacting reader, so the pipe never drains.
        (self.root / "chatter.py").write_text("""import sys
burst = b"tick\\n" * 16384
while True:
    sys.stdout.buffer.write(burst)
    sys.stdout.flush()
""")
        (self.root / "Makefile").write_text("check:\n\t@python3 chatter.py\n")
        status, stdout, stderr, elapsed = self._run_bounded("--timeout", "0.5")
        self.assertEqual(status, 124, stderr)
        self.assertLess(elapsed, 25)
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual((record["outcome"], record["timeout"]), ("timeout", 0.5))
        self.assertEqual(record["resource_cleanup"]["outcome"], "success")
        self.assertEqual(record["log"]["completeness"], "complete")
        self.assertIn("tick", (self.output / "command.log").read_text())
        self.refused("invalid execution outcome")

    def test_command_that_closes_output_is_still_awaited(self):
        (self.root / "Makefile").write_text("check:\n\t@echo closing; exec >/dev/null 2>&1; sleep 1; exit 0\n")
        self.record()
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual((record["outcome"], record["exit_status"]), ("success", 0))
        self.assertIn("closing", (self.output / "command.log").read_text())

    def test_resource_cleanup_failure_is_recorded_beside_the_gate_outcome(self):
        with patch.object(validation_resources.Invocation, "finish", return_value=["path-x: injected refusal"]):
            self.assertEqual(evidence.record(self.root, self.policy, self.output), 2)
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        self.assertEqual(record["outcome"], "success")
        self.assertEqual(record["exit_status"], 0)
        self.assertEqual(record["resource_cleanup"], {"outcome": "failure", "detail": ["path-x: injected refusal"]})
        self.assertEqual(evidence.inspect_record(self.root, self.output)["classification"],
                         "cleanup-failure-after-success")
        self.refused("owned resource cleanup")

    def test_managed_binding_reaches_the_child_without_changing_reusable_inputs(self):
        (self.root / "Makefile").write_text('check:\n\t@printf "%s|%s" "$$CONVEYOR_VALIDATION_INVOCATION" "$$TMPDIR"\n')
        self.record()
        record = evidence.read_record(self.output / "manifest.json", (self.output / "key").read_bytes())
        invocation = Path(record["invocation"])
        binding, tmpdir = (self.output / "command.log").read_text().split("|")
        self.assertEqual(binding, str(invocation))
        self.assertIn(invocation.name, tmpdir)
        self.assertFalse(Path(tmpdir).exists(), "invocation temporary child was not removed")
        inventory = validation_resources.load_inventory(invocation)
        self.assertEqual(inventory["state"], "completed")
        self.assertEqual(inventory["configuration"]["evidence"], str(self.output))
        self.assertNotIn(validation_resources.BINDING, record["before"]["environment"])
        evidence.check(self.root, self.policy, self.output)

    def test_legacy_manifest_without_invocation_fields_remains_checkable(self):
        self.record()
        key = (self.output / "key").read_bytes()
        record = evidence.read_record(self.output / "manifest.json", key)
        for field in ("invocation", "timeout", "resource_cleanup"):
            del record[field]
        evidence.write_record(self.output / "manifest.json", record, key, replace=True)
        evidence.check(self.root, self.policy, self.output)
        self.assertEqual(evidence.inspect_record(self.root, self.output)["classification"], "success")

    def test_ram_backed_temporary_root_refuses_before_recording(self):
        del os.environ[validation_resources.ALLOW_RAM_TMP]
        with patch.object(validation_resources, "backing_filesystem", return_value="tmpfs"):
            with self.assertRaisesRegex(evidence.Refused, "RAM-backed.*CONVEYOR_VALIDATION_ALLOW_RAM_TMP=1"):
                evidence.record(self.root, self.policy, self.output)
        self.assertFalse(self.output.exists())

    def test_managed_postgres_fixture_field_is_validated(self):
        path = self.base / "policy.json"
        self.policy["layer"] = "postgres"
        self.policy["environment"].append("TEST_DATABASE_URL")
        self.policy["backend"] = {"isolation": "disposable-per-run", "probe": ["python3", "-c", 'print("{}")']}
        fixture = {"backend": "postgres", "url_env": "TEST_DATABASE_URL", "prepared_url_env": "TEST_DATABASE_URL",
                   "external_network_env": "CONVEYOR_TEST_EXTERNAL_NETWORK", "database_prefix": "conveyor",
                   "minimum_free_bytes": 1, "timeout": "20s"}
        for value, accepted in ((True, True), (False, True), ("yes", False)):
            with self.subTest(value=value):
                self.policy["fixture"] = dict(fixture, managed_postgres=value)
                path.write_text(json.dumps(self.policy))
                if accepted:
                    evidence.policy_read(path)
                else:
                    with self.assertRaises(evidence.Refused):
                        evidence.policy_read(path)
        self.policy["layer"] = "singlestore"
        self.policy["fixture"] = dict(fixture, backend="singlestore", managed_postgres=True)
        path.write_text(json.dumps(self.policy))
        with self.assertRaisesRegex(evidence.Refused, "managed_postgres"):
            evidence.policy_read(path)

    def test_no_tracked_exclusion_or_unknown_symlink(self):
        self.policy["exclude"]["generated"] = "claimed output"
        with self.assertRaises(evidence.Refused):
            evidence.snapshot(self.root, self.policy, b"key")
        self.policy["exclude"] = {}
        (self.root / "external-link").symlink_to(self.base / "unlisted")
        (self.base / "unlisted").write_text("external")
        with self.assertRaises(evidence.Refused):
            evidence.snapshot(self.root, self.policy, b"key")

    def test_backend_layers_capture_identity_but_never_reuse_mutable_database(self):
        for layer in ("postgres", "singlestore"):
            with self.subTest(layer=layer):
                self.output = self.base / layer
                self.policy["layer"] = layer
                self.policy["backend"] = {"isolation": "disposable-per-run", "probe": ["python3", "-c",
                    'import json; print(json.dumps(dict(identity="fixture",version="1",configuration="isolated",instance="new")))']}
                self.record()
                self.refused("database evidence")
                self.policy["backend"]["probe"][-1] = 'print("{}")'
                with self.assertRaises(evidence.Refused):
                    evidence.snapshot(self.root, self.policy, b"key")


class CacheCleanupFilterTests(unittest.TestCase):
    """Cleanup reuses the shared inspector's owner filter over fixture /proc trees.

    Only filesystem metadata is injected: process-entry owners, directory
    modes, birth times, and clocks. The real inspector runs every case.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        environment = patch.dict(os.environ, {"XDG_CACHE_HOME": str(self.base / "cache-home")})
        environment.start()
        self.addCleanup(environment.stop)
        self.task_cache = self.base / "cache-home" / "conveyor" / "fixture-task"
        self.child = self.task_cache / "go-build"
        self.child.mkdir(parents=True)
        self.proc = self.base / "proc"
        self.proc.mkdir()
        (self.proc / "uptime").write_text("1000.00 0\n")

    def process(self, pid, start=None, descriptor=None):
        """Model a process entry: uninspectable unless descriptor names a readable target."""
        entry = self.proc / str(pid)
        entry.mkdir()
        if start is not None:
            (entry / "stat").write_text(f"{pid} (fixture) S 1 {pid} {pid} 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 {start}\n")
        if descriptor is not None:
            (entry / "fd").mkdir()
            (entry / "fd" / "3").symlink_to(descriptor)
            (entry / "cwd").symlink_to(self.base, target_is_directory=True)
            (entry / "root").symlink_to(Path("/"), target_is_directory=True)
            (entry / "environ").write_bytes(b"")
        return entry

    def foreign(self, *entries):
        """Report the given process entries as owned by another user."""
        real = Path.stat
        foreign = set(entries)

        def owner(path, *args, **kwargs):
            info = real(path, *args, **kwargs)
            if Path(path) in foreign:
                values = list(info)
                values[stat.ST_UID] = os.getuid() + 1
                return os.stat_result(values)
            return info
        return patch.object(Path, "stat", owner)

    def traversable(self):
        """Give every directory group and other execute, so no ancestor isolates the cache."""
        def lstat(directory):
            values = list(os.lstat(directory))
            values[stat.ST_MODE] |= stat.S_IXGRP | stat.S_IXOTH
            return os.stat_result(values)
        return patch.object(evidence, "owner_only_ancestor",
                            functools.partial(evidence.owner_only_ancestor, lstat=lstat))

    def cleanup(self):
        return evidence.cleanup_cache("fixture-task", self.task_cache, [], self.proc)

    def test_owner_only_ancestor_disregards_foreign_uninspectable_process(self):
        # TemporaryDirectory creates an owner-only (0700) directory.
        self.assertEqual(evidence.owner_only_ancestor(self.child), self.base)
        unknown = self.task_cache / "unknown-child"
        unknown.mkdir()
        sibling = self.task_cache.parent / "sibling-task" / "go-build"
        sibling.mkdir(parents=True)
        durable = self.base / "state" / "conveyor" / "fixture-task" / "command.log"
        durable.parent.mkdir(parents=True)
        durable.write_text("evidence")
        foreign = self.process(4242)
        with self.foreign(foreign):
            self.assertIn("4242:ambiguous:cwd", evidence.active_cache_users(self.child, self.proc))
            self.assertEqual(evidence.cleanup_cache("fixture-task", self.task_cache, [durable], self.proc),
                             ["go-build"])
        self.assertFalse(self.child.exists())
        self.assertTrue(self.task_cache.is_dir())
        self.assertTrue(unknown.is_dir())
        self.assertTrue(sibling.is_dir())
        self.assertEqual(durable.read_text(), "evidence")

    def test_traversable_chain_inspects_foreign_uninspectable_process(self):
        foreign = self.process(4242, start=1)
        with self.foreign(foreign), self.traversable():
            self.assertIsNone(evidence.owner_only_ancestor(self.child))
            with self.assertRaisesRegex(evidence.Refused, "4242:ambiguous"):
                self.cleanup()
        self.assertTrue(self.child.is_dir())

    def test_own_uninspectable_process_refuses_under_owner_isolation(self):
        # A non-dumpable process of the invoking user (for example sshd-session)
        # can enter an owner-only cache, so it stays a possible user whatever
        # its start time: old, recent, far later, or unknown (no stat entry).
        for pid, start in ((4242, 1), (4343, 100_000), (4444, 10**12), (4545, None)):
            entry = self.process(pid, start=start)
            with self.assertRaisesRegex(evidence.Refused, f"{pid}:ambiguous"):
                self.cleanup()
            shutil.rmtree(entry)
        self.assertTrue(self.child.is_dir())

    def test_clock_changes_and_birth_times_never_disregard_an_uninspectable_process(self):
        """No creation-time filter: wall-clock steps cannot map a birth time onto start ticks.

        A process that started long before the child (start tick 1) still
        blocks cleanup whatever the clocks or the child's birth time report.
        """
        foreign = self.process(4242, start=1)
        own = self.process(4343, start=1)
        real_stat = Path.stat
        now = time.time()
        for wall in (now, now - 300, now + 300, now - 86_400):
            for birth in (None, now - 600, now + 600, float("nan"), float("inf"), -1.0, "malformed"):
                def child_stat(path, *args, birth=birth, **kwargs):
                    info = real_stat(path, *args, **kwargs)
                    if Path(path) != self.child or birth is None:
                        return info
                    return SimpleNamespace(st_uid=info.st_uid, st_mode=info.st_mode, st_birthtime=birth,
                                           st_ctime=birth, st_mtime=birth)
                with patch.object(evidence.time, "time", return_value=wall), \
                        patch.object(Path, "stat", child_stat):
                    with self.foreign(foreign), self.traversable(), \
                            self.assertRaisesRegex(evidence.Refused, "4242:ambiguous"):
                        self.cleanup()
                    with self.assertRaisesRegex(evidence.Refused, "4343:ambiguous"):
                        self.cleanup()
        self.assertTrue(self.child.is_dir())

    def test_readable_reference_refuses_under_owner_isolation(self):
        (self.child / "object").write_text("cached")
        self.process(4242, start=1, descriptor=self.child / "object")
        with self.assertRaisesRegex(evidence.Refused, "4242:fd:3"):
            self.cleanup()
        with self.traversable(), self.assertRaisesRegex(evidence.Refused, "4242:fd:3"):
            self.cleanup()
        self.assertTrue((self.child / "object").is_file())

    def test_missing_proc_refuses_with_owner_isolation(self):
        with self.assertRaisesRegex(evidence.Refused, "requires /proc"):
            evidence.active_cache_users(self.child, self.base / "absent-proc", uid=os.getuid())
        with self.assertRaisesRegex(evidence.Refused, "requires /proc"):
            evidence.cleanup_cache("fixture-task", self.task_cache, [], self.base / "absent-proc")
        self.assertTrue(self.child.is_dir())

    @unittest.skipIf(os.geteuid() == 0, "root reads a mode 000 directory")
    def test_uninspectable_proc_refuses_with_owner_isolation(self):
        self.proc.chmod(0)
        self.addCleanup(self.proc.chmod, 0o700)
        with self.assertRaisesRegex(evidence.Refused, "ambiguous: /proc"):
            self.cleanup()
        self.assertTrue(self.child.is_dir())

    def test_later_child_refusal_preserves_earlier_candidates(self):
        # go-build precedes tmp in DISPOSABLE_CACHE_CHILDREN; every child is
        # preflighted before any deletion.
        later = self.task_cache / "tmp"
        later.mkdir()
        (later / "object").write_text("cached")
        self.process(4242, start=1, descriptor=later / "object")
        with self.assertRaisesRegex(evidence.Refused, "active: tmp"):
            self.cleanup()
        self.assertTrue(self.child.is_dir())
        self.assertTrue((later / "object").is_file())

    def test_owner_only_ancestor_requires_an_owned_untraversable_directory(self):
        uid = os.getuid()
        path = Path("/fixture/private/cache/child")

        def metadata(modes):
            def lstat(directory):
                mode, owner = modes.get(str(directory), (stat.S_IFDIR | 0o755, uid))
                if mode is None:
                    raise PermissionError(directory)
                return os.stat_result((mode, 0, 0, 0, owner, 0, 0, 0, 0, 0))
            return lstat

        with patch.object(Path, "resolve", lambda self, strict=False: self):
            self.assertIsNone(evidence.owner_only_ancestor(path, metadata({})))
            private = {"/fixture/private": (stat.S_IFDIR | 0o700, uid)}
            self.assertEqual(evidence.owner_only_ancestor(path, metadata(private)), Path("/fixture/private"))
            for mode, owner in ((stat.S_IFDIR | 0o700, uid + 1), (stat.S_IFDIR | 0o710, uid),
                                (stat.S_IFDIR | 0o701, uid), (stat.S_IFLNK | 0o700, uid),
                                (stat.S_IFREG | 0o700, uid), (None, uid)):
                rejected = {"/fixture/private": (mode, owner)}
                self.assertIsNone(evidence.owner_only_ancestor(path, metadata(rejected)), (mode, owner))


class CacheCreationMarkerTests(unittest.TestCase):
    """prepare-cache records boot-relative creation ticks; cleanup uses them only when every fact matches.

    The fixture /proc tree supplies uptime, the boot ID, and process entries.
    The real shared inspector runs every cleanup case.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        # TemporaryDirectory creates an owner-only (0700) directory, so owner
        # isolation limits inspection to the invoking user's processes.
        self.base = Path(self.tmp.name).resolve()
        environment = patch.dict(os.environ, {"XDG_CACHE_HOME": str(self.base / "cache-home")})
        environment.start()
        self.addCleanup(environment.stop)
        self.task_cache = self.base / "cache-home" / "conveyor" / "fixture-task"
        self.proc = self.base / "proc"
        (self.proc / "sys" / "kernel" / "random").mkdir(parents=True)
        self.set_uptime("1000.00")
        self.set_boot("boot-a")
        self.tick = 1000 * os.sysconf("SC_CLK_TCK")

    def set_uptime(self, value):
        (self.proc / "uptime").write_text(value + " 0\n")

    def set_boot(self, value):
        (self.proc / "sys" / "kernel" / "random" / "boot_id").write_text(value + "\n")

    def prepare(self, task_cache=None, **kwargs):
        return evidence.prepare_cache("fixture-task", task_cache or self.task_cache, self.proc, **kwargs)

    def cleanup(self, references=()):
        return evidence.cleanup_cache("fixture-task", self.task_cache, list(references), self.proc)

    def marker_path(self):
        return self.task_cache / evidence.CACHE_MARKER

    def marker(self):
        return json.loads(self.marker_path().read_text())

    def write_marker(self, value, mode=0o600):
        path = self.marker_path()
        path.unlink(missing_ok=True)
        path.write_bytes(value if isinstance(value, bytes) else json.dumps(value).encode())
        path.chmod(mode)

    def process(self, pid, start=None, cwd=None, root=None, descriptor=None, environ=None):
        """Model a process entry: uninspectable unless a readable reference is given."""
        entry = self.proc / str(pid)
        entry.mkdir()
        if start is not None:
            (entry / "stat").write_text(f"{pid} (fixture) S 1 {pid} {pid} 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 {start}\n")
        if cwd is None and root is None and descriptor is None and environ is None:
            return entry
        (entry / "cwd").symlink_to(cwd or self.base, target_is_directory=True)
        (entry / "root").symlink_to(root or Path("/"), target_is_directory=True)
        (entry / "fd").mkdir()
        if descriptor is not None:
            (entry / "fd" / "3").symlink_to(descriptor)
        (entry / "environ").write_bytes(environ or b"")
        return entry

    # -- prepare-cache -------------------------------------------------------

    def test_prepare_creates_owned_root_children_and_owner_only_marker(self):
        result = self.prepare()
        children = list(evidence.DISPOSABLE_CACHE_CHILDREN)
        self.assertEqual(result["created"], children)
        self.assertEqual(result["existing"], [])
        self.assertEqual(result["recorded"], children)
        root = self.task_cache.lstat()
        self.assertTrue(stat.S_ISDIR(root.st_mode))
        self.assertEqual(stat.S_IMODE(root.st_mode), 0o700)
        self.assertEqual(root.st_uid, os.getuid())
        self.assertEqual(sorted(path.name for path in self.task_cache.iterdir()),
                         sorted(children + [evidence.CACHE_MARKER]))
        info = self.marker_path().lstat()
        self.assertTrue(stat.S_ISREG(info.st_mode))
        self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
        marker = self.marker()
        self.assertEqual(set(marker), {"schema", "boot_id", "children"})
        self.assertEqual(marker["schema"], evidence.CACHE_MARKER_SCHEMA)
        self.assertEqual(marker["boot_id"], "boot-a")
        for name in children:
            child = (self.task_cache / name).lstat()
            self.assertTrue(stat.S_ISDIR(child.st_mode))
            self.assertEqual(marker["children"][name],
                             {"device": child.st_dev, "inode": child.st_ino, "created_ticks": self.tick})
        self.assertEqual(evidence.read_cache_marker(self.task_cache), (marker, None))

    def test_tick_is_sampled_before_each_child_mkdir(self):
        events = []
        ticks = itertools.count(500)
        real_mkdir = os.mkdir

        def sample(proc):
            value = next(ticks)
            events.append(("tick", value))
            return value

        def mkdir(path, *args, **kwargs):
            if Path(path).parent == self.task_cache:
                events.append(("mkdir", Path(path).name))
            return real_mkdir(path, *args, **kwargs)
        with patch.object(validation_resources, "current_ticks", sample), patch.object(evidence.os, "mkdir", mkdir):
            self.prepare()
        expected = []
        for offset, name in enumerate(evidence.DISPOSABLE_CACHE_CHILDREN):
            expected += [("tick", 500 + offset), ("mkdir", name)]
        self.assertEqual(events, expected)
        recorded = {name: entry["created_ticks"] for name, entry in self.marker()["children"].items()}
        self.assertEqual(recorded, {name: 500 + offset for offset, name in enumerate(evidence.DISPOSABLE_CACHE_CHILDREN)})

    def test_repeated_preparation_keeps_existing_entries_and_replaces_recreated_ones(self):
        (self.task_cache / "go-build").mkdir(parents=True)
        first = self.prepare()
        self.assertEqual(first["existing"], ["go-build"])
        self.assertNotIn("go-build", first["recorded"])
        before = self.marker_path().read_bytes()
        self.assertNotIn("go-build", self.marker()["children"])

        # Children that still exist keep their entries byte-for-byte.
        self.set_uptime("2000.00")
        again = self.prepare()
        self.assertEqual((again["created"], again["recorded"]), ([], []))
        self.assertEqual(again["existing"], list(evidence.DISPOSABLE_CACHE_CHILDREN))
        self.assertEqual(self.marker_path().read_bytes(), before)

        # A child this call re-creates replaces its earlier entry; an
        # unrecorded child created now gets the tick sampled before its mkdir.
        old_npm = self.marker()["children"]["npm"]
        old_tmp = self.marker()["children"]["tmp"]
        shutil.rmtree(self.task_cache / "npm")
        shutil.rmtree(self.task_cache / "go-build")
        replaced = self.prepare()
        self.assertEqual(replaced["created"], ["go-build", "npm"])
        self.assertEqual(replaced["recorded"], ["go-build", "npm"])
        marker = self.marker()
        later = 2000 * os.sysconf("SC_CLK_TCK")
        for name in ("go-build", "npm"):
            info = (self.task_cache / name).lstat()
            self.assertEqual(marker["children"][name],
                             {"device": info.st_dev, "inode": info.st_ino, "created_ticks": later})
        self.assertGreater(marker["children"]["npm"]["created_ticks"], old_npm["created_ticks"])
        self.assertEqual(marker["children"]["tmp"], old_tmp)

    def test_prepare_refuses_sibling_symlink_and_unsafe_targets_before_creating(self):
        for task in ("", ".", "..", "nested/task"):
            with self.assertRaisesRegex(evidence.Refused, "invalid task identity"):
                evidence.prepare_cache(task, self.task_cache, self.proc)
        sibling = self.task_cache.parent / "sibling-task"
        with self.assertRaisesRegex(evidence.Refused, "current task child"):
            self.prepare(sibling)
        self.assertFalse(sibling.exists())

        elsewhere = self.base / "elsewhere"
        elsewhere.mkdir()
        self.task_cache.parent.mkdir(parents=True)
        self.task_cache.symlink_to(elsewhere, target_is_directory=True)
        with self.assertRaisesRegex(evidence.Refused, "cannot be a symlink"):
            self.prepare()
        with self.assertRaisesRegex(evidence.Refused, "cannot be a symlink"):
            self.prepare(elsewhere)
        self.assertEqual(list(elsewhere.iterdir()), [])
        self.task_cache.unlink()

        self.task_cache.mkdir()
        target = self.base / "target"
        target.mkdir()
        (self.task_cache / "tmp").symlink_to(target, target_is_directory=True)
        with self.assertRaisesRegex(evidence.Refused, "symlink: tmp"):
            self.prepare()
        (self.task_cache / "tmp").unlink()
        (self.task_cache / "npm").write_text("not a directory")
        with self.assertRaisesRegex(evidence.Refused, "ownership is missing or ambiguous: npm"):
            self.prepare()
        (self.task_cache / "npm").unlink()
        (self.task_cache / "go-tmp").mkdir()
        foreign = (self.task_cache / "go-tmp").lstat()
        real_lstat = os.lstat

        def lstat(path, *args, **kwargs):
            info = real_lstat(path, *args, **kwargs)
            if (info.st_dev, info.st_ino) != (foreign.st_dev, foreign.st_ino):
                return info
            values = list(info)
            values[stat.ST_UID] = os.getuid() + 1
            return os.stat_result(values)
        with patch.object(evidence.os, "lstat", lstat), \
                self.assertRaisesRegex(evidence.Refused, "ownership is missing or ambiguous: go-tmp"):
            self.prepare()
        self.assertEqual(sorted(path.name for path in self.task_cache.iterdir()), ["go-tmp"])
        self.assertEqual(list(target.iterdir()), [])

    def test_missing_timing_facts_create_the_cache_without_bounds(self):
        cases = {
            "boot": lambda: (self.proc / "sys" / "kernel" / "random" / "boot_id").unlink(),
            "uptime": lambda: (self.proc / "uptime").unlink(),
        }
        for label, remove in cases.items():
            with self.subTest(label):
                shutil.rmtree(self.task_cache, ignore_errors=True)
                self.set_uptime("1000.00")
                self.set_boot("boot-a")
                remove()
                result = self.prepare()
                self.assertEqual(result["created"], list(evidence.DISPOSABLE_CACHE_CHILDREN))
                self.assertEqual(result["recorded"], [])
                self.assertFalse(os.path.lexists(self.marker_path()))
        for backend in (validation_resources.DARWIN_BACKEND, validation_resources.UNAVAILABLE):
            with self.subTest(backend):
                shutil.rmtree(self.task_cache)
                self.set_uptime("1000.00")
                self.set_boot("boot-a")
                with patch.object(validation_resources, "current_ticks", side_effect=AssertionError("sampled")), \
                        patch.object(validation_resources, "boot_id", side_effect=AssertionError("read")):
                    result = self.prepare(backend=backend)
                self.assertEqual(result["created"], list(evidence.DISPOSABLE_CACHE_CHILDREN))
                self.assertEqual(result["recorded"], [])
                self.assertIn("not boot-relative", result["marker"])
                self.assertFalse(os.path.lexists(self.marker_path()))

    def test_unusable_or_cross_boot_marker_is_preserved_without_new_entries(self):
        self.prepare()
        valid = self.marker()
        copy_path = self.base / "valid-marker.json"
        copy_path.write_text(json.dumps(valid))
        copy_path.chmod(0o600)
        cases = {
            "cross-boot": (dict(valid, boot_id="boot-b"), 0o600, "another boot"),
            "malformed": (b"{not json", 0o600, "not valid JSON"),
            "group-readable": (valid, 0o640, "not owner-only"),
            "symlink": (None, None, "symlink"),
        }
        for label, (content, mode, reason) in cases.items():
            with self.subTest(label):
                for name in evidence.DISPOSABLE_CACHE_CHILDREN:
                    shutil.rmtree(self.task_cache / name, ignore_errors=True)
                if content is None:
                    self.marker_path().unlink()
                    self.marker_path().symlink_to(copy_path)
                else:
                    self.write_marker(content, mode)
                before = os.readlink(self.marker_path()) if content is None else self.marker_path().read_bytes()
                result = self.prepare()
                self.assertEqual(result["created"], list(evidence.DISPOSABLE_CACHE_CHILDREN))
                self.assertEqual(result["recorded"], [])
                self.assertIn(reason, result["marker"])
                after = os.readlink(self.marker_path()) if content is None else self.marker_path().read_bytes()
                self.assertEqual(after, before)
                self.assertEqual(sorted(path.name for path in self.task_cache.iterdir()),
                                 sorted(list(evidence.DISPOSABLE_CACHE_CHILDREN) + [evidence.CACHE_MARKER]))

    def test_partial_preparation_records_only_children_it_created(self):
        real_mkdir = os.mkdir

        def mkdir(path, *args, **kwargs):
            if Path(path) == self.task_cache / "playwright":
                raise OSError(28, "No space left on device")
            return real_mkdir(path, *args, **kwargs)
        with patch.object(evidence.os, "mkdir", mkdir), self.assertRaisesRegex(OSError, "No space"):
            self.prepare()
        self.assertEqual(sorted(self.marker()["children"]), ["go-build", "go-tmp", "tmp"])
        self.assertFalse((self.task_cache / "playwright").exists())
        self.assertFalse((self.task_cache / "npm").exists())
        # A later run creates the rest; the earlier entries stay exact.
        earlier = self.marker()["children"]
        self.set_uptime("1500.00")
        self.assertEqual(self.prepare()["recorded"], ["playwright", "npm"])
        marker = self.marker()["children"]
        self.assertEqual({name: marker[name] for name in earlier}, earlier)
        self.assertEqual(marker["npm"]["created_ticks"], 1500 * os.sysconf("SC_CLK_TCK"))

    def test_failed_marker_publication_leaves_no_bound(self):
        with patch.object(evidence.os, "replace", side_effect=OSError(5, "I/O error")), \
                self.assertRaisesRegex(OSError, "I/O error"):
            self.prepare()
        self.assertEqual(sorted(path.name for path in self.task_cache.iterdir()),
                         sorted(evidence.DISPOSABLE_CACHE_CHILDREN))
        self.process(4242, start=1)
        with self.assertRaisesRegex(evidence.Refused, "4242:ambiguous"):
            self.cleanup()

    def test_recreation_replaces_the_tick_when_the_inode_is_reused(self):
        self.prepare()
        # Stage a directory and point the stale go-build entry at its exact
        # device and inode with the old tick. The patched mkdir moves it into
        # place, so the re-created child deterministically reuses that inode.
        staged = self.base / "staged-go-build"
        staged.mkdir(mode=0o700)
        reused = staged.lstat()
        shutil.rmtree(self.task_cache / "go-build")
        marker = self.marker()
        marker["children"]["go-build"] = {"device": reused.st_dev, "inode": reused.st_ino,
                                          "created_ticks": self.tick}
        self.write_marker(marker)
        real_mkdir = os.mkdir

        def mkdir(path, *args, **kwargs):
            if Path(path) == self.task_cache / "go-build":
                os.rename(staged, path)
                return None
            return real_mkdir(path, *args, **kwargs)
        self.set_uptime("2000.00")
        with patch.object(evidence.os, "mkdir", mkdir):
            result = self.prepare()
        self.assertEqual((result["created"], result["recorded"]), (["go-build"], ["go-build"]))
        info = (self.task_cache / "go-build").lstat()
        self.assertEqual((info.st_dev, info.st_ino), (reused.st_dev, reused.st_ino))
        self.assertEqual(self.marker()["children"]["go-build"],
                         {"device": reused.st_dev, "inode": reused.st_ino,
                          "created_ticks": 2000 * os.sysconf("SC_CLK_TCK")})

    def test_child_created_by_another_actor_after_the_preflight_is_never_dated(self):
        real_mkdir = os.mkdir

        def raced(path, *args, **kwargs):
            # Another actor wins the race: the directory appears, then this
            # call's mkdir fails with FileExistsError.
            if Path(path) == self.task_cache / "npm":
                real_mkdir(path, *args, **kwargs)
                raise FileExistsError(17, "File exists", str(path))
            return real_mkdir(path, *args, **kwargs)

        # No earlier entry: the raced child stays unrecorded.
        with patch.object(evidence.os, "mkdir", raced):
            first = self.prepare()
        children = list(evidence.DISPOSABLE_CACHE_CHILDREN)
        self.assertEqual(first["created"], children[:-1])
        self.assertEqual(first["existing"], ["npm"])
        self.assertEqual(first["recorded"], children[:-1])
        self.assertNotIn("npm", self.marker()["children"])

        # A stale earlier entry: the raced child leaves the marker byte-for-byte.
        shutil.rmtree(self.task_cache)
        self.prepare()
        shutil.rmtree(self.task_cache / "npm")
        before = self.marker_path().read_bytes()
        self.set_uptime("2000.00")
        with patch.object(evidence.os, "mkdir", raced):
            again = self.prepare()
        self.assertEqual((again["created"], again["recorded"]), ([], []))
        self.assertEqual(again["existing"], children[:-1] + ["npm"])
        self.assertEqual(self.marker_path().read_bytes(), before)

    def test_recreation_without_timing_facts_keeps_the_marker_unchanged(self):
        self.prepare()
        shutil.rmtree(self.task_cache / "npm")
        before = self.marker_path().read_bytes()
        (self.proc / "uptime").unlink()
        result = self.prepare()
        self.assertEqual((result["created"], result["recorded"]), (["npm"], []))
        self.assertEqual(self.marker_path().read_bytes(), before)

    def test_prepare_cache_command(self):
        env = dict(os.environ, XDG_CACHE_HOME=str(self.base / "cache-home"), PYTHONDONTWRITEBYTECODE="1")
        script = str(REPO / "scripts" / "validation_evidence.py")
        result = subprocess.run([sys.executable, script, "prepare-cache", "--task", "fixture-task",
                                 "--task-cache", str(self.task_cache)], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Prepared task cache " + str(self.task_cache), result.stdout)
        self.assertIn("created go-build, go-tmp, tmp, playwright, npm", result.stdout)
        for name in evidence.DISPOSABLE_CACHE_CHILDREN:
            self.assertTrue((self.task_cache / name).is_dir())
        if validation_resources.process_backend() == validation_resources.PROC_BACKEND \
                and validation_resources.boot_id() and validation_resources.current_ticks() is not None:
            self.assertIn("creation bounds recorded: go-build, go-tmp, tmp, playwright, npm", result.stdout)
            self.assertEqual(sorted(self.marker()["children"]), sorted(evidence.DISPOSABLE_CACHE_CHILDREN))
        else:
            self.assertFalse(os.path.lexists(self.marker_path()))
        refused = subprocess.run([sys.executable, script, "prepare-cache", "--task", "fixture-task",
                                  "--task-cache", str(self.task_cache.parent / "sibling-task")],
                                 env=env, capture_output=True, text=True)
        self.assertEqual(refused.returncode, 2)
        self.assertIn("current task child", refused.stderr)
        missing = subprocess.run([sys.executable, script, "prepare-cache", "--task", "fixture-task"],
                                 env=env, capture_output=True, text=True)
        self.assertEqual(missing.returncode, 2)
        self.assertIn("prepare-cache requires --task and --task-cache", missing.stderr)

    # -- cleanup with the marker ---------------------------------------------

    def test_marker_disregards_own_uninspectable_process_started_before_the_tick(self):
        self.prepare()
        unknown = self.task_cache / "retained-unknown-child"
        unknown.mkdir()
        sibling = self.task_cache.parent / "sibling-task" / "go-build"
        sibling.mkdir(parents=True)
        durable = self.base / "state" / "conveyor" / "fixture-task" / "command.log"
        durable.parent.mkdir(parents=True)
        durable.write_text("evidence")
        marker = self.marker_path().read_bytes()
        # An sshd-session or systemd --user manager of the invoking user:
        # non-dumpable, so cwd, fd, and environ cannot be read.
        self.process(4242, start=self.tick - 1)
        self.process(4343, start=1)
        self.assertIn("4242:ambiguous:cwd", evidence.active_cache_users(self.task_cache / "go-build", self.proc))
        self.assertEqual(self.cleanup([durable]), list(evidence.DISPOSABLE_CACHE_CHILDREN))
        for name in evidence.DISPOSABLE_CACHE_CHILDREN:
            self.assertFalse((self.task_cache / name).exists())
        self.assertTrue(self.task_cache.is_dir())
        self.assertEqual(self.marker_path().read_bytes(), marker)
        self.assertTrue(unknown.is_dir())
        self.assertTrue(sibling.is_dir())
        self.assertEqual(durable.read_text(), "evidence")

    def test_second_round_preparation_bounds_cleanup_from_the_new_tick(self):
        """The multi-round task: prepare, clean up, prepare again, clean up again."""
        children = list(evidence.DISPOSABLE_CACHE_CHILDREN)
        self.prepare()
        first = self.marker()["children"]
        self.assertEqual(self.cleanup(), children)

        self.set_uptime("2000.00")
        second_tick = 2000 * os.sysconf("SC_CLK_TCK")
        again = self.prepare()
        self.assertEqual((again["created"], again["recorded"]), (children, children))
        marker = self.marker()
        for name in children:
            info = (self.task_cache / name).lstat()
            self.assertEqual(marker["children"][name],
                             {"device": info.st_dev, "inode": info.st_ino, "created_ticks": second_tick})
            self.assertGreaterEqual(marker["children"][name]["created_ticks"], first[name]["created_ticks"])

        unknown = self.task_cache / "retained-unknown-child"
        unknown.mkdir()
        sibling = self.task_cache.parent / "sibling-task" / "go-build"
        sibling.mkdir(parents=True)
        durable = self.base / "state" / "conveyor" / "fixture-task" / "command.log"
        durable.parent.mkdir(parents=True)
        durable.write_text("evidence")
        marker_bytes = self.marker_path().read_bytes()

        # Equal, later, and unknown starts, and readable references, refuse.
        refusals = {
            "equal": (dict(start=second_tick), "4242:ambiguous"),
            "later": (dict(start=second_tick + 1), "4242:ambiguous"),
            "unknown": (dict(), "4242:ambiguous"),
            "readable cwd": (dict(start=1, cwd=self.task_cache / "go-build"), "4242:cwd"),
        }
        for label, (process, reason) in refusals.items():
            with self.subTest(label):
                entry = self.process(4242, **process)
                with self.assertRaisesRegex(evidence.Refused, "active: go-build .*" + reason):
                    self.cleanup()
                shutil.rmtree(entry)
        for name in children:
            self.assertTrue((self.task_cache / name).is_dir())

        # The invoking user's sshd-session started between the two bounds:
        # round 1's stale tick would not cover it, the fresh tick does.
        self.process(4242, start=self.tick + 1)
        self.process(4343, start=1)
        self.assertIn("4242:ambiguous:cwd", evidence.active_cache_users(self.task_cache / "go-build", self.proc))
        stdout = StringIO()
        with patch.object(sys, "argv", ["validation_evidence.py", "cleanup", "--task", "fixture-task",
                                        "--task-cache", str(self.task_cache), "--reference", str(durable)]), \
                patch.object(evidence, "cleanup_cache", functools.partial(evidence.cleanup_cache, proc=self.proc)), \
                patch("sys.stdout", stdout):
            self.assertEqual(evidence.main(), 0)
        self.assertEqual(stdout.getvalue().strip(), "Removed disposable cache children: " + ", ".join(children))
        for name in children:
            self.assertFalse((self.task_cache / name).exists())
        self.assertTrue(self.task_cache.is_dir())
        self.assertEqual(self.marker_path().read_bytes(), marker_bytes)
        self.assertTrue(unknown.is_dir())
        self.assertTrue(sibling.is_dir())
        self.assertEqual(durable.read_text(), "evidence")

    def test_equal_later_or_unknown_start_refuses(self):
        self.prepare()
        for pid, start in ((4242, self.tick), (4343, self.tick + 1), (4444, 10**12), (4545, None)):
            with self.subTest(start=start):
                entry = self.process(pid, start=start)
                with self.assertRaisesRegex(evidence.Refused, f"active: go-build .*{pid}:ambiguous"):
                    self.cleanup()
                shutil.rmtree(entry)
        for name in evidence.DISPOSABLE_CACHE_CHILDREN:
            self.assertTrue((self.task_cache / name).is_dir())

    def test_readable_references_refuse_even_when_the_process_predates_the_tick(self):
        self.prepare()
        child = self.task_cache / "tmp"
        (child / "object").write_text("cached")
        cases = {
            "cwd": dict(cwd=child),
            "root": dict(root=child),
            "fd:3": dict(descriptor=child / "object"),
            "env:TMPDIR": dict(environ=b"TMPDIR=" + os.fsencode(child) + b"\0"),
        }
        for label, reference in cases.items():
            with self.subTest(label):
                entry = self.process(4242, start=1, **reference)
                with self.assertRaisesRegex(evidence.Refused, "active: tmp .*4242:" + label):
                    self.cleanup()
                shutil.rmtree(entry)
        self.assertTrue((child / "object").is_file())
        for name in evidence.DISPOSABLE_CACHE_CHILDREN:
            self.assertTrue((self.task_cache / name).is_dir())

    def test_marker_facts_that_do_not_match_omit_the_bound(self):
        self.process(4242, start=1)

        def boot_mismatch():
            self.set_boot("boot-b")

        def recreated_child():
            # Keep the old directory alive so the new child has a distinct inode.
            (self.task_cache / "go-build").rename(self.task_cache / "retained-old-go-build")
            (self.task_cache / "go-build").mkdir()
            self.assertNotEqual((self.task_cache / "go-build").lstat().st_ino,
                                (self.task_cache / "retained-old-go-build").lstat().st_ino)

        def missing_entry():
            marker = self.marker()
            del marker["children"]["go-build"]
            self.write_marker(marker)

        def symlinked():
            copy_path = self.base / "marker-copy.json"
            copy_path.unlink(missing_ok=True)
            copy_path.write_bytes(self.marker_path().read_bytes())
            copy_path.chmod(0o600)
            self.marker_path().unlink()
            self.marker_path().symlink_to(copy_path)

        def group_writable():
            self.marker_path().chmod(0o620)

        def rewrite(change):
            def apply():
                marker = self.marker()
                change(marker)
                self.write_marker(marker)
            return apply

        def raw(content):
            return lambda: self.write_marker(content)

        def directory():
            self.marker_path().unlink()
            self.marker_path().mkdir(mode=0o700)

        cases = {
            "boot mismatch": boot_mismatch,
            "recreated child": recreated_child,
            "missing entry": missing_entry,
            "symlinked marker": symlinked,
            "group-writable marker": group_writable,
            "directory marker": directory,
            "malformed JSON": raw(b"{\"schema\": 1,"),
            "not an object": raw(b"[]"),
            "empty boot ID": rewrite(lambda m: m.update(boot_id="")),
            "unknown schema": rewrite(lambda m: m.update(schema=2)),
            "boolean schema": rewrite(lambda m: m.update(schema=True)),
            "extra field": rewrite(lambda m: m.update(note="x")),
            "boolean tick": rewrite(lambda m: m["children"]["go-build"].update(created_ticks=True)),
            "string tick": rewrite(lambda m: m["children"]["go-build"].update(created_ticks=str(self.tick))),
            "negative tick": rewrite(lambda m: m["children"]["go-build"].update(created_ticks=-1)),
            "float tick": rewrite(lambda m: m["children"]["go-build"].update(created_ticks=float(self.tick))),
            "missing inode": rewrite(lambda m: m["children"]["go-build"].pop("inode")),
            "unknown child": rewrite(lambda m: m["children"].update(other=m["children"]["go-build"])),
            "oversized": raw(b" " * (evidence.CACHE_MARKER_LIMIT + 1)),
        }
        for label, damage in cases.items():
            with self.subTest(label):
                shutil.rmtree(self.task_cache, ignore_errors=True)
                self.set_boot("boot-a")
                self.prepare()
                for name in evidence.DISPOSABLE_CACHE_CHILDREN[1:]:
                    shutil.rmtree(self.task_cache / name)
                damage()
                with self.assertRaisesRegex(evidence.Refused, "active: go-build .*4242:ambiguous"):
                    self.cleanup()
                self.assertTrue((self.task_cache / "go-build").is_dir())

    def test_foreign_owned_marker_omits_the_bound(self):
        self.prepare()
        self.process(4242, start=1)
        marker = self.marker_path().lstat()
        real_fstat = os.fstat

        def fstat(descriptor, *args, **kwargs):
            info = real_fstat(descriptor, *args, **kwargs)
            if (info.st_dev, info.st_ino) != (marker.st_dev, marker.st_ino):
                return info
            values = list(info)
            values[stat.ST_UID] = os.getuid() + 1
            return os.stat_result(values)
        with patch.object(evidence.os, "fstat", fstat):
            self.assertEqual(evidence.read_cache_marker(self.task_cache),
                             (None, "marker is not owned by the invoking user"))
            with self.assertRaisesRegex(evidence.Refused, "4242:ambiguous"):
                self.cleanup()
        self.assertEqual(self.cleanup(), list(evidence.DISPOSABLE_CACHE_CHILDREN))

    def test_non_proc_backends_never_use_the_marker(self):
        self.prepare()
        calls = []

        def inspector(path, proc=None, backend=None, uid=None, created_after=None, disregarded=None):
            calls.append(created_after)
            return []
        for backend in (validation_resources.DARWIN_BACKEND, validation_resources.UNAVAILABLE):
            with self.subTest(backend):
                calls.clear()
                with patch.object(validation_resources, "process_backend", return_value=backend), \
                        patch.object(validation_resources, "boot_id", side_effect=AssertionError("read")):
                    self.assertEqual(evidence.creation_bounds(self.task_cache, self.proc), {})
                    with patch.object(evidence, "active_cache_users", inspector):
                        evidence.cleanup_cache("fixture-task", self.task_cache, [], self.proc)
                self.assertEqual(calls, [None] * len(evidence.DISPOSABLE_CACHE_CHILDREN))
                self.prepare()

    def test_proc_backend_forwards_the_recorded_tick(self):
        self.prepare()
        calls = {}

        def inspector(path, proc=None, backend=None, uid=None, created_after=None, disregarded=None):
            calls[Path(path).name] = (created_after, uid)
            return []
        with patch.object(evidence, "active_cache_users", inspector):
            evidence.cleanup_cache("fixture-task", self.task_cache, [], self.proc)
        self.assertEqual(calls, {name: (self.tick, os.getuid()) for name in evidence.DISPOSABLE_CACHE_CHILDREN})

    @unittest.skipUnless(sys.platform.startswith("linux") and Path("/proc/self/stat").is_file(),
                         "real non-dumpable processes need Linux /proc")
    @unittest.skipIf(os.geteuid() == 0, "root reads a non-dumpable process's proc entries")
    def test_real_non_dumpable_process_before_creation_is_disregarded(self):
        """The SSH-host case with real processes: a non-dumpable process of the invoking user.

        Assertions name only this test's processes, so unrelated processes on
        the host cannot change the outcome.
        """
        program = ("import ctypes, sys, time\n"
                   "libc = ctypes.CDLL(None, use_errno=True)\n"
                   "if libc.prctl(4, 0, 0, 0, 0) != 0: sys.exit(3)\n"  # PR_SET_DUMPABLE
                   "print('ready', flush=True)\n"
                   "time.sleep(60)\n")

        def start():
            process = subprocess.Popen([sys.executable, "-c", program], cwd=self.base,
                                       stdout=subprocess.PIPE, text=True)
            self.addCleanup(process.stdout.close)
            self.addCleanup(process.wait, 5)
            self.addCleanup(process.kill)
            self.assertEqual(process.stdout.readline().strip(), "ready")
            return process

        before = start()
        try:
            os.readlink(f"/proc/{before.pid}/cwd")
            self.skipTest("this kernel lets the owner read a non-dumpable process's cwd")
        except PermissionError:
            pass
        time.sleep(0.1)  # Several clock ticks separate the start from the creation sample.
        result = evidence.prepare_cache("fixture-task", self.task_cache)
        self.assertEqual(result["recorded"], list(evidence.DISPOSABLE_CACHE_CHILDREN))
        after = start()
        child = self.task_cache / "go-build"
        bound = evidence.creation_bounds(self.task_cache)["go-build"]["created_ticks"]
        unbounded = evidence.active_cache_users(child, uid=os.getuid())
        bounded = evidence.active_cache_users(child, uid=os.getuid(), created_after=bound)
        self.assertIn(f"{before.pid}:ambiguous:cwd", unbounded)
        self.assertFalse([user for user in bounded if user.startswith(f"{before.pid}:")], bounded)
        self.assertIn(f"{after.pid}:ambiguous:cwd", bounded)


    @unittest.skipUnless(sys.platform.startswith("linux") and Path("/proc/self/stat").is_file(),
                         "real non-dumpable processes need Linux /proc")
    @unittest.skipIf(os.geteuid() == 0, "root reads a non-dumpable process's proc entries")
    def test_real_non_dumpable_process_between_preparations_is_disregarded(self):
        """The multi-round SSH-host case with real processes.

        The children are removed directly between rounds, because real
        cleanup would also inspect unrelated processes on the host.
        Assertions name only this test's processes.
        """
        program = ("import ctypes, sys, time\n"
                   "libc = ctypes.CDLL(None, use_errno=True)\n"
                   "if libc.prctl(4, 0, 0, 0, 0) != 0: sys.exit(3)\n"  # PR_SET_DUMPABLE
                   "print('ready', flush=True)\n"
                   "time.sleep(60)\n")

        def start():
            process = subprocess.Popen([sys.executable, "-c", program], cwd=self.base,
                                       stdout=subprocess.PIPE, text=True)
            self.addCleanup(process.stdout.close)
            self.addCleanup(process.wait, 5)
            self.addCleanup(process.kill)
            self.assertEqual(process.stdout.readline().strip(), "ready")
            return process

        children = list(evidence.DISPOSABLE_CACHE_CHILDREN)
        self.assertEqual(evidence.prepare_cache("fixture-task", self.task_cache)["recorded"], children)
        first = evidence.creation_bounds(self.task_cache)["go-build"]["created_ticks"]
        for name in children:
            shutil.rmtree(self.task_cache / name)
        time.sleep(0.1)  # Several clock ticks separate the first bound from the next start.
        between = start()
        try:
            os.readlink(f"/proc/{between.pid}/cwd")
            self.skipTest("this kernel lets the owner read a non-dumpable process's cwd")
        except PermissionError:
            pass
        time.sleep(0.1)
        self.assertEqual(evidence.prepare_cache("fixture-task", self.task_cache)["recorded"], children)
        after = start()
        child = self.task_cache / "go-build"
        entry = evidence.creation_bounds(self.task_cache)["go-build"]
        info = child.lstat()
        self.assertEqual((entry["device"], entry["inode"]), (info.st_dev, info.st_ino))
        self.assertGreater(entry["created_ticks"], first)
        stale = evidence.active_cache_users(child, uid=os.getuid(), created_after=first)
        fresh = evidence.active_cache_users(child, uid=os.getuid(), created_after=entry["created_ticks"])
        self.assertIn(f"{between.pid}:ambiguous:cwd", stale)
        self.assertFalse([user for user in fresh if user.startswith(f"{between.pid}:")], fresh)
        self.assertIn(f"{after.pid}:ambiguous:cwd", fresh)

class CacheCleanupSSHSessionTests(unittest.TestCase):
    """Cache cleanup with uninspectable OpenSSH session processes of the invoking user.

    The fixture /proc tree is the shared one from the validation_resources
    suite; only process-entry owners are injected. The real inspector runs
    every case, under owner isolation (TemporaryDirectory is 0700).
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        environment = patch.dict(os.environ, {"XDG_CACHE_HOME": str(self.base / "cache-home")})
        environment.start()
        self.addCleanup(environment.stop)
        self.task_cache = self.base / "cache-home" / "conveyor" / "fixture-task"
        self.proc = FixtureProc(self.base / "proc")
        (self.proc.root / "sys" / "kernel" / "random").mkdir(parents=True)
        (self.proc.root / "sys" / "kernel" / "random" / "boot_id").write_text("boot-a\n")
        (self.proc.root / "uptime").write_text("1000.00 0\n")
        self.tick = 1000 * os.sysconf("SC_CLK_TCK")
        self.lines = []

    def line(self, pid, parent, command="sshd-session", parent_command="sshd-session"):
        return (f"Disregarded Linux SSH session: pid={pid} parent={parent} command={command} "
                f"parent_command={parent_command} reason=same-user-uninspectable-with-live-root-owned-ssh-parent")

    def prepare(self):
        return evidence.prepare_cache("fixture-task", self.task_cache, self.proc.root)

    def cleanup(self, references=()):
        with self.proc.owned():
            return evidence.cleanup_cache("fixture-task", self.task_cache, list(references), self.proc.root,
                                          report=self.lines.append)

    def main(self, *references):
        stdout, stderr = StringIO(), StringIO()
        argv = ["validation_evidence.py", "cleanup", "--task", "fixture-task", "--task-cache", str(self.task_cache)]
        for reference in references:
            argv += ["--reference", str(reference)]
        with patch.object(sys, "argv", argv), self.proc.owned(), \
                patch.object(evidence, "cleanup_cache", functools.partial(evidence.cleanup_cache, proc=self.proc.root)), \
                patch("sys.stdout", stdout), patch("sys.stderr", stderr):
            status = evidence.main()
        return status, stdout.getvalue(), stderr.getvalue()

    def test_sessions_newer_than_the_cache_are_disregarded_and_reported_once(self):
        children = list(evidence.DISPOSABLE_CACHE_CHILDREN)
        self.assertEqual(self.prepare()["recorded"], children)
        unknown = self.task_cache / "retained-unknown-child"
        unknown.mkdir()
        durable = self.base / "state" / "conveyor" / "fixture-task" / "command.log"
        durable.parent.mkdir(parents=True)
        durable.write_text("evidence")
        marker = (self.task_cache / evidence.CACHE_MARKER).read_bytes()
        # Both sessions start after every child's creation tick, so the
        # creation bound cannot disregard them.
        self.proc.ssh(4242, 100, start=self.tick + 5, parent_start=self.tick + 4)
        self.proc.ssh(4343, 101, child_command="sshd", parent_command="sshd", start=self.tick + 9,
                      parent_start=1)
        self.assertEqual(self.cleanup([durable]), children)
        self.assertEqual(self.lines, [self.line(4242, 100), self.line(4343, 101, "sshd", "sshd")])
        for name in children:
            self.assertFalse((self.task_cache / name).exists())
        self.assertTrue(unknown.is_dir())
        self.assertEqual((self.task_cache / evidence.CACHE_MARKER).read_bytes(), marker)
        self.assertEqual(durable.read_text(), "evidence")

    def test_cli_prints_each_disregarded_session_before_the_removal_summary(self):
        self.prepare()
        self.proc.ssh(4242, 100, start=self.tick + 5)
        status, stdout, stderr = self.main()
        self.assertEqual((status, stderr), (0, ""))
        self.assertEqual(stdout.splitlines(), [
            self.line(4242, 100),
            "Removed disposable cache children: " + ", ".join(evidence.DISPOSABLE_CACHE_CHILDREN)])

    def test_later_child_refusal_keeps_the_reported_sessions(self):
        self.prepare()
        (self.task_cache / "tmp" / "object").write_text("cached")
        self.proc.ssh(4242, 100, start=self.tick + 5)
        # A readable build process holds a descriptor in tmp, which is inspected after go-build.
        self.proc.entry(4545, command="go", ppid=4242, start=self.tick + 6, cwd=self.base, root=Path("/"),
                        descriptor=self.task_cache / "tmp" / "object", environ=b"")
        with self.assertRaisesRegex(evidence.Refused, r"active: tmp \(4545:fd:3\)"):
            self.cleanup()
        self.assertEqual(self.lines, [self.line(4242, 100)])
        status, stdout, stderr = self.main()
        self.assertEqual(status, 2)
        self.assertEqual(stdout.splitlines(), [self.line(4242, 100)])
        self.assertIn("disposable cache child is active: tmp (4545:fd:3)", stderr)
        for name in evidence.DISPOSABLE_CACHE_CHILDREN:
            self.assertTrue((self.task_cache / name).is_dir())

    def test_unverified_sessions_and_readable_references_still_refuse(self):
        self.prepare()
        (self.task_cache / "go-build" / "object").write_text("cached")
        cases = {
            "user-owned parent": (dict(parent_uid=os.getuid()), "4242:ambiguous:cwd"),
            "root-owned systemd parent": (dict(parent_command="systemd"), "4242:ambiguous:cwd"),
            "other command": (dict(child_command="bash"), "4242:ambiguous:cwd"),
            "readable cwd": (dict(cwd=self.task_cache / "go-build"), "4242:cwd"),
            "readable descriptor beside unreadable entries": (
                dict(descriptor=self.task_cache / "go-build" / "object"), "4242:fd:3"),
        }
        for label, (ssh, reason) in cases.items():
            with self.subTest(label):
                self.lines.clear()
                self.proc.ssh(4242, 100, start=self.tick + 5, **ssh)
                with self.assertRaisesRegex(evidence.Refused, "active: go-build .*" + reason):
                    self.cleanup()
                self.assertEqual(self.lines, [])
                shutil.rmtree(self.proc.root / "4242")
                shutil.rmtree(self.proc.root / "100")
        # A parent that exited leaves the session ambiguous too.
        self.proc.entry(4242, command="sshd-session", ppid=100, start=self.tick + 5)
        with self.assertRaisesRegex(evidence.Refused, "4242:ambiguous:cwd"):
            self.cleanup()
        self.assertEqual(self.lines, [])
        self.assertTrue((self.task_cache / "go-build" / "object").is_file())


class MakeGraphTests(unittest.TestCase):
    # Exercise both the local default and .github/workflows/ci.yml explicitly,
    # regardless of the environment that launches this test suite.
    playwright_installs = (
        ("", "npx playwright install chromium"),
        ("--with-deps", "npx playwright install --with-deps chromium"),
    )

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        shutil.copyfile(REPO / "Makefile", self.root / "Makefile")
        for directory in ("web", "internal/httpapi/dashboard", "scripts", "tools"):
            (self.root / directory).mkdir(parents=True)
        (self.root / "internal/httpapi/dashboard/index.html").write_text("fresh")
        (self.root / "scripts/test-install.sh").write_text('exit "${INSTALL_EXIT:-0}"\n')
        run(self.root, "git", "init", "-b", "task")
        run(self.root, "git", "add", ".")
        run(self.root, "git", "-c", "user.name=Fixture", "-c", "user.email=f@example.invalid", "commit", "-m", "fixture")
        self.log = self.root / "calls"
        self.env = dict(os.environ, INVOCATIONS=str(self.log), PATH=str(self.root / "tools") + os.pathsep + os.environ["PATH"])
        self.env["PLAYWRIGHT_INSTALL_ARGS"] = ""
        stub = '''#!PYTHON_EXECUTABLE
import json, os, pathlib, sys, time
name = pathlib.Path(sys.argv[0]).name
call = name + ' ' + ' '.join(sys.argv[1:])
with open(os.environ['INVOCATIONS'], 'a') as f: f.write(json.dumps(call)+'\\n')
if os.environ.get('FAIL') == call: sys.exit(7)
root = pathlib.Path(os.environ['INVOCATIONS']).parent
if call == 'npm ci':
    time.sleep(0.03)
    (root/'installed').touch()
if call == 'npm run build':
    assert (root/'installed').exists()
    time.sleep(0.03)
    (root/'built').touch()
    if os.environ.get('DRIFT'): (root/'internal/httpapi/dashboard/index.html').write_text('drift')
if name == 'go' and (sys.argv[1] in ['build', 'vet']):
    if os.environ.get('EXPECT_UI'): assert (root/'built').exists(), 'raced dashboard generation'
'''
        for tool in ("npm", "npx", "go", "gofmt", "python3"):
            path = self.root / "tools" / tool
            path.write_text(stub.replace("PYTHON_EXECUTABLE", sys.executable))
            path.chmod(0o755)

    def make(self, *targets, **variables):
        return subprocess.run(["make", *targets], cwd=self.root, env=dict(self.env, **variables), capture_output=True, text=True)

    def calls(self):
        return [json.loads(x) for x in self.log.read_text().splitlines()]

    def test_composite_once_serial_and_parallel_preserves_all_checks(self):
        for jobs, (install_args, install_call) in itertools.product(("-j1", "-j8"), self.playwright_installs):
            with self.subTest(jobs=jobs, install_args=install_args):
                self.log.unlink(missing_ok=True)
                for name in ("installed", "built"):
                    (self.root / name).unlink(missing_ok=True)
                result = self.make(jobs, "validate", EXPECT_UI="1", PLAYWRIGHT_INSTALL_ARGS=install_args)
                self.assertEqual(result.returncode, 0, result.stderr)
                calls = self.calls()
                self.assertEqual(calls.count("npm ci"), 1)
                self.assertEqual(calls.count("npm run build"), 1)
                for call in ("go vet ./...", "gofmt -l .", "go test ./...", "npm run lint", "npm run test:e2e --", "python3 scripts/validate_compose_isolation.py"):
                    self.assertIn(call, calls)
                self.assertEqual([x for x in calls if x.startswith("npx playwright install")], [install_call])
                self.assertTrue(any(x.startswith("npm run test:e2e") for x in calls))
                self.assertEqual(len([x for x in calls if x.startswith("go build")]), 2)
                self.assertNotIn("npm run typecheck", calls)  # Remains the test-web contract.

    def test_standalone_targets_prepare_and_fail(self):
        for target in ("build", "test", "test-web", "test-ui", "web-typecheck"):
            with self.subTest(target=target):
                self.log.unlink(missing_ok=True)
                result = self.make(target, FAIL="npm ci")
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("npm run build", self.calls())
        self.log.unlink(missing_ok=True)
        result = self.make("vet", "fmt-check")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("npm ci", self.calls())
        result = self.make("test-web")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("npm run typecheck", self.calls())

    def test_failure_propagates_and_drift_refused(self):
        for install_args, install_call in self.playwright_installs:
            for failure in ("npm ci", "npm run build", "go vet ./...", "go test ./...", "npm run lint", install_call, "npm run test:e2e --", "python3 scripts/validate_compose_isolation.py"):
                with self.subTest(install_args=install_args, failure=failure):
                    self.log.unlink(missing_ok=True)
                    result = self.make("validate", FAIL=failure, PLAYWRIGHT_INSTALL_ARGS=install_args)
                    self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn(failure, self.calls())
                    if failure == install_call:
                        self.assertNotIn("npm run lint", self.calls())
                        self.assertNotIn("npm run test:e2e --", self.calls())
        self.assertNotEqual(self.make("validate", INSTALL_EXIT="8").returncode, 0)
        self.assertNotEqual(self.make("validate", DRIFT="1").returncode, 0)
        self.assertNotEqual(self.make("test").returncode, 0)
        self.assertNotEqual(self.make("dashboard-fresh").returncode, 0)


if __name__ == "__main__":
    unittest.main()
