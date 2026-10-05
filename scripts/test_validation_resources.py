"""Reproducible fixtures for owned validation resources and explicit recovery.

ResourceTests use real local child processes and run in `make test-validation`.
DockerLifecycleTests use real Docker and PostgreSQL and run in
`make test-validation-docker`, which fails rather than skips without Docker
(component-verification-strategy, "Validation resource ownership and recovery").
"""

import errno
import json
import os
from pathlib import Path
import plistlib
import secrets
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time
import unittest
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


if __name__ == "__main__":
    unittest.main()
