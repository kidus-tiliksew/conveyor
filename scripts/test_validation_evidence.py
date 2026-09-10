"""Deterministic evidence and actual Make-graph tests; no network or databases."""
import copy
import itertools
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import validation_evidence as evidence

REPO = Path(__file__).resolve().parents[1]


def run(root, *args, env=None):
    return subprocess.run(args, cwd=root, env=env, capture_output=True, text=True, check=True).stdout


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
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
        with self.assertRaises(OSError):
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
        (self.output / "command.log").write_text("fabricated success")
        self.refused("log")

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
