"""Reproducible fixtures for owned validation resources and explicit recovery.

ResourceTests use real local child processes and run in `make test-validation`.
DockerLifecycleTests use real Docker and PostgreSQL and run in
`make test-validation-docker`, which fails rather than skips without Docker
(component-verification-strategy, "Validation resource ownership and recovery").
"""

import json
import os
from pathlib import Path
import secrets
import signal
import socket
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


def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
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
                                            ["make", "postgres://admin:private@db/x_test", "check"])
        second = resources.Invocation.create("resource-task", self.checkout, ["make"])
        self.addCleanup(second.finish)
        self.assertNotEqual(first.path, second.path)
        self.assertEqual(first.path.stat().st_mode & 0o777, 0o700)
        self.assertEqual((first.path / "inventory.json").stat().st_mode & 0o777, 0o600)
        text = (first.path / "inventory.json").read_text()
        self.assertNotIn("private", text)
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
