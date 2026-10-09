"""Reproducible fixtures for owned validation resources and explicit recovery.

ResourceTests use real local child processes and run in `make test-validation`.
DockerLifecycleTests use real Docker and PostgreSQL and run in
`make test-validation-docker`, which fails rather than skips without Docker
(component-verification-strategy, "Validation resource ownership and recovery").
"""

import errno
import functools
import json
import os
from pathlib import Path
import plistlib
import secrets
import shutil
import signal
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import time
import unittest
import unittest.mock
from unittest.mock import patch

import validation_resources as resources

HELPER = Path(resources.__file__).resolve()
SCRIPTS = HELPER.parent
ROOT = SCRIPTS.parent
HAS_PROC = Path("/proc").is_dir()
HAS_BACKEND = resources.process_backend() != resources.UNAVAILABLE
NO_BACKEND = "no process backend on this host (Linux /proc or macOS libproc)"
IS_DARWIN = resources.process_backend() == resources.DARWIN_BACKEND


def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    if resources.process_backend() == resources.DARWIN_BACKEND:
        return resources.darwin().state(pid) != "zombie"
    stat = Path(f"/proc/{pid}/stat")
    try:
        return stat.read_text().rsplit(")", 1)[1].split()[0] != "Z"
    except OSError:
        return not HAS_PROC


def wait_for(predicate, timeout=10.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.05)
    return predicate()


class IsolatedState(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        self.checkout = self.base / "checkout"
        self.checkout.mkdir()
        self.env = dict(os.environ)
        self.env.pop(resources.BINDING, None)
        self.env.update({"XDG_STATE_HOME": str(self.base / "state"),
                         resources.TMP_ROOT: str(self.base / "cache"),
                         resources.ALLOW_RAM_TMP: "1"})
        isolation = patch.dict(os.environ, self.env, clear=True)
        isolation.start()
        self.addCleanup(isolation.stop)
        self.pids = []
        self.addCleanup(self.kill_leftovers)

    def kill_leftovers(self):
        for pid in self.pids:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass

    def invocations(self, task="resource-task"):
        root = self.base / "state" / "conveyor" / task / "invocations"
        return sorted(root.iterdir()) if root.is_dir() else []

    def launcher(self, *command, task="resource-task", options=()):
        # Output goes to files: an orphaned child keeps inherited descriptors
        # open after its owner is killed, so pipes would never reach EOF.
        log = (self.base / f"{task}-{secrets.token_hex(3)}.log").open("w")
        self.addCleanup(log.close)
        process = subprocess.Popen([sys.executable, str(HELPER), "launch", "--task", task, "--grace", "0.3",
                                    "--checkout", str(self.checkout), *options, "--", *command],
                                   cwd=self.checkout, env=self.env, stdin=subprocess.DEVNULL,
                                   stdout=log, stderr=log)
        process.log = Path(log.name)
        self.pids.append(process.pid)
        return process

    def finished(self, process, timeout=20):
        process.wait(timeout=timeout)
        return process.log.read_text()

    def child_script(self, pid_file, *, ignore_term=False, exit_code=None, descendant=False):
        lines = ["import os, pathlib, signal, subprocess, sys, time"]
        if ignore_term:
            lines.append("signal.signal(signal.SIGTERM, signal.SIG_IGN)")
        if descendant:
            lines.append("p = subprocess.Popen(['sleep', '30'])")
            lines.append(f"pathlib.Path({str(pid_file)!r}).write_text(str(p.pid))")
        else:
            lines.append(f"pathlib.Path({str(pid_file)!r}).write_text(str(os.getpid()))")
        lines.append(f"sys.exit({exit_code})" if exit_code is not None else "time.sleep(30)")
        return [sys.executable, "-c", "\n".join(lines)]

    def read_pid(self, pid_file):
        self.assertTrue(wait_for(pid_file.exists), "supervised command did not start")
        pid = int(pid_file.read_text())
        self.pids.append(pid)
        return pid


class ResourceTests(IsolatedState):
    def test_inventory_is_owner_only_sanitized_and_exclusive(self):
        first = resources.Invocation.create("resource-task", self.checkout,
                                            ["make", "postgres://admin:fixture-secret@db/x_test", "check"])
        second = resources.Invocation.create("resource-task", self.checkout, ["make"])
        self.addCleanup(second.finish)
        self.assertNotEqual(first.path, second.path)
        self.assertEqual(first.path.stat().st_mode & 0o777, 0o700)
        self.assertEqual((first.path / "inventory.json").stat().st_mode & 0o777, 0o600)
        text = (first.path / "inventory.json").read_text()
        # macOS canonical temporary paths contain "/private/", so the secret is distinct.
        self.assertNotIn("fixture-secret", text)
        inventory = json.loads(text)
        self.assertEqual(inventory["argv"][1], "[redacted]")
        self.assertEqual(inventory["owner"]["pid"], os.getpid())
        self.assertEqual(inventory["checkout"], str(self.checkout))
        tmp = [entry for entry in inventory["resources"] if entry["kind"] == "path"]
        self.assertEqual(len(tmp), 1)
        self.assertEqual(tmp[0]["state"], "sealed")
        self.assertEqual(tmp[0]["identity"]["inode"], Path(tmp[0]["identity"]["path"]).stat().st_ino)
        managed = first.managed_env()
        self.assertEqual(managed[resources.BINDING], str(first.path))
        self.assertTrue(managed["TMPDIR"].startswith(str(self.base / "cache")))
        self.assertTrue(resources.owner_active(first.path, first.inventory))
        self.assertEqual(first.finish(outcome="success"), [])
        self.assertFalse(Path(tmp[0]["identity"]["path"]).exists())
        self.assertFalse(resources.owner_active(first.path, resources.load_inventory(first.path)))
        with self.assertRaises(FileExistsError):
            (first.path).mkdir()

    def test_launch_cleans_up_after_success_failure_timeout_and_surviving_descendant(self):
        cases = {
            "descendant": (dict(descendant=True, exit_code=0), (), 0),
            "failure": (dict(exit_code=3), (), 3),
            "timeout": (dict(), ("--timeout", "0.5"), 124),
        }
        for name, (script, options, expected) in cases.items():
            with self.subTest(case=name):
                pid_file = self.base / (name + ".pid")
                process = self.launcher(*self.child_script(pid_file, **script), task=name, options=options)
                output = self.finished(process)
                self.assertEqual(process.returncode, expected, output)
                pid = self.read_pid(pid_file)
                self.assertTrue(wait_for(lambda: not alive(pid), 2), f"{name}: supervised process survived")
                [path] = self.invocations(name)
                inventory = resources.load_inventory(path)
                self.assertEqual(inventory["state"], "completed")
                self.assertTrue(all(entry["state"] in ("removed", "absent") for entry in inventory["resources"]))
                tmp = next(entry for entry in inventory["resources"] if entry["kind"] == "path")
                self.assertFalse(Path(tmp["identity"]["path"]).exists())

    def test_sigint_and_sigterm_stop_a_child_that_ignores_term(self):
        for signum in (signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=signum.name):
                pid_file = self.base / (signum.name + ".pid")
                process = self.launcher(*self.child_script(pid_file, ignore_term=True), task=signum.name.lower())
                pid = self.read_pid(pid_file)
                process.send_signal(signum)
                output = self.finished(process)
                self.assertEqual(process.returncode, 128 + signum, output)
                self.assertTrue(wait_for(lambda: not alive(pid), 2), "TERM-ignoring child survived")
                [path] = self.invocations(signum.name.lower())
                inventory = resources.load_inventory(path)
                group = next(entry for entry in inventory["resources"] if entry["kind"] == "process-group")
                self.assertEqual(group["state"], "removed")
                self.assertIn("SIGKILL", group["detail"])
                self.assertEqual(inventory["outcome"], "interrupted")

    def test_unsealable_process_record_never_executes_the_command(self):
        invocation = resources.Invocation.create("resource-task", self.checkout, ["seal"])
        self.addCleanup(invocation.finish)
        marker = self.base / "executed"
        original = invocation.seal

        def refuse(rid, identity):
            if invocation.resource(rid)["kind"] == "process-group":
                raise OSError("injected durable write failure")
            return original(rid, identity)

        with patch.object(invocation, "seal", side_effect=refuse):
            with self.assertRaises(OSError):
                resources.start_process(invocation, ["touch", str(marker)], env=os.environ, cwd=self.base,
                                        role="gate")
        time.sleep(0.2)
        self.assertFalse(marker.exists(), "command ran without a sealed record")
        group = next(entry for entry in invocation.inventory["resources"] if entry["kind"] == "process-group")
        self.assertEqual(group["state"], "absent")

    def test_two_invocations_in_one_checkout_stop_and_recover_independently(self):
        pid_a, pid_b = self.base / "a.pid", self.base / "b.pid"
        first = self.launcher(*self.child_script(pid_a), task="shared")
        second = self.launcher(*self.child_script(pid_b), task="shared")
        child_a, child_b = self.read_pid(pid_a), self.read_pid(pid_b)
        first.send_signal(signal.SIGTERM)
        self.finished(first)
        self.assertFalse(alive(child_a))
        self.assertTrue(alive(child_b), "stopping one invocation reached the other")
        paths = self.invocations("shared")
        self.assertEqual(len(paths), 2)
        second_path = next(path for path in paths if resources.load_inventory(path)["state"] == "active")
        second_tmp = next(entry for entry in resources.load_inventory(second_path)["resources"] if entry["kind"] == "path")
        self.assertTrue(Path(second_tmp["identity"]["path"]).is_dir())
        second.kill()
        self.finished(second)
        self.assertTrue(alive(child_b), "SIGKILL of the owner cannot run teardown")
        complete, _ = resources.recover(second_path)
        self.assertTrue(complete)
        self.assertTrue(wait_for(lambda: not alive(child_b), 2))
        first_path = next(path for path in paths if path != second_path)
        self.assertEqual(resources.load_inventory(first_path)["state"], "completed")
        self.assertFalse((first_path / "recovery.jsonl").exists())

    def test_forced_owner_termination_is_inspectable_and_recovered_explicitly(self):
        pid_file = self.base / "orphan.pid"
        process = self.launcher(*self.child_script(pid_file), task="forced")
        child = self.read_pid(pid_file)
        [path] = self.invocations("forced")
        report = resources.inspect_invocation(path)
        self.assertTrue(report["owner_active"])
        self.assertEqual({entry["classification"] for entry in report["resources"]}, {"active"})
        with self.assertRaisesRegex(resources.Refusal, "owner is active"):
            resources.recover(path)
        process.kill()
        self.finished(process)
        before = (path / "inventory.json").read_bytes()
        report = resources.inspect_invocation(path)
        self.assertEqual((path / "inventory.json").read_bytes(), before, "inspection mutated the inventory")
        self.assertFalse(report["owner_active"])
        self.assertEqual({entry["classification"] for entry in report["resources"]}, {"abandoned"})
        self.assertTrue(alive(child))
        result = subprocess.run([sys.executable, str(HELPER), "recover", "--invocation", str(path), "--grace", "0.3"],
                                env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(wait_for(lambda: not alive(child), 2))
        recovered = resources.load_inventory(path)
        self.assertEqual(recovered["state"], "recovered")
        actions = [json.loads(line) for line in (path / "recovery.jsonl").read_text().splitlines()]
        self.assertEqual(sorted(action["kind"] for action in actions), ["path", "process-group"])
        complete, again = resources.recover(path)
        self.assertTrue(complete)
        self.assertEqual(again, [], "recovery of absent recorded resources must be idempotent")

    def orphaned_invocation(self, task):
        pid_file = self.base / (task + ".pid")
        process = self.launcher(*self.child_script(pid_file), task=task)
        child = self.read_pid(pid_file)
        process.kill()
        self.finished(process)
        [path] = self.invocations(task)
        return path, child

    def rewrite(self, path, change):
        value = json.loads((path / "inventory.json").read_text())
        change(value)
        resources.write_json(path / "inventory.json", value)

    def test_recovery_refuses_changed_process_identity_without_signaling(self):
        path, child = self.orphaned_invocation("birth")
        original = resources.load_inventory(path)

        def change_birth(value):
            for entry in value["resources"]:
                if entry["kind"] == "process-group":
                    entry["identity"]["birth"]["start_ticks"] += 1
                    entry["identity"]["uid"] = os.getuid() + 1
        self.rewrite(path, change_birth)
        complete, actions = resources.recover(path, grace=0.3)
        self.assertFalse(complete)
        self.assertTrue(alive(child), "recovery signaled a group whose identity changed")
        refused = {action["kind"]: action["detail"] for action in actions if action["action"] == "refused"}
        self.assertIn("do not match the sealed birth identity", refused["process-group"])
        # The surviving child still uses its inherited TMPDIR, so the path stays too.
        self.assertIn("in use", refused["path"])
        self.assertEqual(resources.load_inventory(path)["state"], "recovery-incomplete")
        resources.write_json(path / "inventory.json", original)
        complete, _ = resources.recover(path, grace=0.3)
        self.assertTrue(complete)
        self.assertTrue(wait_for(lambda: not alive(child), 2))

    def test_recovery_refuses_substituted_or_referenced_paths(self):
        for mode in ("symlink", "reference"):
            with self.subTest(mode=mode):
                path, child = self.orphaned_invocation(mode)
                tmp = Path(next(entry for entry in resources.load_inventory(path)["resources"]
                                if entry["kind"] == "path")["identity"]["path"])
                references = []
                sentinel = None
                if mode == "symlink":
                    foreign = self.base / "foreign"
                    foreign.mkdir()
                    sentinel = foreign / "keep"
                    sentinel.write_text("foreign")
                    for child_path in sorted(tmp.rglob("*"), reverse=True):
                        child_path.rmdir()
                    tmp.rmdir()
                    tmp.symlink_to(foreign)
                else:
                    sentinel = tmp / "tmp" / "evidence.log"
                    sentinel.write_text("retained")
                    references = [str(sentinel)]
                complete, actions = resources.recover(path, references, grace=0.3)
                self.assertFalse(complete)
                self.assertTrue(wait_for(lambda: not alive(child), 2), "verified process group was not recovered")
                self.assertEqual([action["kind"] for action in actions if action["action"] == "refused"], ["path"])
                self.assertTrue(sentinel.exists())

    def test_recovery_refuses_corrupt_legacy_foreign_and_symlinked_records(self):
        path, child = self.orphaned_invocation("records")
        original = (path / "inventory.json").read_bytes()
        (path / "inventory.json").write_text("{not json")
        with self.assertRaisesRegex(resources.Refusal, "missing or corrupt"):
            resources.recover(path)
        self.rewrite_raw(path, dict(json.loads(original), schema=0))
        with self.assertRaisesRegex(resources.Refusal, "legacy"):
            resources.recover(path)
        self.rewrite_raw(path, dict(json.loads(original), host={"hostname": "x", "machine": "0" * 64}))
        with self.assertRaisesRegex(resources.Refusal, "another host"):
            resources.recover(path)
        link = self.base / "linked-invocation"
        link.symlink_to(path)
        with self.assertRaisesRegex(resources.Refusal, "not a symlink"):
            resources.recover(link)
        value = json.loads(original)
        value["resources"].append({"id": "volume-1", "kind": "volume", "state": "sealed", "identity": {"name": "v"}})
        self.rewrite_raw(path, value)
        complete, actions = resources.recover(path, grace=0.3)
        self.assertFalse(complete)
        self.assertIn(("volume", "refused"), [(action["kind"], action["action"]) for action in actions])
        self.assertTrue(wait_for(lambda: not alive(child), 2))

    def rewrite_raw(self, path, value):
        (path / "inventory.json").write_text(json.dumps(value))

    def test_inspection_classifies_pending_and_ambiguous_without_cleanup_permission(self):
        invocation = resources.Invocation.create("resource-task", self.checkout, ["inspect"], tmp=False)
        pending = invocation.register("container", {"project": "conveyor-test-x"})
        ambiguous = invocation.register("network", {"project": "conveyor-test-x"})
        invocation._update_resource(ambiguous, state="ambiguous")
        os.close(invocation._owner_lock)
        invocation._owner_lock = None
        report = resources.inspect_invocation(invocation.path)
        states = {entry["id"]: entry["classification"] for entry in report["resources"]}
        self.assertEqual(states, {pending: "pending", ambiguous: "ambiguous"})
        self.assertIn(invocation.id, json.dumps(resources.inspect_task("resource-task")))
        with patch.object(resources, "docker") as docker:
            with patch.object(resources, "process_birth", return_value=None):
                complete, actions = resources.recover(invocation.path)
            docker.assert_not_called()
        self.assertFalse(complete)
        self.assertEqual({action["action"] for action in actions}, {"refused"})

    def test_default_recovery_honors_recorded_retained_references(self):
        # Recovery named only by its invocation must protect references the
        # inventory records, in both the current list and legacy configuration.
        for mode in ("references", "configuration"):
            with self.subTest(mode=mode):
                path, child = self.orphaned_invocation("recorded-" + mode)
                tmp = Path(next(entry for entry in resources.load_inventory(path)["resources"]
                                if entry["kind"] == "path")["identity"]["path"])
                sentinel = tmp / "tmp" / "evidence" / "manifest.json"
                sentinel.parent.mkdir()
                sentinel.write_text("retained")

                def record_reference(value, reference=str(sentinel.parent)):
                    if mode == "references":
                        value["references"] = [reference]
                    else:
                        value.pop("references", None)
                        value["configuration"]["evidence"] = reference
                self.rewrite(path, record_reference)
                self.assertEqual(resources.inspect_invocation(path)["references"], [str(sentinel.parent)])
                result = subprocess.run([sys.executable, str(HELPER), "recover", "--invocation", str(path),
                                         "--grace", "0.3"], env=self.env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertIn("retained reference", result.stdout)
                self.assertTrue(wait_for(lambda: not alive(child), 2), "verified process group was not recovered")
                self.assertEqual(sentinel.read_text(), "retained")
                actions = [json.loads(line) for line in (path / "recovery.jsonl").read_text().splitlines()]
                self.assertEqual([(a["kind"], a["action"]) for a in actions if a["action"] == "refused"],
                                 [("path", "refused")])
                self.assertEqual(resources.load_inventory(path)["state"], "recovery-incomplete")

    def test_owner_cleanup_reads_references_recorded_after_it_loaded(self):
        owner = resources.Invocation.create("resource-task", self.checkout, ["owner"])
        tmp = Path(owner.managed_env()["TMPDIR"]).parent
        sentinel = tmp / "tmp" / "evidence.log"
        sentinel.write_text("retained")
        # Another writer (a joined launcher, or a record from before durable
        # references) adds a reference the owner's in-memory view lacks.
        value = json.loads((owner.path / "inventory.json").read_text())
        value["references"] = [str(sentinel)]
        resources.write_json(owner.path / "inventory.json", value)
        self.assertEqual(owner.inventory["references"], [])
        failures = owner.finish(outcome="success")
        self.assertEqual(len(failures), 1)
        self.assertIn("retained reference", failures[0])
        self.assertEqual(sentinel.read_text(), "retained")
        self.assertEqual(resources.load_inventory(owner.path)["state"], "cleanup-failed")

    def test_retain_records_durably_and_refuses_disposable_placement(self):
        owner = resources.Invocation.create("resource-task", self.checkout, ["owner"])
        self.addCleanup(lambda: owner.owner and owner.finish())
        env = dict(os.environ, **{resources.BINDING: str(owner.path)})
        env.pop(resources.TMP_ROOT)  # The joined runner need not know the owner's root.
        joined = resources.Invocation.enter("resource-task", self.checkout, ["joined"], env=env)
        self.assertFalse(joined.owner)
        tmp = Path(owner.managed_env()["TMPDIR"])
        for disposable in (tmp / "evidence", tmp.parent, self.base / "cache" / "other", self.base):
            with self.subTest(reference=disposable):
                with self.assertRaisesRegex(resources.Refusal, "cannot live in disposable path"):
                    joined.retain(disposable)
        self.assertEqual(resources.load_inventory(owner.path)["references"], [])
        durable = self.base / "durable" / "evidence"
        self.assertEqual(joined.retain(durable), str(durable))
        joined.retain(durable)
        self.assertEqual(resources.load_inventory(owner.path)["references"], [str(durable)])
        self.assertEqual(owner.finish(), [])
        self.assertFalse(tmp.exists())

    def test_inherited_binding_from_another_checkout_is_never_joined(self):
        owner = resources.Invocation.create("resource-task", self.checkout, ["owner"])
        self.addCleanup(owner.finish)
        other = self.base / "other-checkout"
        other.mkdir()
        with self.assertRaisesRegex(resources.Refusal, "belongs to checkout"):
            resources.Invocation.join(owner.path, other)
        env = dict(os.environ, **{resources.BINDING: str(owner.path)})
        separate = resources.Invocation.enter("resource-task", other, ["separate"], env=env)
        self.addCleanup(separate.finish)
        self.assertTrue(separate.owner)
        self.assertNotEqual(separate.path, owner.path)
        # A different task label in the same checkout joins the one owner.
        nested = resources.Invocation.enter("manual-validation", self.checkout, ["nested"], env=env)
        self.assertFalse(nested.owner)
        self.assertEqual(nested.path, owner.path)

    def test_malformed_recorded_references_fail_closed(self):
        path, child = self.orphaned_invocation("malformed")
        self.rewrite(path, lambda value: value.update(references="not-a-list"))
        with self.assertRaisesRegex(resources.Refusal, "malformed retained references"):
            resources.recover(path, grace=0.3)
        self.assertTrue(alive(child), "recovery mutated resources of a malformed inventory")

    def test_join_requires_an_active_owner_and_binding_reaches_descendants(self):
        owner = resources.Invocation.create("resource-task", self.checkout, ["owner"])
        env = dict(os.environ, **{resources.BINDING: str(owner.path)})
        joined = resources.Invocation.enter("resource-task", self.checkout, ["joined"], env=env)
        self.assertFalse(joined.owner)
        self.assertEqual(joined.path, owner.path)
        out = self.base / "binding"
        supervised = resources.start_process(joined, ["sh", "-c", f'printf %s "${resources.BINDING}" > {out}'],
                                             env=dict(env, **joined.managed_env()), cwd=self.base, role="probe")
        supervised.process.wait(timeout=10)
        self.assertEqual(out.read_text(), str(owner.path))
        self.assertEqual(joined.cleanup(rids=joined.registered), [])
        owner.finish()
        fresh = resources.Invocation.enter("resource-task", self.checkout, ["after"], env=env)
        self.addCleanup(fresh.finish)
        self.assertTrue(fresh.owner)
        self.assertNotEqual(fresh.path, owner.path)

    def test_temporary_root_requires_disk_backing_or_explicit_override(self):
        mountinfo = self.base / "mountinfo"
        disk = self.base / "disk"
        ram = self.base / "ram"
        mountinfo.write_text(
            f"22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"
            f"40 22 0:40 / {ram} rw - tmpfs tmpfs rw\n"
        )
        env = {"XDG_STATE_HOME": str(self.base / "state"), resources.TMP_ROOT: str(disk)}
        decision = resources.resolve_tmp_root("task", self.checkout, env, mountinfo)
        self.assertEqual((decision["backing"], decision["override"]), ("disk", False))
        env[resources.TMP_ROOT] = str(ram)
        with self.assertRaisesRegex(resources.Refusal, f"RAM-backed.*{resources.TMP_ROOT}.*{resources.ALLOW_RAM_TMP}=1"):
            resources.resolve_tmp_root("task", self.checkout, env, mountinfo)
        env[resources.ALLOW_RAM_TMP] = "1"
        decision = resources.resolve_tmp_root("task", self.checkout, env, mountinfo)
        self.assertEqual((decision["backing"], decision["override"]), ("ram", True))
        self.assertIn("RAM-backed", decision["warning"])
        del env[resources.ALLOW_RAM_TMP]
        with self.assertRaisesRegex(resources.Refusal, "cannot be established"):
            resources.resolve_tmp_root("task", self.checkout, env, self.base / "missing-mountinfo")
        for inside in (self.base / "state" / "conveyor" / "x", self.checkout / "tmp"):
            env[resources.TMP_ROOT] = str(inside)
            with self.assertRaisesRegex(resources.Refusal, "inside"):
                resources.resolve_tmp_root("task", self.checkout, env, mountinfo)
        with patch.object(resources, "backing_filesystem", return_value="tmpfs"):
            with self.assertRaises(resources.Refusal):
                resources.Invocation.create("ram-task", self.checkout, ["x"],
                                            env={"XDG_STATE_HOME": str(self.base / "state"),
                                                 resources.TMP_ROOT: str(ram)})
        [refused] = self.invocations("ram-task")
        self.assertEqual(resources.load_inventory(refused)["outcome"].split(":", 1)[0], "refused")

    def test_postgres_budget_is_finite_and_validated(self):
        default = resources.postgres_budget({})
        self.assertEqual((default["memory"], default["tmpfs"]),
                         (resources.DEFAULT_POSTGRES_MEMORY, resources.DEFAULT_POSTGRES_TMPFS))
        self.assertLess(default["tmpfs_bytes"], default["memory_bytes"])
        self.assertEqual(resources.postgres_budget({"CONVEYOR_TEST_POSTGRES_MEMORY": "2g",
                                                    "CONVEYOR_TEST_POSTGRES_TMPFS_SIZE": "1g"})["memory_bytes"],
                         2 * 1024 ** 3)
        for memory, tmpfs in (("0", "1m"), ("-1", "1m"), ("unlimited", "1m"), ("1g", "1g"), ("512m", "1g"),
                              ("1.5g", "1m"), ("1t", "1m")):
            with self.subTest(memory=memory, tmpfs=tmpfs):
                with self.assertRaises(resources.Refusal):
                    resources.postgres_budget({"CONVEYOR_TEST_POSTGRES_MEMORY": memory,
                                               "CONVEYOR_TEST_POSTGRES_TMPFS_SIZE": tmpfs})

    def test_occupied_pinned_port_is_refused_and_auto_ports_differ(self):
        with socket.socket() as occupied:
            occupied.bind(("127.0.0.1", 0))
            occupied.listen()
            port = str(occupied.getsockname()[1])
            for variable in ("CONVEYOR_TEST_POSTGRES_PORT", "TEST_POSTGRES_PORT"):
                with self.assertRaisesRegex(resources.Refusal, "occupied"):
                    resources.select_port({variable: port})
        self.assertFalse(resources.select_port({})[1])

    def test_container_teardown_releases_its_database_without_a_separate_drop(self):
        invocation = resources.Invocation.create("resource-task", self.checkout, ["db"], tmp=False)
        container = invocation.register("container", {"project": "conveyor-test-x"})
        invocation.seal(container, {"id": "c" * 64})
        database = invocation.register("database", {"server": "invocation-container", "container": "c" * 64,
                                                    "backend": "postgres", "database": "conveyor_x_test"})
        invocation.seal(database, {"incarnation": "1"})
        with patch.object(resources, "remove_container", return_value=("removed", "removed")), \
                patch.object(resources, "drop_database") as drop:
            self.assertEqual(invocation.finish(), [])
        drop.assert_not_called()
        self.assertEqual(invocation.resource(database)["detail"], "removed with invocation container")

    def test_docker_teardown_refuses_identity_mismatch_and_never_removes_external_networks(self):
        identity = {"id": "a" * 64, "project": "conveyor-test-x"}
        calls = []

        def docker(args, **_kwargs):
            calls.append(args)
            info = {"Id": "a" * 64, "Config": {"Labels": {resources.LABEL_INVOCATION: "another",
                                                           resources.COMPOSE_PROJECT_LABEL: "conveyor-test-x"}}}
            return subprocess.CompletedProcess(args, 0, json.dumps([info]), "")

        with patch.object(resources, "docker", side_effect=docker):
            with self.assertRaisesRegex(resources.Refusal, "label"):
                resources.remove_container(identity, "mine")
            with self.assertRaisesRegex(resources.Refusal, "never removed"):
                resources.remove_network(dict(identity, external=True), "mine")
        self.assertFalse([call for call in calls if call[:1] in (["rm"], ["network", "rm"]) or "rm" in call])


def bsdinfo_bytes(*, status=2, pid=4242, ppid=1, uid=501, pgid=4242, start=(1_700_000_000, 123456)):
    raw = bytearray(resources.BSDINFO_SIZE)
    struct.pack_into("<5I", raw, 4, status, 0, pid, ppid, uid)
    struct.pack_into("<I", raw, 100, pgid)
    struct.pack_into("<2Q", raw, 120, *start)
    return bytes(raw)


def kinfo_bytes(*entries):
    raw = bytearray(resources.KINFO_PROC_SIZE * len(entries))
    for index, (pid, status) in enumerate(entries):
        offset = index * resources.KINFO_PROC_SIZE
        raw[offset + 36] = status
        struct.pack_into("<i", raw, offset + 40, pid)
    return bytes(raw)


def procargs_bytes(argv, environment):
    # argc, the exec path, alignment padding, argv, the environment, then Apple strings.
    return (struct.pack("<i", len(argv)) + b"/usr/bin/tool\0\0\0\0" + b"".join(value + b"\0" for value in argv)
            + b"".join(value + b"\0" for value in environment) + b"\0ptr_munge=\0")


def vnodepath_bytes(cwd, root=b""):
    raw = bytearray(resources.VNODEPATHINFO_SIZE)
    raw[resources.VNODE_INFO_SIZE:resources.VNODE_INFO_SIZE + len(cwd)] = cwd
    start = resources.VNODE_INFO_PATH_SIZE + resources.VNODE_INFO_SIZE
    raw[start:start + len(root)] = root
    return bytes(raw)


def fdpath_bytes(path):
    raw = bytearray(resources.FDVNODEPATH_SIZE)
    start = resources.PROC_FILEINFO_SIZE + resources.VNODE_INFO_SIZE
    raw[start:start + len(path)] = path
    return bytes(raw)


def statfs_bytes(fstype, mounted_on, mounted_from):
    raw = bytearray(resources.STATFS_SIZE)
    raw[72:72 + len(fstype)] = fstype
    raw[88:88 + len(mounted_on)] = mounted_on
    raw[1112:1112 + len(mounted_from)] = mounted_from
    return bytes(raw)


class FakeDarwin:
    """Scripted macOS facts for exercising the Darwin logic on any host.

    Each process maps to a dict with optional keys: status, uid, pgid, start,
    gone (state only), cwd, root, vnode_error, fds ({fd: (type, path or errno)}),
    environment (list of bytes, or None when KERN_PROCARGS2 fails).
    """

    def __init__(self, table, *, statfs=None):
        self.table = table
        self.statfs_result = statfs
        self.selectors = []

    def processes(self, selector, value):
        self.selectors.append((selector, value))
        rows = [(pid, entry.get("status", 2)) for pid, entry in self.table.items() if not entry.get("gone")]
        if selector == resources.KERN_PROC_PGRP:
            rows = [(pid, status) for pid, status in rows if self.table[pid].get("pgid") == value]
        elif selector == resources.KERN_PROC_UID:
            rows = [(pid, status) for pid, status in rows if self.table[pid].get("uid", value) == value]
        return rows

    def state(self, pid):
        entry = self.table.get(pid)
        if entry is None or entry.get("gone"):
            return "gone"
        return "zombie" if entry.get("status") == resources.SZOMB else "live"

    def bsdinfo(self, pid):
        entry = self.table.get(pid)
        if entry is None or entry.get("gone") or entry.get("status") == resources.SZOMB:
            return None
        return {"status": entry.get("status", 2), "pid": pid, "ppid": 1, "uid": entry.get("uid", 501),
                "pgid": entry.get("pgid", pid), "start": entry.get("start", 1)}

    def birth(self, pid):
        info = self.bsdinfo(pid)
        return None if info is None else {"start_ticks": info["start"], "boot_id": "boot"}

    def vnode_paths(self, pid):
        entry = self.table[pid]
        if entry.get("vnode_error"):
            return None
        return entry.get("cwd", "/"), entry.get("root", "")

    def descriptors(self, pid):
        return [(fd, kind) for fd, (kind, _value) in sorted(self.table[pid].get("fds", {}).items())]

    def descriptor_path(self, pid, fd):
        _kind, value = self.table[pid]["fds"][fd]
        return (None, value) if isinstance(value, int) else (value, 0)

    def procargs(self, pid):
        environment = self.table[pid].get("environment", [])
        return None if environment is None else (["tool"], environment)

    def statfs(self, path):
        return self.statfs_result


class DarwinParserTests(unittest.TestCase):
    """Portable fixtures for the macOS structure parsers; they run on every host."""

    def test_process_structures_parse_fixed_layouts_and_reject_other_sizes(self):
        info = resources.parse_bsdinfo(bsdinfo_bytes(status=3, pid=77, ppid=5, uid=501, pgid=70,
                                                     start=(1_700_000_000, 42)))
        self.assertEqual(info, {"status": 3, "pid": 77, "ppid": 5, "uid": 501, "pgid": 70,
                                "start": 1_700_000_000_000_042})
        self.assertIsNone(resources.parse_bsdinfo(bsdinfo_bytes()[:-1]))
        self.assertEqual(resources.parse_kinfo_procs(kinfo_bytes((70, 2), (71, resources.SZOMB))),
                         [(70, 2), (71, resources.SZOMB)])
        self.assertEqual(resources.parse_kinfo_procs(b""), [])
        self.assertIsNone(resources.parse_kinfo_procs(kinfo_bytes((70, 2)) + b"\0"))
        self.assertEqual(resources.parse_fdlist(struct.pack("<iIiI", 0, 1, 7, 2)), [(0, 1), (7, 2)])
        self.assertIsNone(resources.parse_fdlist(b"\0" * 7))

    def test_procargs2_environment_parses_and_withheld_environment_is_empty(self):
        binding = (resources.BINDING + "=/state/invocation").encode()
        argv, environment = resources.parse_procargs2(procargs_bytes([b"make", b"check"], [binding, b"GOCACHE=/c"]))
        self.assertEqual(argv, ["make", "check"])
        self.assertEqual(environment, [binding, b"GOCACHE=/c"])
        # macOS returns only argc, the exec path, and argv for a platform binary.
        withheld = struct.pack("<i", 2) + b"/bin/sleep\0\0\0" + b"sleep\0" + b"30\0"
        self.assertEqual(resources.parse_procargs2(withheld), (["sleep", "30"], []))
        self.assertIsNone(resources.parse_procargs2(b"\1\0"))
        self.assertIsNone(resources.parse_procargs2(struct.pack("<i", 3) + b"/bin/sleep\0" + b"sleep\0"))

    def test_vnode_descriptor_and_statfs_paths(self):
        self.assertEqual(resources.parse_vnodepathinfo(vnodepath_bytes(b"/private/tmp/work")), ("/private/tmp/work", ""))
        self.assertEqual(resources.parse_vnodepathinfo(vnodepath_bytes(b"/a", b"/jail")), ("/a", "/jail"))
        self.assertIsNone(resources.parse_vnodepathinfo(b"\0" * 10))
        self.assertEqual(resources.parse_fd_vnodepath(fdpath_bytes(b"/Users/u/.cache/x")), "/Users/u/.cache/x")
        self.assertIsNone(resources.parse_fd_vnodepath(b"\0" * 10))
        self.assertEqual(resources.parse_statfs(statfs_bytes(b"apfs", b"/System/Volumes/Data", b"/dev/disk3s5")),
                         {"fstype": "apfs", "mounted_on": "/System/Volumes/Data", "mounted_from": "/dev/disk3s5"})
        self.assertIsNone(resources.parse_statfs(b"\0" * 100))

    def test_ram_disk_images_and_backing_classification(self):
        info = plistlib.dumps({"images": [
            {"image-path": "ram://20480", "system-entities": [{"dev-entry": "/dev/disk6"},
                                                              {"dev-entry": "/dev/disk7s1", "mount-point": "/Volumes/ram"}]},
            {"image-path": "/Users/u/tool.dmg", "system-entities": [{"dev-entry": "/dev/disk4",
                                                                     "mount-point": "/Volumes/Tool"}]},
        ]})
        self.assertEqual(resources.ram_disk_entities(info), {"/dev/disk6", "/dev/disk7s1", "/Volumes/ram"})
        self.assertEqual(resources.ram_disk_entities(plistlib.dumps({})), set())
        self.assertIsNone(resources.ram_disk_entities(b"not a property list"))
        root = Path("/Volumes/ram/cache")
        self.assertEqual(resources.classify_backing(root, None, True), "unknown")
        self.assertEqual(resources.classify_backing(root, "tmpfs", True), "ram")
        self.assertEqual(resources.classify_backing(root, "ext4", False), "disk")
        self.assertEqual(resources.classify_backing(root, "smbfs", True), "unknown")

    def test_disk_topology_resolves_whole_disks_and_apfs_physical_stores(self):
        hfs = plistlib.dumps({"DeviceIdentifier": "disk6s1", "ParentWholeDisk": "disk6", "FilesystemType": "hfs"})
        self.assertEqual(resources.disk_topology(hfs), {"/dev/disk6s1", "/dev/disk6"})
        apfs = plistlib.dumps({"DeviceIdentifier": "disk3s5", "ParentWholeDisk": "disk3", "FilesystemType": "apfs",
                               "APFSContainerReference": "disk3",
                               "APFSPhysicalStores": [{"APFSPhysicalStore": "disk0s2"}]})
        self.assertEqual(resources.disk_topology(apfs), {"/dev/disk3s5", "/dev/disk3", "/dev/disk0s2", "/dev/disk0"})
        for name, value in {
            "apfs without stores": {"DeviceIdentifier": "disk7s1", "ParentWholeDisk": "disk7",
                                    "FilesystemType": "apfs", "APFSContainerReference": "disk7"},
            "malformed store": {"DeviceIdentifier": "disk7s1", "ParentWholeDisk": "disk7", "FilesystemType": "apfs",
                                "APFSPhysicalStores": [{"APFSPhysicalStore": "ram"}]},
            "no parent": {"DeviceIdentifier": "disk6s1", "FilesystemType": "hfs"},
            "not a disk": {"DeviceIdentifier": "map auto_home", "ParentWholeDisk": "disk6"},
        }.items():
            with self.subTest(case=name):
                self.assertIsNone(resources.disk_topology(plistlib.dumps(value)))
        self.assertIsNone(resources.disk_topology(b"not a property list"))

    def test_ram_image_backing_matches_whole_disks_physical_stores_and_fails_closed(self):
        images = plistlib.dumps({"images": [
            {"image-path": "ram://20480", "system-entities": [{"dev-entry": "/dev/disk6"}]},
            {"image-path": "ram://40960", "system-entities": [{"dev-entry": "/dev/disk9s1", "mount-point": "/Volumes/r9"}]},
            {"image-path": "/Users/u/tool.dmg", "system-entities": [{"dev-entry": "/dev/disk4"},
                                                                     {"dev-entry": "/dev/disk4s1",
                                                                      "mount-point": "/Volumes/Tool"}]},
        ]})

        def topology(identifier, parent, fstype="hfs", stores=None):
            value = {"DeviceIdentifier": identifier, "ParentWholeDisk": parent, "FilesystemType": fstype}
            if stores is not None:
                value.update(APFSContainerReference=parent,
                             APFSPhysicalStores=[{"APFSPhysicalStore": store} for store in stores])
            return plistlib.dumps(value)

        cases = {
            # The ram:// image lists only its whole disk; the root is on a partition of it.
            "partition of a ram whole disk": ("hfs", "/Volumes/ram", "/dev/disk6s1", topology("disk6s1", "disk6"), images, "ram"),
            "exact ram entity": ("hfs", "/Volumes/r9", "/dev/disk9s1", topology("disk9s1", "disk9"), images, "ram"),
            # An APFS volume on a synthesized container whose physical store is the ram disk.
            "apfs physical store on ram": ("apfs", "/Volumes/ram", "/dev/disk7s1",
                                           topology("disk7s1", "disk7", "apfs", ["disk6"]), images, "ram"),
            "internal apfs": ("apfs", "/System/Volumes/Data", "/dev/disk3s5",
                              topology("disk3s5", "disk3", "apfs", ["disk0s2"]), images, "disk"),
            "file-backed disk image": ("hfs", "/Volumes/Tool", "/dev/disk4s1", topology("disk4s1", "disk4"), images, "disk"),
            "apfs topology unresolved": ("apfs", "/Volumes/ram", "/dev/disk7s1", topology("disk7s1", "disk7", "apfs"),
                                         images, "unknown"),
            "diskutil failed": ("hfs", "/Volumes/ram", "/dev/disk6s1", None, images, "unknown"),
            "topology names another device": ("hfs", "/Volumes/ram", "/dev/disk6s1", topology("disk5s1", "disk5"),
                                              images, "unknown"),
            "hdiutil failed": ("apfs", "/System/Volumes/Data", "/dev/disk3s5",
                               topology("disk3s5", "disk3", "apfs", ["disk0s2"]), None, "unknown"),
            "not a device": ("apfs", "/System/Volumes/Data", "map auto_home", topology("disk3s5", "disk3"), images,
                             "unknown"),
        }
        root = Path("/Volumes/ram/cache")
        for name, (fstype, mounted_on, mounted_from, disk_info, image_info, expected) in cases.items():
            with self.subTest(case=name):
                statfs = resources.parse_statfs(statfs_bytes(fstype.encode(), mounted_on.encode(), mounted_from.encode()))
                outputs = {resources.DISKUTIL: disk_info, resources.HDIUTIL: image_info}

                def run(argv, **_kwargs):
                    output = outputs[argv[0]]
                    return subprocess.CompletedProcess(argv, 1 if output is None else 0, output or b"", b"")
                with patch.object(resources, "darwin", return_value=FakeDarwin({}, statfs=statfs)), \
                     patch.object(resources.subprocess, "run", side_effect=run):
                    self.assertEqual(resources.classify_backing(root, fstype, True), expected)
        with patch.object(resources, "darwin", return_value=FakeDarwin({}, statfs=None)):
            self.assertEqual(resources.classify_backing(root, "apfs", True), "unknown")
        statfs = resources.parse_statfs(statfs_bytes(b"apfs", b"/", b"/dev/disk3s1"))
        with patch.object(resources, "darwin", return_value=FakeDarwin({}, statfs=statfs)), \
             patch.object(resources.subprocess, "run", side_effect=FileNotFoundError(resources.DISKUTIL)):
            self.assertEqual(resources.classify_backing(root, "apfs", True), "unknown")


class DarwinLogicTests(unittest.TestCase):
    """The macOS cache-ownership and member-verification rules over scripted facts."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.cache = Path(self.tmp.name).resolve() / "cache"
        self.cache.mkdir()

    def test_cache_users_report_readable_users_and_ambiguous_live_failures(self):
        cache = str(self.cache)
        table = {
            os.getpid(): {"cwd": cache},
            101: {"cwd": cache + "/go-build"},
            102: {"cwd": "/", "fds": {3: (resources.PROX_FDTYPE_VNODE, cache + "/x"),
                                      4: (resources.PROX_FDTYPE_VNODE, errno.EPERM),
                                      5: (resources.PROX_FDTYPE_VNODE, errno.EBADF),
                                      6: (resources.PROX_FDTYPE_VNODE, errno.EIO),
                                      7: (2, cache + "/socket"),
                                      8: (resources.PROX_FDTYPE_VNODE, "")}},
            103: {"cwd": cache, "environment": [b"GOCACHE=" + cache.encode() + b"/go", b"TMPDIR=t"]},
            104: {"cwd": "/", "environment": []},
            105: {"vnode_error": True, "environment": None},
            106: {"cwd": "/", "environment": [b"TMPDIR=relative"], "vnode_error": True},
            107: {"status": resources.SZOMB, "cwd": cache},
            108: {"cwd": "/", "root": cache},
        }
        fake = FakeDarwin(table)
        with patch.object(resources, "darwin", return_value=fake):
            users = resources._darwin_cache_users(self.cache, None)
            self.assertEqual(fake.selectors, [(resources.KERN_PROC_UID, os.getuid())])
            self.assertEqual(sorted(users), sorted([
                "101:cwd", "102:fd:3", "102:ambiguous:fd:6", "103:cwd", "103:env:GOCACHE", "103:env:TMPDIR",
                "105:ambiguous:cwd", "105:ambiguous:environ", "106:ambiguous:cwd", "106:ambiguous:env:TMPDIR",
                "108:root"]))
            table[105]["gone"] = True
            table[106]["gone"] = True
            self.assertNotIn("105:ambiguous:cwd", resources._darwin_cache_users(self.cache, None))

    def test_unrelated_filter_uses_darwin_start_and_session(self):
        table = {201: {"vnode_error": True, "start": 10}, 202: {"vnode_error": True, "start": 30}}
        sessions = {201: 7, 202: 9}
        with patch.object(resources, "darwin", return_value=FakeDarwin(table)), \
             patch.object(resources.os, "getsid", side_effect=lambda pid: sessions[pid]):
            users = resources.active_cache_users(self.cache, created_after=20, sessions={9},
                                                 backend=resources.DARWIN_BACKEND)
        self.assertEqual(users, ["202:ambiguous:cwd"])

    def test_member_verification_requires_birth_binding_or_pinned_group(self):
        reference = "/state/conveyor/task/invocations/one"
        binding = (resources.BINDING + "=" + reference).encode()
        table = {
            500: {"pgid": 500, "start": 5},
            501: {"pgid": 500, "environment": [binding]},
            502: {"pgid": 500, "environment": []},
            503: {"pgid": 500, "uid": 0, "environment": [binding]},
            504: {"pgid": 500, "environment": [binding]},
            505: {"pgid": 600, "environment": [binding]},
        }
        identity = {"pgid": 500, "leader": 500, "birth": {"start_ticks": 5, "boot_id": "boot"}, "uid": 501}
        sessions = {501: 500, 502: 500, 503: 500, 504: 999, 505: 500}
        fake = FakeDarwin(table)
        with patch.object(resources, "darwin", return_value=fake), \
             patch.object(resources, "process_birth", side_effect=fake.birth), \
             patch.object(resources.os, "getsid", side_effect=lambda pid: sessions[pid]):
            verified = {pinned: [pid for pid in sorted(table)
                                 if resources._darwin_member_verified(pid, identity, reference, pinned)]
                        for pinned in (False, True)}
            self.assertEqual(verified[False], [500, 501])
            self.assertEqual(verified[True], [500, 501, 502])
            changed = dict(identity, birth={"start_ticks": 6, "boot_id": "boot"})
            self.assertFalse(resources._darwin_member_verified(500, changed, reference, True))

    def test_stop_refuses_an_unlisted_group_and_an_unverified_member(self):
        identity = {"pgid": 700, "leader": 700, "birth": {"start_ticks": 1, "boot_id": "boot"}, "uid": 501}
        fake = FakeDarwin({701: {"pgid": 700, "environment": []}})
        sessions = {701: 700}
        with patch.object(resources, "darwin", return_value=fake), \
             patch.object(resources, "process_birth", side_effect=fake.birth), \
             patch.object(resources.os, "getsid", side_effect=lambda pid: sessions[pid]), \
             patch.object(resources.os, "killpg") as killpg:
            ok, detail = resources.stop_group(identity, "/ref", grace=0.1, kill_wait=0.1,
                                              backend=resources.DARWIN_BACKEND)
            self.assertFalse(ok)
            self.assertIn("members [701]", detail)
            with patch.object(fake, "processes", return_value=None):
                ok, detail = resources.stop_group(identity, "/ref", backend=resources.DARWIN_BACKEND)
            self.assertFalse(ok)
            self.assertIn("cannot be listed", detail)
            killpg.assert_not_called()

    def test_stop_relists_before_refusing_a_member_that_exited_during_verification(self):
        identity = {"pgid": 800, "leader": 800, "birth": {"start_ticks": 1, "boot_id": "boot"}, "uid": 501}
        table = {801: {"pgid": 800, "environment": []}, 802: {"pgid": 800, "environment": []}}

        class Exiting(FakeDarwin):
            def bsdinfo(self, pid):
                if pid == 802:
                    table[802]["gone"] = True  # 802 exits while it is being verified.
                    return None
                return super().bsdinfo(pid)

        fake = Exiting(table)
        signaled = []

        def killpg(pgid, sig):
            signaled.append(sig)
            table[801]["gone"] = True
        with patch.object(resources, "darwin", return_value=fake), \
             patch.object(resources, "process_birth", side_effect=fake.birth), \
             patch.object(resources, "_leader_pinned", return_value=True), \
             patch.object(resources.os, "getsid", return_value=800), \
             patch.object(resources.os, "killpg", side_effect=killpg):
            ok, detail = resources.stop_group(identity, "/ref", grace=0.5, kill_wait=0.5,
                                              backend=resources.DARWIN_BACKEND)
        self.assertTrue(ok, detail)
        self.assertEqual(signaled, [signal.SIGTERM])
        # A member still present after the re-list stays a refusal.
        table = {803: {"pgid": 800, "uid": 0, "environment": []}}
        fake = FakeDarwin(table)
        with patch.object(resources, "darwin", return_value=fake), \
             patch.object(resources, "process_birth", side_effect=fake.birth), \
             patch.object(resources, "_leader_pinned", return_value=True), \
             patch.object(resources.os, "getsid", return_value=800), \
             patch.object(resources.os, "killpg") as killpg_mock:
            ok, detail = resources.stop_group(identity, "/ref", backend=resources.DARWIN_BACKEND)
        self.assertFalse(ok)
        self.assertIn("members [803]", detail)
        killpg_mock.assert_not_called()

    def test_unavailable_backend_keeps_the_refusals(self):
        with self.assertRaisesRegex(resources.Refusal, "requires /proc or macOS libproc"):
            resources.active_cache_users(self.cache, backend=resources.UNAVAILABLE)
        identity = {"pgid": 4242, "leader": 4242}
        with patch.object(resources.os, "killpg", side_effect=ProcessLookupError) as killpg:
            ok, detail = resources.stop_group(identity, "/ref", backend=resources.UNAVAILABLE)
        self.assertEqual((ok, detail), (True, "process group 4242 has no remaining members"))
        killpg.assert_called_once_with(4242, 0)  # Existence probe only; no signal.


class LeaderPinTests(unittest.TestCase):
    def test_unreaped_leader_pins_its_group_until_reaped(self):
        process = subprocess.Popen([sys.executable, "-c", "pass"], start_new_session=True)
        self.addCleanup(lambda: process.returncode is None and process.wait())
        identity = {"pgid": process.pid, "leader": process.pid, "birth": None}
        self.assertTrue(resources._leader_pinned(identity, process))
        self.assertTrue(wait_for(lambda: os.waitid(os.P_PID, process.pid,
                                                   os.WEXITED | os.WNOHANG | os.WNOWAIT) is not None, 10))
        self.assertTrue(resources._leader_pinned(identity, process), "an exited, unreaped leader still pins")
        process.wait()
        self.assertFalse(resources._leader_pinned(identity, process))
        self.assertFalse(resources._leader_pinned(dict(identity, leader=process.pid + 1), None))


@unittest.skipUnless(HAS_BACKEND, NO_BACKEND)
class HostBackendTests(IsolatedState):
    """Real processes against the host backend: Linux /proc or macOS libproc."""

    def test_birth_identity_is_stable_and_distinct_per_process(self):
        first = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(5)"])
        time.sleep(0.05)  # Linux start times advance in clock ticks (usually 10 ms).
        second = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(5)"])
        for process in (first, second):
            self.addCleanup(process.wait)
            self.addCleanup(process.kill)
        birth = resources.process_birth(first.pid)
        self.assertIsNotNone(birth)
        self.assertIsNotNone(birth["boot_id"])
        self.assertEqual(resources.process_birth(first.pid), birth)
        self.assertNotEqual(resources.process_birth(second.pid), birth)
        self.assertLessEqual(birth["start_ticks"], resources.current_ticks())

    def survivor_group(self):
        """A sealed group whose leader exits while a binding-free sleep survives in the group."""
        invocation = resources.Invocation.create("resource-task", self.checkout, ["group"])
        self.addCleanup(invocation.finish)
        pid_file = self.base / "survivor.pid"
        script = f"env -u {resources.BINDING} sleep 30 & echo $! > {pid_file}.tmp && mv {pid_file}.tmp {pid_file}"
        supervised = resources.start_process(invocation, ["/bin/sh", "-c", script], env=os.environ,
                                             cwd=self.base, role="gate")
        survivor = self.read_pid(pid_file)
        self.assertTrue(wait_for(lambda: supervised.exit_status() is not None, 10), "leader did not exit")
        return invocation, supervised, survivor

    @unittest.skipUnless(IS_DARWIN, "the pinned group is the macOS substitute for the invocation binding, which "
                                    "macOS withholds for platform binaries; Linux verifies every member's binding")
    def test_unreaped_leader_pins_group_so_environment_withheld_survivor_is_stopped(self):
        _invocation, supervised, survivor = self.survivor_group()
        ok, detail = supervised.stop(grace=0.3)
        self.assertTrue(ok, detail)
        self.assertTrue(wait_for(lambda: not alive(survivor), 5), "pinned survivor was not stopped")
        self.assertIsNotNone(supervised.process.returncode, "stop() reaps the leader after teardown")

    def test_survivor_without_binding_is_refused_after_the_leader_is_reaped(self):
        invocation, supervised, survivor = self.survivor_group()
        supervised.process.wait()
        identity = invocation.resource(supervised.rid)["identity"]
        ok, detail = resources.stop_group(identity, str(invocation.path), process=supervised.process, grace=0.3)
        self.assertFalse(ok)
        self.assertIn(f"members [{survivor}]", detail)
        self.assertTrue(alive(survivor), "an unverified survivor was signaled")

    def test_active_cache_users_report_cwd_descriptor_and_environment_users(self):
        cache = self.base / "cache-child"
        cache.mkdir()
        sleeper = [sys.executable, "-c", "import time; time.sleep(30)"]
        held = (cache / "held").open("w")
        self.addCleanup(held.close)
        processes = {
            "cwd": subprocess.Popen(sleeper, cwd=cache),
            "fd:1": subprocess.Popen(sleeper, cwd=self.base, stdout=held),
            "env:GOCACHE": subprocess.Popen(sleeper, cwd=self.base, env=dict(os.environ, GOCACHE=str(cache))),
        }
        for process in processes.values():
            self.addCleanup(process.wait)
            self.addCleanup(process.kill)
        expected = {f"{process.pid}:{label}" for label, process in processes.items()}
        self.assertTrue(wait_for(lambda: expected <= set(resources.active_cache_users(cache)), 10),
                        resources.active_cache_users(cache))

    def test_host_temporary_root_backing_is_established(self):
        filesystem = resources.backing_filesystem(self.base)
        self.assertIsNotNone(filesystem)
        backing = resources.classify_backing(self.base, filesystem, resources._darwin_mounts(resources.MOUNTINFO))
        self.assertNotEqual(backing, "unknown")
        if IS_DARWIN:
            self.assertEqual(backing, "disk")


# The coordinator's observation of a real user manager on the development host
# (2026-10-08): ExecMainStartTimestampMonotonic microseconds against /proc start
# ticks at CLK_TCK 100. Fixtures only; no test reads the host's manager.
MANAGER_PID = 2586874
MANAGER_START = 1168414290
MANAGER_USEC = 11684142907759
MANAGER_HZ = 100
USER_SERVICE_CGROUP = "0::/user.slice/user-{uid}.slice/user@{uid}.service/init.scope\n"
# The same observation of the manager's "(sd-pam)" PAM helper (2026-10-09): two
# ticks after the manager. The earliest other child of that manager started at
# tick 1200011223.
HELPER_PID = 2586876
HELPER_START = 1168414292
LATER_SERVICE_START = 1200011223


def manager_service(main_pid=MANAGER_PID, monotonic_usec=MANAGER_USEC, active_state="active"):
    return {"main_pid": main_pid, "monotonic_usec": monotonic_usec, "active_state": active_state}


class FixtureManagerQuery:
    """An injected system-manager query.

    It answers in order and then repeats its last answer, records the uid of
    every call, and runs between, when given, after the first call and before
    the second answer, so a case can change a fact between the two reads
    deterministically.
    """

    def __init__(self, *answers, between=None):
        self.answers = list(answers) or [None]
        self.between = between
        self.calls = []

    def __call__(self, uid):
        self.calls.append(uid)
        if len(self.calls) == 2 and self.between is not None:
            self.between()
        answer = self.answers[min(len(self.calls), len(self.answers)) - 1]
        return None if answer is None else dict(answer)


def manager_line(pid=MANAGER_PID, start=MANAGER_START, usec=MANAGER_USEC, uid=None):
    uid = os.getuid() if uid is None else uid
    return (f"Disregarded Linux user manager: pid={pid} uid={uid} service=user@{uid}.service main_pid={pid} "
            f"start={start} monotonic_usec={usec} reason=same-user-uninspectable-system-manager-reported-mainpid")


def helper_line(pid=HELPER_PID, start=HELPER_START, parent=MANAGER_PID, parent_start=MANAGER_START, hz=MANAGER_HZ,
                uid=None):
    uid = os.getuid() if uid is None else uid
    return (f"Disregarded Linux user manager helper: pid={pid} parent={parent} uid={uid} command=(sd-pam) "
            f"start={start} parent_start={parent_start} clk_tck={hz} "
            "reason=same-user-uninspectable-sd-pam-child-of-authenticated-user-manager-within-one-second")


class FixtureProc:
    """A fixture /proc tree for the Linux inspector.

    Each entry gets a stat file (or a raw or missing one) and readable cwd,
    root, fd, and environ entries only when the case names them, so an entry
    without them is uninspectable like a non-dumpable process. Owners come
    from the files, except that owner() reports another uid for an entry
    through an injected Path.stat.
    """

    def __init__(self, root: Path):
        self.root = root
        self.root.mkdir()
        self.owners = {}

    def entry(self, pid, *, command="fixture", ppid=1, start=100, state="S", uid=None, raw=None,
              stat=True, cwd=None, root=None, descriptor=None, environ=None, cgroup=None):
        entry = self.root / str(pid)
        entry.mkdir()
        if raw is not None:
            (entry / "stat").write_text(raw)
        elif stat:
            (entry / "stat").write_text(f"{pid} ({command}) {state} {ppid} {pid} {pid} 0 -1 0 0 0 0 0 0 0 0 0 "
                                        f"20 0 1 0 {start} 0 0\n")
        if uid is not None:
            self.owners[entry] = uid
        if cwd is not None:
            (entry / "cwd").symlink_to(cwd, target_is_directory=True)
        if root is not None:
            (entry / "root").symlink_to(root, target_is_directory=True)
        if descriptor is not None:
            (entry / "fd").mkdir()
            (entry / "fd" / "3").symlink_to(descriptor)
        if environ is not None:
            (entry / "environ").write_bytes(environ)
        if cgroup is not None:
            (entry / "cgroup").write_text(cgroup)
        return entry

    def restat(self, pid, **fields):
        """Rewrite one entry's stat in place, as a later read would see it."""
        values = {"command": "fixture", "state": "S", "ppid": 1, "start": 100}
        info = resources._proc_stat(pid, self.root)
        if info is not None:
            values.update({name: info[name] for name in values})
        values.update(fields)
        (self.root / str(pid) / "stat").write_text(
            f"{pid} ({values['command']}) {values['state']} {values['ppid']} {pid} {pid} 0 -1 0 0 0 0 0 0 0 0 0 "
            f"20 0 1 0 {values['start']} 0 0\n")

    def system_manager(self, *, command="systemd", uid=0, start=1, state="S"):
        """PID 1: the local system manager, root-owned unless the case says otherwise."""
        return self.entry(1, command=command, ppid=0, start=start, uid=uid, state=state)

    def manager(self, pid=None, *, start=None, ppid=1, command="systemd", **entry):
        """The invoking user's systemd manager, uninspectable like the real non-dumpable one."""
        return self.entry(MANAGER_PID if pid is None else pid, command=command, ppid=ppid,
                          start=MANAGER_START if start is None else start, **entry)

    def helper(self, pid=None, *, start=None, ppid=MANAGER_PID, command="(sd-pam)", **entry):
        """The manager's uninspectable "(sd-pam)" PAM helper; the stat line reads "<pid> ((sd-pam)) ..."."""
        return self.entry(HELPER_PID if pid is None else pid, command=command, ppid=ppid,
                          start=HELPER_START if start is None else start, **entry)

    def ssh(self, pid=4242, parent=100, *, child_command="sshd-session", parent_command="sshd-session",
            parent_uid=0, parent_start=50, start=100, **child):
        """A same-user session process and its privilege-separation monitor."""
        self.entry(parent, command=parent_command, ppid=1, start=parent_start, uid=parent_uid)
        return self.entry(pid, command=child_command, ppid=parent, start=start, **child)

    def owned(self):
        real = Path.stat
        owners = self.owners

        def stat(path, *args, **kwargs):
            info = real(path, *args, **kwargs)
            uid = owners.get(Path(path))
            if uid is None:
                return info
            values = list(info)
            values[4] = uid  # st_uid
            return os.stat_result(values)
        return patch.object(Path, "stat", stat)


class LinuxSSHSessionTests(unittest.TestCase):
    """The Linux inspector's OpenSSH session rule over fixture /proc trees.

    The real inspector runs every case; only process-entry owners are
    injected, so no case needs root or a running SSH daemon.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        self.cache = self.base / "cache"
        self.cache.mkdir(mode=0o700)
        self.proc = FixtureProc(self.base / "proc")

    def scan(self, uid=None, **kwargs):
        found = []
        with self.proc.owned():
            users = resources.active_cache_users(self.cache, self.proc.root,
                                                 uid=os.getuid() if uid is None else uid,
                                                 disregarded=found, **kwargs)
        return users, found

    def assert_ambiguous(self, pid=4242):
        users, found = self.scan()
        self.assertIn(f"{pid}:ambiguous:cwd", users)
        self.assertFalse([record for record in found if record["pid"] == pid], found)
        return users

    def test_same_user_session_with_live_root_owned_parent_is_disregarded_and_recorded(self):
        for child_command, parent_command in (("sshd-session", "sshd-session"), ("sshd", "sshd"),
                                              ("sshd-session", "sshd")):
            with self.subTest(child=child_command, parent=parent_command):
                self.proc.ssh(child_command=child_command, parent_command=parent_command)
                users, found = self.scan()
                self.assertEqual(users, [])
                self.assertEqual(found, [{"pid": 4242, "ppid": 100, "command": child_command,
                                          "parent_command": parent_command, "start": 100,
                                          "reason": resources.SSH_DISREGARD_REASON}])
                self.assertEqual(resources.disregard_line(found[0]),
                                 f"Disregarded Linux SSH session: pid=4242 parent=100 command={child_command} "
                                 f"parent_command={parent_command} "
                                 "reason=same-user-uninspectable-with-live-root-owned-ssh-parent")
                shutil.rmtree(self.proc.root)
                self.proc = FixtureProc(self.base / "proc")

    def test_rule_applies_whatever_the_creation_tick_and_without_a_collector(self):
        self.proc.ssh(start=10**9)
        users, found = self.scan(created_after=1)
        self.assertEqual((users, [record["pid"] for record in found]), ([], [4242]))
        with self.proc.owned():
            self.assertEqual(resources.active_cache_users(self.cache, self.proc.root, uid=os.getuid()), [])
            # Without owner isolation the parent is inspected as a process of
            # its own; the session process still does not count.
            users = resources.active_cache_users(self.cache, self.proc.root)
        self.assertFalse([value for value in users if value.startswith("4242:")], users)

    def test_unverified_ancestry_stays_ambiguous(self):
        cases = {
            "user-owned parent (renamed process)": dict(parent_uid=os.getuid()),
            "root-owned parent named systemd": dict(parent_command="systemd"),
            "root-owned parent named sshd-sessionx": dict(parent_command="sshd-sessionx"),
            "differently named child": dict(child_command="bash"),
            "child named like a session listener": dict(child_command="sshd-session: kidus"),
            "zombie parent": None,
            "parent started after the child (reused PID)": dict(parent_start=101),
        }
        for label, ssh in cases.items():
            with self.subTest(label):
                if ssh is None:
                    self.proc.entry(100, command="sshd-session", uid=0, start=50, state="Z")
                    self.proc.entry(4242, command="sshd-session", ppid=100)
                else:
                    self.proc.ssh(**ssh)
                self.assert_ambiguous()
                shutil.rmtree(self.proc.root)
                self.proc = FixtureProc(self.base / "proc")

    def test_missing_unreadable_or_reparented_parent_stays_ambiguous(self):
        cases = {
            "exited parent": lambda: None,
            "parent without stat": lambda: self.proc.entry(100, uid=0, stat=False),
            "parent with malformed stat": lambda: self.proc.entry(100, uid=0, raw="100 (sshd-session S 1\n"),
        }
        for label, parent in cases.items():
            with self.subTest(label):
                parent()
                self.proc.entry(4242, command="sshd-session", ppid=100)
                self.assert_ambiguous()
                shutil.rmtree(self.proc.root)
                self.proc = FixtureProc(self.base / "proc")
        # Reparented to init (or a subreaper) after its monitor exited.
        self.proc.entry(1, command="systemd", ppid=0, start=1, uid=0)
        self.proc.entry(4242, command="sshd-session", ppid=1)
        self.assert_ambiguous()
        shutil.rmtree(self.proc.root)
        self.proc = FixtureProc(self.base / "proc")
        for ppid in (0, 4242):
            with self.subTest(ppid=ppid):
                self.proc.entry(4242, command="sshd-session", ppid=ppid)
                self.assert_ambiguous()
                shutil.rmtree(self.proc.root)
                self.proc = FixtureProc(self.base / "proc")

    def test_unreadable_or_malformed_child_stat_stays_ambiguous(self):
        raws = {
            "missing": None,
            "no closing parenthesis": "4242 (sshd-session S 100 4242 4242\n",
            "truncated": "4242 (sshd-session) S 100\n",
            "another pid": "4343 (sshd-session) S 100 4242 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0\n",
            "non-numeric parent": "4242 (sshd-session) S x 4242 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0\n",
            "not text": b"\xff\xfe",
        }
        for label, raw in raws.items():
            with self.subTest(label):
                self.proc.entry(100, command="sshd-session", uid=0, start=50)
                entry = self.proc.entry(4242, stat=False)
                if isinstance(raw, bytes):
                    (entry / "stat").write_bytes(raw)
                elif raw is not None:
                    (entry / "stat").write_text(raw)
                self.assert_ambiguous()
                shutil.rmtree(self.proc.root)
                self.proc = FixtureProc(self.base / "proc")

    def test_foreign_session_process_stays_ambiguous_without_owner_isolation(self):
        self.proc.ssh(uid=os.getuid() + 1)
        with self.proc.owned():
            found = []
            users = resources.active_cache_users(self.cache, self.proc.root, disregarded=found)
        self.assertIn("4242:ambiguous:cwd", users)
        self.assertEqual(found, [])

    def test_identity_changes_during_inspection_stay_ambiguous(self):
        """The second read of every fact must match the first: exit, reparenting, and PID reuse do not qualify."""
        real = resources._proc_stat

        def changing(pid_changed, change):
            calls = {}

            def stat(pid, proc=resources.PROC):
                info = real(pid, proc)
                calls[pid] = calls.get(pid, 0) + 1
                if pid == pid_changed and info is not None and calls[pid] > 1:
                    return change(dict(info))
                return info
            return patch.object(resources, "_proc_stat", stat)

        cases = {
            "child start changed (PID reused)": (4242, lambda info: {**info, "start": info["start"] + 1}),
            "child reparented": (4242, lambda info: {**info, "ppid": 1}),
            "child renamed": (4242, lambda info: {**info, "command": "bash"}),
            "child exited": (4242, lambda info: None),
            "child became a zombie": (4242, lambda info: {**info, "state": "Z"}),
            "parent start changed (PID reused)": (100, lambda info: {**info, "start": info["start"] + 1}),
            "parent exited": (100, lambda info: None),
            "parent became a zombie": (100, lambda info: {**info, "state": "Z"}),
            "parent renamed": (100, lambda info: {**info, "command": "systemd"}),
        }
        self.proc.ssh()
        for label, (pid, change) in cases.items():
            with self.subTest(label), changing(pid, change):
                self.assert_ambiguous()
        # An owner that changes between the two reads also disqualifies.
        real_stat = Path.stat
        parent = self.proc.root / "100"
        reads = []

        def owner(path, *args, **kwargs):
            info = real_stat(path, *args, **kwargs)
            if Path(path) != parent:
                return info
            reads.append(path)
            values = list(info)
            values[4] = 0 if len(reads) == 1 else os.getuid()
            return os.stat_result(values)
        with patch.object(Path, "stat", owner):
            found = []
            users = resources.active_cache_users(self.cache, self.proc.root, uid=os.getuid(), disregarded=found)
        self.assertIn("4242:ambiguous:cwd", users)
        self.assertEqual(found, [])

    def test_readable_references_still_block_a_session_process(self):
        (self.cache / "object").write_text("cached")
        cases = {
            "cwd": dict(cwd=self.cache),
            "root": dict(root=self.cache),
            "fd:3": dict(descriptor=self.cache / "object"),
            "env:GOCACHE": dict(cwd=self.base, environ=b"GOCACHE=" + os.fsencode(self.cache) + b"\0"),
        }
        for label, references in cases.items():
            with self.subTest(label):
                self.proc.ssh(**references)
                users, found = self.scan()
                self.assertIn("4242:" + label, users)
                self.assertEqual(found, [])
                shutil.rmtree(self.proc.root)
                self.proc = FixtureProc(self.base / "proc")
        # Mixed: a readable descriptor into the cache beside unreadable cwd,
        # root, and environ keeps both the reference and the ambiguity.
        self.proc.ssh(descriptor=self.cache / "object")
        users, found = self.scan()
        self.assertIn("4242:fd:3", users)
        self.assertIn("4242:ambiguous:cwd", users)
        self.assertIn("4242:ambiguous:environ", users)
        self.assertEqual(found, [])
        self.assertTrue((self.cache / "object").is_file())

    def test_macos_backend_never_applies_the_rule(self):
        table = {301: {"vnode_error": True, "environment": None}}
        found = []
        with patch.object(resources, "darwin", return_value=FakeDarwin(table)), \
                patch.object(resources, "_ssh_session", side_effect=AssertionError("Linux rule on macOS")):
            users = resources.active_cache_users(self.cache, backend=resources.DARWIN_BACKEND, disregarded=found)
        self.assertEqual(users, ["301:ambiguous:cwd", "301:ambiguous:environ"])
        self.assertEqual(found, [])


class SSHSessionPathRemovalTests(IsolatedState):
    """Temporary-path teardown and recovery retain the disregarded session processes."""

    def setUp(self):
        super().setUp()
        self.proc = FixtureProc(self.base / "proc")
        self.proc.ssh()
        inspector = functools.partial(resources.active_cache_users, proc=self.proc.root)
        patcher = patch.object(resources, "active_cache_users", inspector)
        patcher.start()
        self.addCleanup(patcher.stop)
        owned = self.proc.owned()
        owned.start()
        self.addCleanup(owned.stop)
        self.line = ("Disregarded Linux SSH session: pid=4242 parent=100 command=sshd-session "
                     "parent_command=sshd-session reason=" + resources.SSH_DISREGARD_REASON)

    def path_identity(self, name="disposable"):
        path = self.base / name
        path.mkdir(mode=0o700)
        info = path.lstat()
        return {"path": str(path), "device": info.st_dev, "inode": info.st_ino}

    def test_removal_detail_names_the_disregarded_process(self):
        identity = self.path_identity()
        state, detail = resources.remove_path(identity)
        self.assertEqual(state, "removed")
        self.assertEqual(detail, "removed disposable path " + identity["path"] + "; " + self.line)
        self.assertFalse(Path(identity["path"]).exists())

    def test_refusal_keeps_the_disregarded_process_once_across_rescans(self):
        identity = self.path_identity()
        # An unrelated uninspectable process still blocks, through every rescan.
        self.proc.entry(4343, command="bash", ppid=100)
        with patch.object(resources.time, "sleep"), \
                self.assertRaises(resources.Refusal) as refused:
            resources.remove_path(identity)
        message = str(refused.exception)
        self.assertIn("is in use: 4343:ambiguous:cwd", message)
        self.assertEqual(message.count(self.line), 1)
        self.assertTrue(Path(identity["path"]).is_dir())

    def test_owner_cleanup_and_recovery_retain_the_detail(self):
        owner = resources.Invocation.create("ssh-owner", self.checkout, ["true"])
        self.assertEqual(owner.finish(outcome="success"), [])
        [entry] = [value for value in resources.load_inventory(owner.path)["resources"] if value["kind"] == "path"]
        self.assertEqual(entry["state"], "removed")
        self.assertTrue(entry["detail"].endswith("; " + self.line), entry["detail"])

        orphan = resources.Invocation.create("ssh-orphan", self.checkout, ["true"])
        os.close(orphan._owner_lock)
        orphan._owner_lock = None

        def foreign_owner(value):
            value["owner"]["birth"] = {"start_ticks": -1, "boot_id": "another-boot"}
        value = json.loads((orphan.path / "inventory.json").read_text())
        foreign_owner(value)
        resources.write_json(orphan.path / "inventory.json", value)
        complete, actions = resources.recover(orphan.path)
        self.assertTrue(complete)
        [action] = [value for value in actions if value["kind"] == "path"]
        self.assertTrue(action["detail"].endswith("; " + self.line), action["detail"])
        logged = [json.loads(line) for line in (orphan.path / "recovery.jsonl").read_text().splitlines()]
        self.assertIn(self.line, [value for value in logged if value["kind"] == "path"][0]["detail"])


class LinuxUserManagerTests(unittest.TestCase):
    """The Linux inspector's user-manager rule over fixture /proc trees.

    The real inspector runs every case with an injected system-manager query
    and CLK_TCK; only process-entry owners are injected besides, so no case
    needs root, a running systemd, or the host's manager.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        self.cache = self.base / "cache"
        self.cache.mkdir(mode=0o700)
        self.proc = FixtureProc(self.base / "proc")

    def reset(self):
        shutil.rmtree(self.proc.root)
        self.proc = FixtureProc(self.base / "proc")

    def scan(self, query, hz=MANAGER_HZ, isolated=True, **kwargs):
        found = []
        with self.proc.owned():
            users = resources.active_cache_users(self.cache, self.proc.root,
                                                 uid=os.getuid() if isolated else None, disregarded=found,
                                                 manager_query=query, clock_ticks=lambda: hz, **kwargs)
        return users, found

    def record(self, pid=MANAGER_PID, start=MANAGER_START, usec=MANAGER_USEC):
        return {"kind": resources.USER_MANAGER_KIND, "pid": pid, "start": start, "uid": os.getuid(),
                "service": f"user@{os.getuid()}.service", "main_pid": pid, "monotonic_usec": usec,
                "reason": resources.USER_MANAGER_REASON}

    def assert_ambiguous(self, query, pid=MANAGER_PID, **kwargs):
        users, found = self.scan(query, **kwargs)
        self.assertIn(f"{pid}:ambiguous:cwd", users)
        self.assertEqual([record for record in found if record["pid"] == pid], [])
        return users

    def test_matching_mainpid_and_start_time_disregarded(self):
        self.proc.system_manager()
        self.proc.manager()
        query = FixtureManagerQuery(manager_service())
        users, found = self.scan(query)
        self.assertEqual(users, [])
        self.assertEqual(found, [self.record()])
        self.assertEqual(resources.disregard_line(found[0]), manager_line())
        # Two independent queries for the invoking uid, one before and one after inspection.
        self.assertEqual(query.calls, [os.getuid(), os.getuid()])
        # Newer than the cache: the rule does not depend on a creation tick.
        users, found = self.scan(FixtureManagerQuery(manager_service()), created_after=1)
        self.assertEqual((users, found), ([], [self.record()]))
        # Unbounded inspection (no owner isolation): PID 1 is inspected as a
        # process of its own, and the manager still does not count.
        users, found = self.scan(FixtureManagerQuery(manager_service()), isolated=False)
        self.assertFalse([value for value in users if value.startswith(f"{MANAGER_PID}:")], users)
        self.assertEqual(found, [self.record()])

    def test_timestamp_one_tick_boundary(self):
        self.assertTrue(resources.start_matches(MANAGER_START, MANAGER_USEC, MANAGER_HZ))
        self.proc.system_manager()
        self.proc.manager()
        for hz in (100, 250, 1000):
            exact = MANAGER_START * 1_000_000 // hz
            tick = 1_000_000 // hz
            cases = {"exact": (exact, True), "one tick early": (exact - tick, True),
                     "one tick late": (exact + tick, True), "just before": (exact - tick - 1, False),
                     "just after": (exact + tick + 1, False)}
            for label, (usec, qualifies) in cases.items():
                with self.subTest(hz=hz, case=label):
                    self.assertEqual(resources.start_matches(MANAGER_START, usec, hz), qualifies)
                    query = FixtureManagerQuery(manager_service(monotonic_usec=usec))
                    if qualifies:
                        users, found = self.scan(query, hz=hz)
                        self.assertEqual((users, found), ([], [self.record(usec=usec)]))
                    else:
                        self.assert_ambiguous(query, hz=hz)

    def test_renamed_orphan_with_pid1_and_init_scope_refuses(self):
        """PID-1 ancestry and the manager's init.scope cgroup are forgeable; only the query authenticates."""
        cgroup = USER_SERVICE_CGROUP.format(uid=os.getuid())
        impostor = 4242
        variants = {
            "orphan of live root PID 1 in init.scope": dict(ppid=1, cgroup=cgroup),
            "non-PID-1 parent": dict(ppid=4000, cgroup=cgroup),
            "subreaper parent": dict(ppid=4000, cgroup=cgroup, subreaper=True),
            "alternate session cgroup": dict(
                ppid=1, cgroup=f"0::/user.slice/user-{os.getuid()}.slice/session-3.scope\n"),
            "foreign user's manager cgroup": dict(ppid=1, cgroup=USER_SERVICE_CGROUP.format(uid=os.getuid() + 1)),
        }
        answers = {"queried MainPID differs": manager_service(),
                   "service unavailable": None}
        for label, variant in variants.items():
            for answer_label, answer in answers.items():
                with self.subTest(label, answer=answer_label):
                    self.reset()
                    options = dict(variant)
                    self.proc.system_manager()
                    if options.pop("subreaper", False):
                        # A readable same-user subreaper (systemd --user itself is one) adopts the orphan.
                        self.proc.entry(4000, command="systemd", ppid=1, start=50, environ=b"", cwd=self.base,
                                        root=Path("/"))
                    elif options["ppid"] == 4000:
                        self.proc.entry(4000, command="bash", ppid=1, start=50, environ=b"", cwd=self.base,
                                        root=Path("/"))
                    # Same start as the real manager, so only the MainPID binding can tell them apart.
                    self.proc.manager(impostor, **options)
                    query = FixtureManagerQuery(answer)
                    self.assert_ambiguous(query, pid=impostor)

    def test_pid_reuse_start_mismatch_refuses(self):
        self.proc.system_manager()
        for start in (MANAGER_START + 2, MANAGER_START - 1, 10**9):
            with self.subTest(start=start):
                shutil.rmtree(self.proc.root / str(MANAGER_PID), ignore_errors=True)
                self.proc.manager(start=start)
                self.assert_ambiguous(FixtureManagerQuery(manager_service()))

    def test_foreign_uid_never_disregarded(self):
        self.proc.system_manager()
        self.proc.manager(uid=os.getuid() + 1)
        query = FixtureManagerQuery(manager_service())
        # Without owner isolation the foreign process is inspected and stays ambiguous.
        self.assert_ambiguous(query, isolated=False)
        # With owner isolation it is filtered out before any rule, as before.
        users, found = self.scan(query)
        self.assertEqual((users, found), ([], []))
        self.assertEqual(query.calls, [])

    def test_second_read_changes_refuse(self):
        """A fact that changes between the reads before and after inspection disqualifies the process."""
        other = os.getuid() + 1

        def candidate(**fields):
            return lambda: self.proc.restat(MANAGER_PID, **fields)

        def pid1(**fields):
            return lambda: self.proc.restat(1, **fields)

        def owner(pid, uid):
            return lambda: self.proc.owners.__setitem__(self.proc.root / str(pid), uid)

        def remove(pid, name="stat"):
            return lambda: (self.proc.root / str(pid) / name).unlink()

        def second(**fields):
            return dict(answers=(manager_service(), manager_service(**fields)))

        changes = {
            "second query MainPID changed": (second(main_pid=MANAGER_PID + 1), None),
            "second query timestamp changed": (second(monotonic_usec=MANAGER_USEC + 1), None),
            "second query not active": (second(active_state="deactivating"), None),
            "second query unavailable": (dict(answers=(manager_service(), None)), None),
            "candidate start changed (PID reused)": ({}, candidate(start=MANAGER_START + 1)),
            "candidate renamed": ({}, candidate(command="bash")),
            "candidate became a zombie": ({}, candidate(state="Z")),
            "candidate stat unreadable": ({}, remove(MANAGER_PID)),
            "candidate owner changed": ({}, owner(MANAGER_PID, other)),
            "PID 1 restarted": ({}, pid1(start=2)),
            "PID 1 renamed": ({}, pid1(command="init")),
            "PID 1 became a zombie": ({}, pid1(state="Z")),
            "PID 1 stat unreadable": ({}, remove(1)),
            "PID 1 owner changed": ({}, owner(1, os.getuid())),
        }
        for label, (options, between) in changes.items():
            with self.subTest(label):
                self.reset()
                self.proc.system_manager()
                self.proc.manager()
                answers = options.get("answers", (manager_service(),))
                query = FixtureManagerQuery(*answers, between=between)
                users = self.assert_ambiguous(query)
                self.assertEqual(len(query.calls), 2)
                # The still-live candidate stays ambiguous; no exit is manufactured.
                self.assertIn(f"{MANAGER_PID}:ambiguous:environ", users)
        # A candidate that exits during inspection leaves no manager record.
        self.reset()
        self.proc.system_manager()
        self.proc.manager()
        query = FixtureManagerQuery(manager_service(),
                                    between=lambda: shutil.rmtree(self.proc.root / str(MANAGER_PID)))
        _, found = self.scan(query)
        self.assertEqual(found, [])

    def test_readable_cache_references_always_block(self):
        (self.cache / "object").write_text("cached")
        cases = {
            "cwd": dict(cwd=self.cache),
            "root": dict(root=self.cache),
            "fd:3": dict(descriptor=self.cache / "object"),
            "env:GOCACHE": dict(cwd=self.base, environ=b"GOCACHE=" + os.fsencode(self.cache) + b"\0"),
        }
        for label, references in cases.items():
            with self.subTest(label):
                self.reset()
                self.proc.system_manager()
                self.proc.manager(**references)
                users, found = self.scan(FixtureManagerQuery(manager_service()))
                self.assertIn(f"{MANAGER_PID}:" + label, users)
                self.assertEqual(found, [])
        # Mixed: a readable descriptor beside unreadable cwd, root, and environ.
        self.reset()
        self.proc.system_manager()
        self.proc.manager(descriptor=self.cache / "object")
        users, found = self.scan(FixtureManagerQuery(manager_service()))
        self.assertIn(f"{MANAGER_PID}:fd:3", users)
        self.assertIn(f"{MANAGER_PID}:ambiguous:cwd", users)
        self.assertIn(f"{MANAGER_PID}:ambiguous:environ", users)
        self.assertEqual(found, [])
        self.assertTrue((self.cache / "object").is_file())

    def test_namespace_manager_mismatch_refuses(self):
        """PID 1 must be the live root-owned systemd of the same /proc; nothing else is queried."""
        cases = {
            "no PID 1 (namespace without the host's init)": None,
            "PID 1 is a sandbox init": dict(command="codex"),
            "PID 1 named init": dict(command="init"),
            "PID 1 owned by the user": dict(uid=os.getuid()),
            "PID 1 a zombie": dict(state="Z"),
            "PID 1 stat unreadable": "unreadable",
        }
        for label, pid1 in cases.items():
            with self.subTest(label):
                self.reset()
                if pid1 == "unreadable":
                    self.proc.entry(1, uid=0, stat=False)
                elif pid1 is not None:
                    self.proc.system_manager(**pid1)
                self.proc.manager()
                query = FixtureManagerQuery(manager_service())
                self.assert_ambiguous(query)
                self.assertEqual(query.calls, [])

    def test_clock_tick_failures_refuse(self):
        self.proc.system_manager()
        self.proc.manager()
        for hz in (None, 0, -100, 100.0, "100"):
            with self.subTest(hz=hz):
                self.assert_ambiguous(FixtureManagerQuery(manager_service()), hz=hz)

    def test_macos_never_queries_system_manager(self):
        table = {301: {"vnode_error": True, "environment": None}}
        found = []
        refuse = FixtureManagerQuery(manager_service())
        with patch.object(resources, "darwin", return_value=FakeDarwin(table)), \
                patch.object(resources, "_user_manager_first", side_effect=AssertionError("Linux rule on macOS")), \
                patch.object(resources, "query_user_manager", side_effect=AssertionError("query on macOS")):
            users = resources.active_cache_users(self.cache, backend=resources.DARWIN_BACKEND, disregarded=found,
                                                 manager_query=refuse)
        self.assertEqual(users, ["301:ambiguous:cwd", "301:ambiguous:environ"])
        self.assertEqual((found, refuse.calls), ([], []))


class FakeQueryChild:
    """A stand-in for the query's child process whose output comes from a file or pipe."""

    def __init__(self, stdout, status=0, hang=False):
        self.stdout = stdout
        self.status = status
        self.hang = hang
        self.returncode = None
        self.killed = False

    def poll(self):
        return self.returncode

    def kill(self):
        self.killed = True
        self.returncode = -signal.SIGKILL

    def wait(self, timeout=None):
        if self.returncode is None:
            if self.hang and timeout is not None:
                raise subprocess.TimeoutExpired("systemctl", timeout)
            self.returncode = self.status
        return self.returncode


class UserManagerQueryTests(unittest.TestCase):
    """The bounded, trusted system-manager query and its parser, through injected process seams only."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        self.valid = (f"ActiveState=active\nMainPID={MANAGER_PID}\n"
                      f"ExecMainStartTimestampMonotonic={MANAGER_USEC}\n").encode()

    def output_child(self, data, **options):
        path = self.base / f"output-{secrets.token_hex(3)}"
        path.write_bytes(data)
        return FakeQueryChild(path.open("rb"), **options)

    def test_absolute_system_query_and_clean_environment(self):
        hostile = {"PATH": str(self.base), "DBUS_SYSTEM_BUS_ADDRESS": "unix:path=" + str(self.base / "bus"),
                   "LD_PRELOAD": str(self.base / "evil.so"), "LD_LIBRARY_PATH": str(self.base),
                   "SYSTEMD_PAGER": "evil", "SYSTEMCTL_FORCE_BUS": "1", "LC_ALL": "xx_XX"}
        calls, trusted = [], []

        def run(argv, env, timeout, limit):
            calls.append((argv, env, timeout, limit))
            return 0, self.valid
        with patch.dict(os.environ, hostile):
            result = resources.query_user_manager(1000, run=run, trusted=lambda path: trusted.append(path) or True)
        self.assertEqual(result, manager_service())
        self.assertEqual(trusted, ["/usr/bin/systemctl"])
        [(argv, env, timeout, limit)] = calls
        self.assertEqual(argv, ["/usr/bin/systemctl", "--system", "--no-pager", "--no-ask-password", "show",
                                "user@1000.service", "-p", "MainPID", "-p", "ExecMainStartTimestampMonotonic",
                                "-p", "ActiveState"])
        for option in ("--user", "-H", "--host", "-M", "--machine"):
            self.assertNotIn(option, argv)
        self.assertEqual(env, {"LC_ALL": "C"})
        self.assertEqual((timeout, limit), (2.0, 16 * 1024))

        # The production runner executes the argv list directly with exactly
        # that environment: no shell, no PATH lookup, nothing inherited. The
        # recorded child is redirected to an interpreter that prints the
        # environment it received, so the host's manager is never queried.
        recorded = []

        def popen(argv, **kwargs):
            recorded.append((argv, kwargs))
            program = "import os, sys; sys.stdout.write(' '.join(sorted(os.environ)))"
            return subprocess.Popen([sys.executable, "-c", program], **kwargs)
        with patch.dict(os.environ, hostile):
            status, output = resources.bounded_output(resources.user_manager_argv(1000),
                                                      dict(resources.USER_MANAGER_ENVIRONMENT), 2.0, 16 * 1024,
                                                      popen=popen)
        [(argv, kwargs)] = recorded
        self.assertIsInstance(argv, list)
        self.assertEqual(argv[0], "/usr/bin/systemctl")
        self.assertFalse(kwargs.get("shell", False))
        self.assertNotIn("executable", kwargs)
        self.assertEqual(kwargs["env"], {"LC_ALL": "C"})
        self.assertEqual((kwargs["stdin"], kwargs["stderr"]), (subprocess.DEVNULL, subprocess.STDOUT))
        self.assertEqual(status, 0)
        received = output.decode().split()
        self.assertIn("LC_ALL", received)
        for name in hostile:
            if name != "LC_ALL":
                self.assertNotIn(name, received)

    def test_untrusted_executable_refuses(self):
        directory = os.stat_result((stat.S_IFDIR | 0o755, 0, 0, 1, 0, 0, 0, 0, 0, 0))
        executable = os.stat_result((stat.S_IFREG | 0o755, 0, 0, 1, 0, 0, 0, 0, 0, 0))

        def lstat(overrides):
            def read(path):
                value = overrides.get(str(path))
                if isinstance(value, BaseException):
                    raise value
                if value is not None:
                    return value
                return executable if str(path) == "/usr/bin/systemctl" else directory
            return read

        def mode(kind, bits, uid=0):
            return os.stat_result((kind | bits, 0, 0, 1, uid, 0, 0, 0, 0, 0))
        self.assertTrue(resources.root_controlled_executable("/usr/bin/systemctl", lstat({})))
        cases = {
            "executable is a symlink": {"/usr/bin/systemctl": mode(stat.S_IFLNK, 0o777)},
            "ancestor is a symlink": {"/usr/bin": mode(stat.S_IFLNK, 0o777)},
            "executable owned by the user": {"/usr/bin/systemctl": mode(stat.S_IFREG, 0o755, uid=1000)},
            "ancestor owned by the user": {"/usr": mode(stat.S_IFDIR, 0o755, uid=1000)},
            "executable group-writable": {"/usr/bin/systemctl": mode(stat.S_IFREG, 0o775)},
            "ancestor world-writable": {"/usr/bin": mode(stat.S_IFDIR, 0o777)},
            "root world-writable": {"/": mode(stat.S_IFDIR, 0o1777)},
            "executable not a regular file": {"/usr/bin/systemctl": mode(stat.S_IFDIR, 0o755)},
            "executable not executable": {"/usr/bin/systemctl": mode(stat.S_IFREG, 0o644)},
            "missing executable": {"/usr/bin/systemctl": FileNotFoundError()},
            "unreadable ancestor": {"/usr": PermissionError()},
        }
        for label, overrides in cases.items():
            with self.subTest(label):
                self.assertFalse(resources.root_controlled_executable("/usr/bin/systemctl", lstat(overrides)))
        for path in ("usr/bin/systemctl", "/usr/bin/../bin/systemctl", "systemctl"):
            with self.subTest(path=path):
                self.assertFalse(resources.root_controlled_executable(path, lstat({})))
        # An untrusted executable is never run.
        run = unittest.mock.Mock(side_effect=AssertionError("untrusted executable ran"))
        self.assertIsNone(resources.query_user_manager(1000, run=run, trusted=lambda path: False))
        run.assert_not_called()

    def test_unavailable_error_timeout_and_output_limit_refuse(self):
        argv, env = resources.user_manager_argv(1000), dict(resources.USER_MANAGER_ENVIRONMENT)
        limit = resources.USER_MANAGER_OUTPUT_LIMIT
        # Missing executable or a start failure.
        for error in (FileNotFoundError(), PermissionError(), OSError(errno.ENOEXEC, "exec format")):
            with self.subTest(start_failure=type(error).__name__):
                popen = unittest.mock.Mock(side_effect=error)
                self.assertIsNone(resources.bounded_output(argv, env, 2.0, limit, popen=popen))
        # Bus or permission failure, and any nonzero exit, refuse even with well-formed output.
        for result in (None, (1, b"Failed to connect to bus: Permission denied\n"), (1, self.valid), (-9, self.valid)):
            with self.subTest(result=result):
                self.assertIsNone(resources.query_user_manager(1000, run=lambda *args: result, trusted=lambda p: True))
        # The two-second deadline: a child that never finishes writing is
        # killed once the injected clock passes the deadline.
        read, write = os.pipe()
        self.addCleanup(os.close, write)
        child = FakeQueryChild(os.fdopen(read, "rb"))
        clock = iter((100.0, 102.0))
        self.assertIsNone(resources.bounded_output(argv, env, 2.0, limit, popen=lambda *a, **k: child,
                                                   clock=lambda: next(clock)))
        self.assertTrue(child.killed)
        self.assertTrue(child.stdout.closed)
        # Output arrives in time but the deadline passes before the next read.
        child = self.output_child(b"ActiveState=active\n")
        clock = iter((100.0, 100.5, 102.5))
        self.assertIsNone(resources.bounded_output(argv, env, 2.0, limit, popen=lambda *a, **k: child,
                                                   clock=lambda: next(clock)))
        # Output complete but the child does not exit within the remaining deadline.
        child = self.output_child(self.valid, hang=True)
        self.assertIsNone(resources.bounded_output(argv, env, 2.0, limit, popen=lambda *a, **k: child,
                                                   clock=lambda: 100.0))
        self.assertTrue(child.killed)
        # 16 KiB of combined output is accepted; one byte more stops the query.
        child = self.output_child(b"x" * limit)
        self.assertEqual(resources.bounded_output(argv, env, 2.0, limit, popen=lambda *a, **k: child,
                                                  clock=lambda: 100.0), (0, b"x" * limit))
        self.assertFalse(child.killed)
        child = self.output_child(b"x" * (limit + 1))
        self.assertIsNone(resources.bounded_output(argv, env, 2.0, limit, popen=lambda *a, **k: child,
                                                   clock=lambda: 100.0))
        self.assertTrue(child.killed)
        self.assertTrue(child.stdout.closed)

    def test_malformed_duplicate_missing_properties_refuse(self):
        valid = self.valid.decode()
        cases = {
            "empty": b"",
            "undecodable": b"\xff\xfe",
            "truncated": valid.encode()[:-1],
            "missing MainPID": b"ActiveState=active\nExecMainStartTimestampMonotonic=1\n",
            "missing timestamp": f"ActiveState=active\nMainPID={MANAGER_PID}\n".encode(),
            "missing state": f"MainPID={MANAGER_PID}\nExecMainStartTimestampMonotonic=1\n".encode(),
            "duplicate MainPID": (valid + f"MainPID={MANAGER_PID}\n").encode(),
            "duplicate state": (valid + "ActiveState=active\n").encode(),
            "unexpected property": (valid + "SubState=running\n").encode(),
            "line without separator": (valid + "MainPID\n").encode(),
            "blank line": (valid + "\n").encode(),
            "carriage returns": valid.replace("\n", "\r\n").encode(),
        }
        for value in ("0", "-5", "+5", "05", "1.5", "abc", " 5", "", "٥"):
            cases[f"MainPID={value!r}"] = valid.replace(f"MainPID={MANAGER_PID}", "MainPID=" + value).encode()
            cases[f"timestamp={value!r}"] = valid.replace(f"={MANAGER_USEC}", "=" + value).encode()
        self.assertEqual(resources.parse_user_manager(self.valid), manager_service())
        for label, raw in cases.items():
            with self.subTest(label):
                self.assertIsNone(resources.parse_user_manager(raw))
                self.assertIsNone(resources.query_user_manager(1000, run=lambda *args: (0, raw), trusted=lambda p: True))
        # An invalid tick frequency is no frequency.
        def failing(error):
            def sysconf(name):
                raise error
            return sysconf
        self.assertEqual(resources.system_clock_ticks(lambda name: 100), 100)
        for sysconf in (failing(ValueError()), failing(OSError()), lambda name: 0, lambda name: -100,
                        lambda name: 100.0, lambda name: None, lambda name: True):
            with self.subTest(sysconf=sysconf):
                self.assertIsNone(resources.system_clock_ticks(sysconf))

    def test_non_active_service_refuses(self):
        for state in ("inactive", "activating", "deactivating", "failed", "reloading", "maintenance", "",
                      "Active", "active "):
            with self.subTest(state=state):
                raw = self.valid.replace(b"ActiveState=active\n", f"ActiveState={state}\n".encode())
                self.assertIsNone(resources.query_user_manager(1000, run=lambda *args: (0, raw), trusted=lambda p: True))
        self.assertEqual(resources.query_user_manager(1000, run=lambda *args: (0, self.valid), trusted=lambda p: True),
                         manager_service())


class UserManagerPathRemovalTests(IsolatedState):
    """Temporary-path teardown and recovery retain the disregarded user manager.

    The fixture manager starts far later than any real temporary path, so no
    creation bound can disregard it in place of the rule.
    """

    START = 10**10
    USEC = START * (1_000_000 // MANAGER_HZ)

    def setUp(self):
        super().setUp()
        self.proc = FixtureProc(self.base / "proc")
        self.proc.system_manager()
        self.proc.manager(start=self.START)
        self.line = manager_line(start=self.START, usec=self.USEC)
        inspector = functools.partial(resources.active_cache_users, proc=self.proc.root)
        for name, value in (("active_cache_users", inspector), ("system_clock_ticks", lambda: MANAGER_HZ)):
            patcher = patch.object(resources, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.query = self.answer(manager_service(monotonic_usec=self.USEC))
        owned = self.proc.owned()
        owned.start()
        self.addCleanup(owned.stop)
        self.ssh_line = ("Disregarded Linux SSH session: pid=4242 parent=100 command=sshd-session "
                         "parent_command=sshd-session reason=" + resources.SSH_DISREGARD_REASON)

    def answer(self, *answers):
        query = FixtureManagerQuery(*answers)
        patcher = patch.object(resources, "query_user_manager", query)
        patcher.start()
        self.addCleanup(patcher.stop)
        return query

    def path_identity(self, name="disposable"):
        path = self.base / name
        path.mkdir(mode=0o700)
        info = path.lstat()
        return {"path": str(path), "device": info.st_dev, "inode": info.st_ino}

    def orphan(self, task, cross_boot=False):
        """An invocation whose owner is gone. cross_boot records its path on another boot, so
        neither its creation tick nor its sessions narrow live-use inspection."""
        orphan = resources.Invocation.create(task, self.checkout, ["true"])
        os.close(orphan._owner_lock)
        orphan._owner_lock = None
        value = json.loads((orphan.path / "inventory.json").read_text())
        value["owner"]["birth"] = {"start_ticks": -1, "boot_id": "another-boot"}
        if cross_boot:
            for entry in value["resources"]:
                if entry["kind"] == "path":
                    entry["identity"]["boot_id"] = "another-boot"
        resources.write_json(orphan.path / "inventory.json", value)
        return orphan

    def test_verified_manager_path_removal_retains_detail(self):
        identity = self.path_identity()
        state, detail = resources.remove_path(identity)
        self.assertEqual(state, "removed")
        self.assertEqual(detail, "removed disposable path " + identity["path"] + "; " + self.line)
        self.assertFalse(Path(identity["path"]).exists())
        self.assertEqual(len(self.query.calls), 2)

    def test_later_refusal_retains_manager_and_ssh_lines_once(self):
        identity = self.path_identity()
        self.proc.ssh()
        # An unrelated uninspectable process still blocks, through every rescan.
        self.proc.entry(4343, command="bash", ppid=100)
        with patch.object(resources.time, "sleep"), self.assertRaises(resources.Refusal) as refused:
            resources.remove_path(identity)
        message = str(refused.exception)
        self.assertIn("is in use: 4343:ambiguous:cwd", message)
        self.assertEqual(message.count(self.line), 1)
        self.assertEqual(message.count(self.ssh_line), 1)
        self.assertLess(message.index(self.ssh_line), message.index(self.line))
        self.assertTrue(Path(identity["path"]).is_dir())
        # Every rescan queried the system manager afresh: two queries per scan.
        self.assertEqual(len(self.query.calls), 8)

    def test_recovery_retains_manager_report(self):
        owner = resources.Invocation.create("manager-owner", self.checkout, ["true"])
        self.assertEqual(owner.finish(outcome="success"), [])
        [entry] = [value for value in resources.load_inventory(owner.path)["resources"] if value["kind"] == "path"]
        self.assertEqual(entry["state"], "removed")
        self.assertTrue(entry["detail"].endswith("; " + self.line), entry["detail"])

        orphan = self.orphan("manager-orphan")
        complete, actions = resources.recover(orphan.path)
        self.assertTrue(complete)
        [action] = [value for value in actions if value["kind"] == "path"]
        self.assertTrue(action["detail"].endswith("; " + self.line), action["detail"])
        logged = [json.loads(line) for line in (orphan.path / "recovery.jsonl").read_text().splitlines()]
        self.assertIn(self.line, [value for value in logged if value["kind"] == "path"][0]["detail"])

    def test_unverified_manager_keeps_path_and_recovery_refusal(self):
        self.answer(manager_service(main_pid=MANAGER_PID + 1, monotonic_usec=self.USEC))
        identity = self.path_identity()
        with patch.object(resources.time, "sleep"), self.assertRaises(resources.Refusal) as refused:
            resources.remove_path(identity)
        message = str(refused.exception)
        self.assertIn(f"{MANAGER_PID}:ambiguous:cwd", message)
        self.assertNotIn("Disregarded Linux user manager", message)
        self.assertTrue(Path(identity["path"]).is_dir())

        orphan = self.orphan("manager-unverified", cross_boot=True)
        [path_entry] = [value for value in resources.load_inventory(orphan.path)["resources"]
                        if value["kind"] == "path"]
        with patch.object(resources.time, "sleep"):
            complete, actions = resources.recover(orphan.path)
        self.assertFalse(complete)
        [action] = [value for value in actions if value["kind"] == "path"]
        self.assertEqual(action["action"], "refused")
        self.assertIn(f"{MANAGER_PID}:ambiguous:cwd", action["detail"])
        self.assertNotIn("Disregarded Linux user manager", action["detail"])
        self.assertTrue(Path(path_entry["identity"]["path"]).is_dir())
        logged = [json.loads(line) for line in (orphan.path / "recovery.jsonl").read_text().splitlines()]
        refused_paths = [value for value in logged if value["kind"] == "path"]
        self.assertEqual(refused_paths[0]["action"], "refused")
        self.assertIn(f"{MANAGER_PID}:ambiguous:cwd", refused_paths[0]["detail"])


class LinuxUserManagerHelperTests(unittest.TestCase):
    """The Linux inspector's rule for the authenticated user manager's "(sd-pam)" helper.

    The real inspector runs every case over fixture /proc trees with an
    injected system-manager query and CLK_TCK; only process-entry owners are
    injected besides, so no case needs root, a running systemd, a user
    service, or the host's manager. Changes between reads are made through
    deterministic hooks, never by waiting.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        self.cache = self.base / "cache"
        self.cache.mkdir(mode=0o700)
        self.proc = FixtureProc(self.base / "proc")

    def reset(self):
        shutil.rmtree(self.proc.root)
        self.proc = FixtureProc(self.base / "proc")

    def host(self, manager_start=MANAGER_START, **helper):
        """PID 1, the user manager, and its helper, as on the observed host."""
        self.reset()
        self.proc.system_manager()
        self.proc.manager(start=manager_start)
        return self.proc.helper(**helper)

    def scan(self, query=None, hz=MANAGER_HZ, isolated=True, **kwargs):
        query = FixtureManagerQuery(manager_service()) if query is None else query
        found = []
        with self.proc.owned():
            users = resources.active_cache_users(self.cache, self.proc.root,
                                                 uid=os.getuid() if isolated else None, disregarded=found,
                                                 manager_query=query, clock_ticks=lambda: hz, **kwargs)
        return users, found

    def helper_record(self, pid=HELPER_PID, start=HELPER_START, parent_start=MANAGER_START, hz=MANAGER_HZ):
        return {"kind": resources.USER_MANAGER_HELPER_KIND, "pid": pid, "ppid": MANAGER_PID, "uid": os.getuid(),
                "command": "(sd-pam)", "start": start, "parent_start": parent_start, "clk_tck": hz,
                "reason": resources.USER_MANAGER_HELPER_REASON}

    def kinds(self, found):
        """Each record's kind and PID, independent of the order /proc listed the processes in."""
        return sorted((record.get("kind"), record["pid"]) for record in found)

    def assert_helper_ambiguous(self, query=None, pid=HELPER_PID, **kwargs):
        users, found = self.scan(query, **kwargs)
        self.assertIn(f"{pid}:ambiguous:cwd", users)
        self.assertEqual([record for record in found if record["pid"] == pid], [])
        return users, found

    def test_matching_helper_and_inclusive_window(self):
        self.host()
        query = FixtureManagerQuery(manager_service())
        users, found = self.scan(query)
        self.assertEqual(users, [])
        self.assertEqual(found[1], self.helper_record())
        self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID),
                                             (resources.USER_MANAGER_HELPER_KIND, HELPER_PID)])
        self.assertEqual(resources.disregard_line(found[1]), helper_line())
        # The stat line carries the parenthesized command inside the kernel's own parentheses.
        self.assertTrue((self.proc.root / str(HELPER_PID) / "stat").read_text()
                        .startswith(f"{HELPER_PID} ((sd-pam)) S {MANAGER_PID} "))
        # The helper adds no query: only the manager's two.
        self.assertEqual(query.calls, [os.getuid(), os.getuid()])
        for start, qualifies in ((MANAGER_START, True), (MANAGER_START + MANAGER_HZ, True),
                                 (MANAGER_START - 1, False), (MANAGER_START + MANAGER_HZ + 1, False)):
            with self.subTest(start=start - MANAGER_START):
                self.host(start=start)
                if qualifies:
                    users, found = self.scan()
                    self.assertEqual((users, found[1:]), ([], [self.helper_record(start=start)]))
                else:
                    users, found = self.assert_helper_ambiguous()
                    # The manager itself is still disregarded; only the helper refuses.
                    self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID)])

    def test_integer_window_and_invalid_clock_ticks(self):
        large = 10**15 + 7
        for hz in (100, 250, 1000):
            usec = large * (1_000_000 // hz)
            for offset, qualifies in ((0, True), (hz, True), (hz + 1, False), (-1, False)):
                with self.subTest(hz=hz, offset=offset):
                    self.assertEqual(resources.helper_start_in_window(large + offset, large, hz), qualifies)
                    self.host(manager_start=large, start=large + offset)
                    query = FixtureManagerQuery(manager_service(monotonic_usec=usec))
                    if qualifies:
                        users, found = self.scan(query, hz=hz)
                        self.assertEqual((users, found[1:]),
                                         ([], [self.helper_record(start=large + offset, parent_start=large, hz=hz)]))
                    else:
                        self.assert_helper_ambiguous(query, hz=hz)
        for hz in (None, 0, -100, 100.0, "100", True):
            with self.subTest(clock_ticks=hz):
                self.assertFalse(resources.helper_start_in_window(HELPER_START, MANAGER_START, hz))
                self.host()
                users, found = self.assert_helper_ambiguous(hz=hz)
                self.assertEqual(found, [])
        for start in (float(HELPER_START), str(HELPER_START), None):
            with self.subTest(helper_start=start):
                self.assertFalse(resources.helper_start_in_window(start, MANAGER_START, MANAGER_HZ))

    def test_later_named_user_service_refused(self):
        """A user service the manager started later and renamed "(sd-pam)" falls outside the window."""
        self.host()
        self.proc.helper(4242, start=LATER_SERVICE_START)
        users, found = self.scan()
        self.assertIn("4242:ambiguous:cwd", users)
        self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID),
                                             (resources.USER_MANAGER_HELPER_KIND, HELPER_PID)])

    def test_early_service_impostor_documents_accepted_risk(self):
        """The accepted residual risk: a renamed non-dumpable service started within one second qualifies.

        Its /proc facts are indistinguishable from the real helper's, so the
        rule disregards it when it exposes no readable reference. The
        operator accepted this on task 261009-227582; a readable reference
        still blocks it.
        """
        self.host()
        self.proc.helper(4242, start=MANAGER_START + MANAGER_HZ)
        users, found = self.scan()
        self.assertEqual(users, [])
        self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID),
                                             (resources.USER_MANAGER_HELPER_KIND, 4242),
                                             (resources.USER_MANAGER_HELPER_KIND, HELPER_PID)])
        (self.cache / "object").write_text("cached")
        self.host()
        self.proc.helper(4242, start=MANAGER_START + MANAGER_HZ, descriptor=self.cache / "object")
        users, found = self.scan()
        self.assertIn("4242:fd:3", users)
        self.assertNotIn(4242, [record["pid"] for record in found])

    def test_unauthenticated_or_different_systemd_parent(self):
        answers = {"service unavailable": None,
                   "MainPID differs": manager_service(main_pid=MANAGER_PID + 1),
                   "service not active": manager_service(active_state="inactive")}
        for label, answer in answers.items():
            with self.subTest(label):
                self.host()
                users, found = self.assert_helper_ambiguous(FixtureManagerQuery(answer))
                self.assertIn(f"{MANAGER_PID}:ambiguous:cwd", users)
                self.assertEqual(found, [])
        # A different same-user systemd, not the queried MainPID, as the parent.
        other = 4000
        self.host(ppid=other)
        self.proc.manager(other, start=MANAGER_START)
        users, found = self.assert_helper_ambiguous()
        self.assertIn(f"{other}:ambiguous:cwd", users)
        self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID)])
        # Parents the manager rule never considers: root-owned systemd, user-owned bash, an exited parent.
        for label, parent in {"root-owned systemd": dict(command="systemd", uid=0),
                              "user-owned shell": dict(command="bash"),
                              "exited parent": None}.items():
            with self.subTest(label):
                self.host(ppid=other)
                if parent is not None:
                    self.proc.entry(other, ppid=1, start=MANAGER_START, **parent)
                self.assert_helper_ambiguous()

    def test_pid1_reparenting(self):
        for ppid in (1, 0, HELPER_PID):
            with self.subTest(ppid=ppid):
                self.host(ppid=ppid)
                users, found = self.assert_helper_ambiguous()
                self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID)])

    def test_foreign_uid_without_owner_isolation(self):
        self.host(uid=os.getuid() + 1)
        users, found = self.assert_helper_ambiguous(isolated=False)
        self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID)])

    def test_wrong_command(self):
        for command in ("sd-pam", "(sd-pam", "sd-pam)", "(sd-pam) ", " (sd-pam)", "(sd-pamx)", "((sd-pam))",
                        "systemd", "(SD-PAM)"):
            with self.subTest(command=command):
                self.host(command=command)
                self.assert_helper_ambiguous()

    def changed_after_manager(self, change):
        """Run change after the manager rule completed its second reads, before the helper's second reads."""
        real = resources._user_manager

        def manager(*args, **kwargs):
            record = real(*args, **kwargs)
            change()
            return record
        return patch.object(resources, "_user_manager", manager)

    def test_changed_parent_start_command_or_owner_during_inspection(self):
        other = os.getuid() + 1

        def helper(**fields):
            return lambda: self.proc.restat(HELPER_PID, **fields)

        def parent(**fields):
            return lambda: self.proc.restat(MANAGER_PID, **fields)

        def owner(pid, uid):
            return lambda: self.proc.owners.__setitem__(self.proc.root / str(pid), uid)

        changes = {
            "helper start changed (PID reused)": helper(start=HELPER_START + 1),
            "helper reparented": helper(ppid=1),
            "helper renamed": helper(command="bash"),
            "helper became a zombie": helper(state="Z"),
            "helper owner changed": owner(HELPER_PID, other),
            "parent start changed (PID reused)": parent(start=MANAGER_START + 1),
            "parent renamed": parent(command="bash"),
            "parent became a zombie": parent(state="Z"),
            "parent owner changed": owner(MANAGER_PID, other),
            "PID 1 restarted": lambda: self.proc.restat(1, start=2),
        }
        for label, change in changes.items():
            # Between the manager's two queries: the manager's own second reads may also notice.
            with self.subTest(label, when="during inspection"):
                self.host()
                self.assert_helper_ambiguous(FixtureManagerQuery(manager_service(), between=change))
            # After the manager's second reads: only the helper's second reads can notice.
            with self.subTest(label, when="after the manager proof"):
                self.host()
                with self.changed_after_manager(change):
                    users, found = self.assert_helper_ambiguous()
                self.assertIn(f"{HELPER_PID}:ambiguous:environ", users)
        # The parent's first read, taken before the helper's inspection, must
        # match the manager's: a parent that differs then never qualifies.
        real = resources._proc_stat

        def first_parent_read_differs(pid, proc=resources.PROC):
            info = real(pid, proc)
            if pid == MANAGER_PID and info is not None and first_parent_read_differs.armed:
                first_parent_read_differs.armed = False
                return {**info, "start": info["start"] + 1}
            return info
        original = resources._user_manager_helper_first

        def arm(*args, **kwargs):
            first_parent_read_differs.armed = True
            try:
                return original(*args, **kwargs)
            finally:
                first_parent_read_differs.armed = False
        self.host()
        with patch.object(resources, "_proc_stat", first_parent_read_differs), \
                patch.object(resources, "_user_manager_helper_first", arm):
            first_parent_read_differs.armed = False
            users, found = self.assert_helper_ambiguous()
        # Only the helper's own first read of its parent differed; the manager is still authenticated.
        self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID)])

    def test_missing_malformed_unreadable_or_exited_identity(self):
        raws = {"missing": None, "no closing parenthesis": f"{HELPER_PID} ((sd-pam S {MANAGER_PID}\n",
                "truncated": f"{HELPER_PID} ((sd-pam)) S {MANAGER_PID}\n", "not text": b"\xff\xfe",
                "non-numeric start": (f"{HELPER_PID} ((sd-pam)) S {MANAGER_PID} 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 "
                                      "x 0 0\n")}
        for label, raw in raws.items():
            with self.subTest(label):
                self.host()
                entry = self.proc.root / str(HELPER_PID)
                (entry / "stat").unlink()
                if isinstance(raw, bytes):
                    (entry / "stat").write_bytes(raw)
                elif raw is not None:
                    (entry / "stat").write_text(raw)
                self.assert_helper_ambiguous()
        # The parent's stat is unreadable: the manager is never authenticated.
        self.host()
        (self.proc.root / str(MANAGER_PID) / "stat").unlink()
        users, found = self.assert_helper_ambiguous()
        self.assertEqual(found, [])
        # A helper stat or owner that becomes unreadable after inspection: the still-live helper stays ambiguous.
        for label, change in {"helper stat unreadable": lambda: (self.proc.root / str(HELPER_PID) / "stat").unlink(),
                              "parent stat unreadable": lambda: (self.proc.root / str(MANAGER_PID) / "stat").unlink()
                              }.items():
            with self.subTest(label):
                self.host()
                with self.changed_after_manager(change):
                    users, found = self.assert_helper_ambiguous()
                self.assertIn(f"{HELPER_PID}:ambiguous:environ", users)
        real_stat = Path.stat
        helper_entry = self.proc.root / str(HELPER_PID)
        reads = []

        def unreadable_owner(path, *args, **kwargs):
            if Path(path) == helper_entry:
                reads.append(path)
                if len(reads) > 1:
                    raise PermissionError(errno.EACCES, "denied", str(path))
            return real_stat(path, *args, **kwargs)
        self.host()
        with patch.object(Path, "stat", unreadable_owner):
            found = []
            users = resources.active_cache_users(self.cache, self.proc.root, uid=os.getuid(), disregarded=found,
                                                 manager_query=FixtureManagerQuery(manager_service()),
                                                 clock_ticks=lambda: MANAGER_HZ)
        self.assertNotIn(HELPER_PID, [record["pid"] for record in found])
        self.assertTrue([value for value in users if value.startswith(f"{HELPER_PID}:ambiguous:")], users)
        # A helper that exits during inspection leaves no helper record.
        self.host()
        with self.changed_after_manager(lambda: shutil.rmtree(self.proc.root / str(HELPER_PID))):
            _, found = self.scan()
        self.assertEqual(self.kinds(found), [(resources.USER_MANAGER_KIND, MANAGER_PID)])

    def test_parent_proof_changes_or_query_fails(self):
        def second(**fields):
            return FixtureManagerQuery(manager_service(), manager_service(**fields))

        cases = {
            "second query MainPID changed": second(main_pid=MANAGER_PID + 1),
            "second query timestamp changed": second(monotonic_usec=MANAGER_USEC + 1),
            "second query not active": second(active_state="deactivating"),
            "second query failed": FixtureManagerQuery(manager_service(), None),
            "first query failed": FixtureManagerQuery(None, manager_service()),
            "PID 1 changed between queries": FixtureManagerQuery(
                manager_service(), between=lambda: self.proc.restat(1, start=2)),
            "PID 1 owner changed between queries": FixtureManagerQuery(
                manager_service(), between=lambda: self.proc.owners.__setitem__(self.proc.root / "1", os.getuid())),
        }
        for label, query in cases.items():
            with self.subTest(label):
                self.host()
                users, found = self.assert_helper_ambiguous(query)
                self.assertIn(f"{MANAGER_PID}:ambiguous:cwd", users)
                self.assertEqual(found, [])

    def test_readable_cache_reference_blocks(self):
        (self.cache / "object").write_text("cached")
        cases = {
            "cwd": dict(cwd=self.cache),
            "root": dict(root=self.cache),
            "fd:3": dict(descriptor=self.cache / "object"),
        }
        for variable in sorted(resources.CACHE_ENVIRONMENT):
            cases["env:" + variable] = dict(cwd=self.base,
                                            environ=os.fsencode(variable) + b"=" + os.fsencode(self.cache) + b"\0")
        for label, references in cases.items():
            with self.subTest(label):
                self.host(**references)
                users, found = self.scan()
                self.assertIn(f"{HELPER_PID}:" + label, users)
                self.assertNotIn(HELPER_PID, [record["pid"] for record in found])
        # Mixed: a readable descriptor beside unreadable cwd, root, and environ.
        self.host(descriptor=self.cache / "object")
        users, found = self.scan()
        self.assertIn(f"{HELPER_PID}:fd:3", users)
        self.assertIn(f"{HELPER_PID}:ambiguous:cwd", users)
        self.assertIn(f"{HELPER_PID}:ambiguous:environ", users)
        self.assertNotIn(HELPER_PID, [record["pid"] for record in found])
        # A parent with a readable reference fails the manager rule and cannot qualify its child.
        self.reset()
        self.proc.system_manager()
        self.proc.manager(descriptor=self.cache / "object")
        self.proc.helper()
        users, found = self.assert_helper_ambiguous()
        self.assertIn(f"{MANAGER_PID}:fd:3", users)
        self.assertEqual(found, [])
        self.assertTrue((self.cache / "object").is_file())

    def test_scan_order_independent(self):
        real = Path.iterdir

        def ordered(first):
            def iterdir(path):
                entries = list(real(path))
                if Path(path) != self.proc.root:
                    return iter(entries)
                return iter(sorted(entries, key=lambda entry: (entry.name != str(first), entry.name)))
            return patch.object(Path, "iterdir", iterdir)

        results = {}
        for first in (HELPER_PID, MANAGER_PID):
            with self.subTest(first=first):
                self.host()
                with ordered(first):
                    users, found = self.scan()
                self.assertEqual(users, [])
                results[first] = sorted(found, key=lambda record: record["pid"])
        self.assertEqual(results[HELPER_PID], results[MANAGER_PID])
        self.assertEqual(results[HELPER_PID][1], self.helper_record())

    def test_proof_not_reused_across_scans_or_children(self):
        self.host()
        _, found = self.scan()
        self.assertEqual(self.kinds(found)[1], (resources.USER_MANAGER_HELPER_KIND, HELPER_PID))
        # A later scan whose authentication fails gets no helper record from the earlier success.
        for query in (FixtureManagerQuery(None), FixtureManagerQuery(manager_service(), None)):
            with self.subTest(calls=len(query.answers)):
                users, found = self.assert_helper_ambiguous(query)
                self.assertEqual(found, [])
        # A second child in the same cleanup scans afresh: its manager proof is queried again.
        query = FixtureManagerQuery(manager_service(), manager_service(), None)
        _, first = self.scan(query)
        users, second = self.scan(query)
        self.assertEqual(len(first), 2)
        self.assertIn(f"{HELPER_PID}:ambiguous:cwd", users)
        self.assertEqual(second, [])

    def test_macos_unchanged(self):
        table = {301: {"vnode_error": True, "environment": None}}
        found = []
        refuse = FixtureManagerQuery(manager_service())
        with patch.object(resources, "darwin", return_value=FakeDarwin(table)), \
                patch.object(resources, "_user_manager_helper_first",
                             side_effect=AssertionError("Linux helper rule on macOS")), \
                patch.object(resources, "_user_manager_helper", side_effect=AssertionError("Linux helper rule on macOS")), \
                patch.object(resources, "query_user_manager", side_effect=AssertionError("query on macOS")):
            users = resources.active_cache_users(self.cache, backend=resources.DARWIN_BACKEND, disregarded=found,
                                                 manager_query=refuse)
        self.assertEqual(users, ["301:ambiguous:cwd", "301:ambiguous:environ"])
        self.assertEqual((found, refuse.calls), ([], []))


class UserManagerHelperPathRemovalTests(IsolatedState):
    """Temporary-path teardown and recovery retain the disregarded manager helper.

    The fixture manager and helper start far later than any real temporary
    path, so no creation bound can disregard them in place of the rules.
    """

    START = 10**10
    USEC = START * (1_000_000 // MANAGER_HZ)
    HELPER_START = START + 2

    def setUp(self):
        super().setUp()
        self.proc = FixtureProc(self.base / "proc")
        self.proc.system_manager()
        self.proc.manager(start=self.START)
        self.proc.helper(start=self.HELPER_START)
        self.manager_line = manager_line(start=self.START, usec=self.USEC)
        self.line = helper_line(start=self.HELPER_START, parent_start=self.START)
        inspector = functools.partial(resources.active_cache_users, proc=self.proc.root)
        for name, value in (("active_cache_users", inspector), ("system_clock_ticks", lambda: MANAGER_HZ)):
            patcher = patch.object(resources, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.query = self.answer(manager_service(monotonic_usec=self.USEC))
        owned = self.proc.owned()
        owned.start()
        self.addCleanup(owned.stop)
        self.ssh_line = ("Disregarded Linux SSH session: pid=4242 parent=100 command=sshd-session "
                         "parent_command=sshd-session reason=" + resources.SSH_DISREGARD_REASON)

    def answer(self, *answers):
        query = FixtureManagerQuery(*answers)
        patcher = patch.object(resources, "query_user_manager", query)
        patcher.start()
        self.addCleanup(patcher.stop)
        return query

    def path_identity(self, name="disposable"):
        path = self.base / name
        path.mkdir(mode=0o700)
        info = path.lstat()
        return {"path": str(path), "device": info.st_dev, "inode": info.st_ino}

    def orphan(self, task, cross_boot=False):
        """An invocation whose owner is gone; cross_boot keeps its creation tick and sessions from applying."""
        orphan = resources.Invocation.create(task, self.checkout, ["true"])
        os.close(orphan._owner_lock)
        orphan._owner_lock = None
        value = json.loads((orphan.path / "inventory.json").read_text())
        value["owner"]["birth"] = {"start_ticks": -1, "boot_id": "another-boot"}
        if cross_boot:
            for entry in value["resources"]:
                if entry["kind"] == "path":
                    entry["identity"]["boot_id"] = "another-boot"
        resources.write_json(orphan.path / "inventory.json", value)
        return orphan

    def test_removal_detail_and_owner_cleanup(self):
        identity = self.path_identity()
        state, detail = resources.remove_path(identity)
        self.assertEqual(state, "removed")
        self.assertEqual(detail, "removed disposable path " + identity["path"] + "; " + self.manager_line
                         + "; " + self.line)
        self.assertFalse(Path(identity["path"]).exists())
        self.assertEqual(len(self.query.calls), 2)
        owner = resources.Invocation.create("helper-owner", self.checkout, ["true"])
        self.assertEqual(owner.finish(outcome="success"), [])
        [entry] = [value for value in resources.load_inventory(owner.path)["resources"] if value["kind"] == "path"]
        self.assertEqual(entry["state"], "removed")
        self.assertTrue(entry["detail"].endswith("; " + self.manager_line + "; " + self.line), entry["detail"])

    def test_recovery_retains_helper_report(self):
        orphan = self.orphan("helper-orphan")
        complete, actions = resources.recover(orphan.path)
        self.assertTrue(complete)
        [action] = [value for value in actions if value["kind"] == "path"]
        self.assertTrue(action["detail"].endswith("; " + self.line), action["detail"])
        logged = [json.loads(line) for line in (orphan.path / "recovery.jsonl").read_text().splitlines()]
        detail = [value for value in logged if value["kind"] == "path"][0]["detail"]
        self.assertIn(self.manager_line, detail)
        self.assertIn(self.line, detail)

    def test_refusal_retains_reports_once(self):
        identity = self.path_identity()
        self.proc.ssh()
        # An unrelated uninspectable process still blocks, through every rescan.
        self.proc.entry(4343, command="bash", ppid=100)
        with patch.object(resources.time, "sleep"), self.assertRaises(resources.Refusal) as refused:
            resources.remove_path(identity)
        message = str(refused.exception)
        self.assertIn("is in use: 4343:ambiguous:cwd", message)
        for line in (self.ssh_line, self.manager_line, self.line):
            self.assertEqual(message.count(line), 1, line)
        self.assertLess(message.index(self.ssh_line), message.index(self.manager_line))
        self.assertLess(message.index(self.manager_line), message.index(self.line))
        self.assertTrue(Path(identity["path"]).is_dir())
        # Every rescan authenticated the manager afresh; the helper added no query.
        self.assertEqual(len(self.query.calls), 8)

    def test_unproven_helper_keeps_path_and_recovery_refusal(self):
        shutil.rmtree(self.proc.root / str(HELPER_PID))
        self.proc.helper(start=self.START + MANAGER_HZ + 1)
        identity = self.path_identity()
        with patch.object(resources.time, "sleep"), self.assertRaises(resources.Refusal) as refused:
            resources.remove_path(identity)
        message = str(refused.exception)
        self.assertIn(f"{HELPER_PID}:ambiguous:cwd", message)
        self.assertNotIn("Disregarded Linux user manager helper", message)
        self.assertTrue(Path(identity["path"]).is_dir())

        orphan = self.orphan("helper-unproven", cross_boot=True)
        [path_entry] = [value for value in resources.load_inventory(orphan.path)["resources"]
                        if value["kind"] == "path"]
        with patch.object(resources.time, "sleep"):
            complete, actions = resources.recover(orphan.path)
        self.assertFalse(complete)
        [action] = [value for value in actions if value["kind"] == "path"]
        self.assertEqual(action["action"], "refused")
        self.assertIn(f"{HELPER_PID}:ambiguous:cwd", action["detail"])
        self.assertNotIn("Disregarded Linux user manager helper", action["detail"])
        self.assertTrue(Path(path_entry["identity"]["path"]).is_dir())
        logged = [json.loads(line) for line in (orphan.path / "recovery.jsonl").read_text().splitlines()]
        refused_paths = [value for value in logged if value["kind"] == "path"]
        self.assertEqual(refused_paths[0]["action"], "refused")
        self.assertIn(f"{HELPER_PID}:ambiguous:cwd", refused_paths[0]["detail"])


@unittest.skipUnless(os.environ.get("CONVEYOR_VALIDATION_DOCKER") == "1",
                     "real Docker fixtures run through make test-validation-docker")
class DockerLifecycleTests(IsolatedState):
    @classmethod
    def setUpClass(cls):
        result = subprocess.run(["docker", "info", "--format", "{{.ServerVersion}}"], capture_output=True, text=True)
        if result.returncode != 0:
            raise AssertionError("Docker is required for make test-validation-docker; this is missing evidence, "
                                 "not a skipped pass: " + result.stderr.strip()[-200:])

    def labelled_containers(self, invocation_id):
        result = subprocess.run(["docker", "ps", "-a", "-q", "--no-trunc", "--filter",
                                 f"label={resources.LABEL_INVOCATION}={invocation_id}"],
                                capture_output=True, text=True, check=True)
        return result.stdout.split()

    def remove_foreign(self, kind, identifier):
        args = ["rm", "-f", identifier] if kind == "container" else ["network", "rm", identifier]
        self.addCleanup(subprocess.run, ["docker", *args], capture_output=True)

    def owned(self, task):
        invocation = resources.Invocation.create(task, ROOT, ["docker-fixture"], tmp=False)
        self.addCleanup(lambda: invocation.owner and invocation.finish())
        return invocation

    def test_two_invocations_in_one_checkout_own_distinct_bounded_containers(self):
        first, second = self.owned("docker-a"), self.owned("docker-b")
        a = resources.start_managed_postgres(first, ROOT)
        b = resources.start_managed_postgres(second, ROOT)
        self.assertNotEqual(a["project"], b["project"])
        self.assertNotEqual(a["port"], b["port"])
        self.assertNotEqual(a["container"], b["container"])
        info = json.loads(subprocess.run(["docker", "inspect", a["container"]], capture_output=True, text=True,
                                         check=True).stdout)[0]
        budget = resources.postgres_budget({})
        self.assertEqual(info["HostConfig"]["Memory"], budget["memory_bytes"])
        self.assertEqual(info["HostConfig"]["Tmpfs"], {"/var/lib/postgresql/data": "size=" + budget["tmpfs"]})
        self.assertEqual(info["Config"]["Labels"][resources.LABEL_INVOCATION], first.id)
        ready = subprocess.run(["docker", "exec", b["container"], "pg_isready", "-U", "conveyor"], capture_output=True)
        self.assertEqual(ready.returncode, 0)
        self.assertEqual(first.finish(), [])
        self.assertEqual(self.labelled_containers(first.id), [])
        network = next(entry for entry in first.inventory["resources"] if entry["kind"] == "network")
        self.assertIsNone(resources._docker_inspect("network", network["identity"]["id"]))
        self.assertEqual(self.labelled_containers(second.id), [b["container"]])
        state = subprocess.run(["docker", "inspect", "-f", "{{.State.Running}}", b["container"]],
                               capture_output=True, text=True, check=True).stdout.strip()
        self.assertEqual(state, "true", "tearing down one invocation stopped the other")
        self.assertEqual(second.finish(), [])
        self.assertEqual(self.labelled_containers(second.id), [])

    def test_budget_override_is_applied(self):
        invocation = self.owned("docker-budget")
        env = dict(os.environ, CONVEYOR_TEST_POSTGRES_MEMORY="1g", CONVEYOR_TEST_POSTGRES_TMPFS_SIZE="256m")
        managed = resources.start_managed_postgres(invocation, ROOT, env)
        info = json.loads(subprocess.run(["docker", "inspect", managed["container"]], capture_output=True,
                                         text=True, check=True).stdout)[0]
        self.assertEqual(info["HostConfig"]["Memory"], 1024 ** 3)
        self.assertEqual(info["HostConfig"]["Tmpfs"], {"/var/lib/postgresql/data": "size=256m"})
        self.assertEqual(invocation.finish(), [])

    def test_occupied_pinned_port_creates_nothing(self):
        invocation = self.owned("docker-pinned")
        with socket.socket() as occupied:
            occupied.bind(("127.0.0.1", 0))
            occupied.listen()
            env = dict(os.environ, CONVEYOR_TEST_POSTGRES_PORT=str(occupied.getsockname()[1]))
            with self.assertRaisesRegex(resources.Refusal, "occupied"):
                resources.start_managed_postgres(invocation, ROOT, env)
        self.assertEqual(self.labelled_containers(invocation.id), [])
        self.assertEqual(invocation.inventory["resources"], [])

    def test_teardown_spares_lookalike_container_and_external_network(self):
        suffix = secrets.token_hex(4)
        network = subprocess.run(["docker", "network", "create", "conveyor-test-external-" + suffix],
                                 capture_output=True, text=True, check=True).stdout.strip()
        self.remove_foreign("network", network)
        invocation = self.owned("docker-lookalike")
        lookalike = subprocess.run(["docker", "create", "--name", f"conveyor-test-{invocation.id}-lookalike",
                                    "--label", f"{resources.LABEL_INVOCATION}=another-{suffix}",
                                    "--label", f"{resources.COMPOSE_PROJECT_LABEL}=conveyor-test-{invocation.id}x",
                                    "postgres:16-alpine"], capture_output=True, text=True, check=True).stdout.strip()
        self.remove_foreign("container", lookalike)
        managed = resources.start_managed_postgres(invocation, ROOT, external_network="conveyor-test-external-" + suffix)
        kinds = [entry["kind"] for entry in invocation.inventory["resources"]]
        self.assertEqual(kinds, ["container"], "an external network must never become an owned resource")
        self.assertEqual(invocation.finish(), [])
        self.assertIsNone(resources._docker_inspect("container", managed["container"]))
        self.assertIsNotNone(resources._docker_inspect("container", lookalike))
        self.assertIsNotNone(resources._docker_inspect("network", network))

    def start_owner(self, task, crash_at=None):
        script = "\n".join([
            "import sys, time",
            f"sys.path.insert(0, {str(SCRIPTS)!r})",
            "import validation_resources as r",
            f"inv = r.Invocation.create({task!r}, {str(ROOT)!r}, ['owner'], tmp=False)",
            "print(inv.path, flush=True)",
            f"r.start_managed_postgres(inv, {str(ROOT)!r})",
            "print('ready', flush=True)",
            "time.sleep(300)",
        ])
        env = dict(os.environ)
        if crash_at:
            env[resources.CRASH_AT] = crash_at
        process = subprocess.Popen([sys.executable, "-c", script], env=env, stdout=subprocess.PIPE, text=True)
        self.addCleanup(process.stdout.close)
        self.pids.append(process.pid)
        path = Path(process.stdout.readline().strip())
        status = process.stdout.readline().strip()
        return process, path, status

    def test_identity_mismatch_refuses_recovery_until_the_sealed_identity_matches(self):
        process, path, status = self.start_owner("docker-mismatch")
        self.assertEqual(status, "ready")
        process.kill()
        process.wait()
        inventory = resources.load_inventory(path)
        container = next(entry for entry in inventory["resources"] if entry["kind"] == "container")
        self.addCleanup(subprocess.run, ["docker", "rm", "-f", container["identity"]["id"]], capture_output=True)
        decoy = subprocess.run(["docker", "create", "--label", f"{resources.LABEL_INVOCATION}={inventory['invocation']}",
                                "postgres:16-alpine"], capture_output=True, text=True, check=True).stdout.strip()
        self.remove_foreign("container", decoy)
        original = (path / "inventory.json").read_bytes()
        value = json.loads(original)
        for entry in value["resources"]:
            if entry["kind"] == "container":
                entry["identity"]["id"] = decoy
        resources.write_json(path / "inventory.json", value)
        complete, actions = resources.recover(path)
        self.assertFalse(complete)
        self.assertIn(("container", "refused"), [(action["kind"], action["action"]) for action in actions])
        self.assertIsNotNone(resources._docker_inspect("container", decoy))
        self.assertIsNotNone(resources._docker_inspect("container", container["identity"]["id"]))
        resources.write_json(path / "inventory.json", json.loads(original))
        complete, _ = resources.recover(path)
        self.assertTrue(complete)
        self.assertIsNone(resources._docker_inspect("container", container["identity"]["id"]))
        self.assertIsNotNone(resources._docker_inspect("container", decoy))

    def test_forced_termination_between_create_seal_and_start(self):
        process, path, _ = self.start_owner("docker-crash-create", crash_at="container-after-create")
        process.wait(timeout=120)
        self.assertEqual(process.returncode, -signal.SIGKILL)
        inventory_id = resources.load_inventory(path)["invocation"]
        for identifier in self.labelled_containers(inventory_id):
            self.remove_foreign("container", identifier)
        report = resources.inspect_invocation(path)
        self.assertEqual({entry["classification"] for entry in report["resources"]}, {"pending"})
        complete, actions = resources.recover(path)
        self.assertFalse(complete, "pending resources without sealed identities must remain operator work")
        self.assertEqual({action["action"] for action in actions}, {"refused"})
        self.assertEqual(len(self.labelled_containers(inventory_id)), 1, "recovery removed an unsealed container")
        networks = subprocess.run(["docker", "network", "ls", "-q", "--filter",
                                   f"label={resources.LABEL_INVOCATION}={inventory_id}"],
                                  capture_output=True, text=True, check=True).stdout.split()
        for identifier in networks:
            self.addCleanup(subprocess.run, ["docker", "network", "rm", identifier], capture_output=True)

        process, path, _ = self.start_owner("docker-crash-seal", crash_at="container-after-seal")
        process.wait(timeout=120)
        report = resources.inspect_invocation(path)
        self.assertEqual({entry["classification"] for entry in report["resources"]}, {"abandoned"})
        complete, _ = resources.recover(path)
        self.assertTrue(complete)
        self.assertEqual(self.labelled_containers(resources.load_inventory(path)["invocation"]), [])

        process, path, status = self.start_owner("docker-crash-running")
        self.assertEqual(status, "ready")
        process.kill()
        process.wait()
        complete, _ = resources.recover(path)
        self.assertTrue(complete)
        self.assertEqual(self.labelled_containers(resources.load_inventory(path)["invocation"]), [])

    def test_fixture_lifecycle_runs_the_gate_against_its_owned_database(self):
        state = self.base / "fixture"
        gate = 'test -n "$CONVEYOR_TEST_DATABASE_URL" && test -n "$CONVEYOR_VALIDATION_INVOCATION" && test -d "$TMPDIR"'
        result = subprocess.run([sys.executable, str(SCRIPTS / "validation_fixtures.py"), "run",
                                 "--task", "docker-fixture", "--managed-postgres", "--backend", "postgres",
                                 "--url-env", "TEST_DATABASE_URL", "--prepared-url-env", "CONVEYOR_TEST_DATABASE_URL",
                                 "--minimum-free-bytes", "1", "--state", str(state), "--", "sh", "-c", gate],
                                cwd=ROOT, env=self.env, capture_output=True, text=True, timeout=600)
        self.assertEqual(result.returncode, 0, result.stderr[-2000:])
        phases = [(p["phase"], p["outcome"]) for p in map(json.loads, (state / "phases.jsonl").read_text().splitlines())]
        for expected in (("managed-container", "success"), ("prepare", "success"), ("before-snapshot", "success"),
                         ("gate", "success"), ("after-snapshot", "success"), ("teardown", "success"),
                         ("resource-cleanup", "success")):
            self.assertIn(expected, phases)
        [path] = self.invocations("docker-fixture")
        inventory = resources.load_inventory(path)
        self.assertEqual(inventory["state"], "completed")
        database = next(entry for entry in inventory["resources"] if entry["kind"] == "database")
        self.assertEqual(database["identity"]["server"], "invocation-container")
        self.assertTrue(database["identity"]["incarnation"])
        self.assertEqual(self.labelled_containers(inventory["invocation"]), [])
        self.assertNotIn("conveyor:conveyor", (path / "inventory.json").read_text())


class TestSelectionTests(unittest.TestCase):
    """Class-level opt-in decorators stay on the classes they select.

    A skip decorator marks the class directly below it, so a helper inserted
    between a decorator and its class silently takes the decorator over. Only
    DockerLifecycleTests is opt-in (CONVEYOR_VALIDATION_DOCKER=1, through make
    test-validation-docker), and HostBackendTests needs a process backend; no
    other class in this module may carry a class-level skip.
    """

    def test_only_the_selected_classes_carry_a_class_skip(self):
        docker = os.environ.get("CONVEYOR_VALIDATION_DOCKER") == "1"
        expected = {"DockerLifecycleTests": not docker, "HostBackendTests": not HAS_BACKEND}
        classes = {name: value for name, value in globals().items()
                   if isinstance(value, type) and value.__module__ == __name__}
        self.assertIn("DockerLifecycleTests", classes)
        for name, value in sorted(classes.items()):
            with self.subTest(name):
                self.assertEqual(value.__dict__.get("__unittest_skip__", False), expected.get(name, False))
        if not docker:
            self.assertEqual(DockerLifecycleTests.__unittest_skip_why__,
                             "real Docker fixtures run through make test-validation-docker")


if __name__ == "__main__":
    unittest.main()
