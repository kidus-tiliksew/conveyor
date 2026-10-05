"""Deterministic tests for owned validation fixture lifecycle and diagnostics."""

import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import Mock, patch

import validation_fixtures as fixtures
import validation_resources as resources


def _alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    stat = Path(f"/proc/{pid}/stat")
    return not stat.exists() or stat.read_text().rsplit(")", 1)[1].split()[0] != "Z"


class FixtureTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.config = {
            "backend": "postgres",
            "url_env": "ROOT_URL",
            "prepared_url_env": "CONVEYOR_TEST_DATABASE_URL",
            "external_network_env": "EXTERNAL_NETWORK",
            "database_prefix": "conveyor",
            "minimum_free_bytes": 1,
            "timeout": "1s",
        }
        self.env = {"ROOT_URL": "postgres://user:private@127.0.0.1:5432/conveyor_test?sslmode=disable"}
        self.calls = []
        # Invocation inventories and temporary children stay inside the fixture.
        isolation = patch.dict(os.environ, {
            "XDG_STATE_HOME": str(self.root / "state"),
            resources.TMP_ROOT: str(self.root / "cache"),
            resources.ALLOW_RAM_TMP: "1",
        })
        isolation.start()
        self.addCleanup(isolation.stop)
        os.environ.pop(resources.BINDING, None)

    def completed(self, argv, env, capture=True):
        self.calls.append((list(argv), dict(env)))
        if "incarnation" in argv:
            return subprocess.CompletedProcess(argv, 0, json.dumps({"incarnation": "16384"}), "")
        if "probe" in argv:
            database = fixtures._database_from_dsn(self.config["backend"], env[self.config["prepared_url_env"]])
            return subprocess.CompletedProcess(argv, 0, json.dumps({
                "identity": self.config["backend"], "version": "test",
                "configuration": "127.0.0.1:5432", "instance": database,
            }), "")
        return subprocess.CompletedProcess(argv, 0, "", "")

    def test_two_preparations_are_unique_and_credentials_are_not_retained(self):
        with patch.object(fixtures, "_run", side_effect=self.completed):
            first, first_env = fixtures.prepare(self.config, self.env, self.root / "first")
            second, second_env = fixtures.prepare(self.config, self.env, self.root / "second")
        self.assertNotEqual(first["database"], second["database"])
        self.assertTrue(first["database"].endswith("_test"))
        self.assertNotEqual(first_env["CONVEYOR_TEST_DATABASE_URL"], second_env["CONVEYOR_TEST_DATABASE_URL"])
        for path in self.root.rglob("*"):
            if path.is_file():
                self.assertNotIn("private", path.read_text())

    def test_missing_configuration_and_capacity_are_distinct(self):
        with self.assertRaisesRegex(fixtures.FixtureError, "required configuration.*unset"):
            fixtures.prepare(self.config, {}, self.root / "missing")
        self.config["minimum_free_bytes"] = 10**30
        with self.assertRaisesRegex(fixtures.FixtureError, "capacity probe failed"):
            fixtures.prepare(self.config, self.env, self.root / "capacity")
        missing = (self.root / "missing" / "phases.jsonl").read_text()
        self.assertIn("configuration-failure", missing)

    def test_singlestore_default_requires_usable_creation_headroom(self):
        self.config.update(
            backend="singlestore", url_env="SS_URL",
            prepared_url_env="CONVEYOR_TEST_SINGLESTORE_URL", minimum_free_bytes=0,
        )
        safe = {"SS_URL": "root:private@tcp(127.0.0.1:3306)/conveyor_test"}
        usage = shutil.disk_usage(self.root)
        simulated = usage.__class__(usage.total, usage.used, fixtures.SINGLESTORE_MINIMUM_BYTES - 1)
        with patch.object(fixtures.shutil, "disk_usage", return_value=simulated):
            with self.assertRaisesRegex(fixtures.FixtureError, "capacity probe failed"):
                fixtures.prepare(self.config, safe, self.root / "headroom")

    def test_external_network_is_inspected_but_never_pruned(self):
        self.env["EXTERNAL_NETWORK"] = "shared-ci"

        def unavailable(argv, env, capture=True):
            self.calls.append((list(argv), dict(env)))
            return subprocess.CompletedProcess(argv, 1, "", "network absent")

        with patch.object(fixtures, "_run", side_effect=unavailable):
            with self.assertRaisesRegex(fixtures.FixtureError, "will not create, prune, or delete"):
                fixtures.prepare(self.config, self.env, self.root / "network")
        flattened = " ".join(" ".join(call) for call, _ in self.calls)
        self.assertIn("docker network inspect shared-ci", flattened)
        for forbidden in ("prune", " rm", "delete"):
            self.assertNotIn(forbidden, flattened)

    def test_singlestore_requires_test_parent_and_uses_repository_driver(self):
        self.config.update(backend="singlestore", url_env="SS_URL", prepared_url_env="CONVEYOR_TEST_SINGLESTORE_URL")
        unsafe = {"SS_URL": "root:private@tcp(127.0.0.1:3306)/production"}
        with self.assertRaisesRegex(fixtures.FixtureError, "must end in _test"):
            fixtures.prepare(self.config, unsafe, self.root / "unsafe")
        safe = {"SS_URL": "root:private@tcp(127.0.0.1:3306)/conveyor_test"}
        with patch.object(fixtures, "_run", side_effect=self.completed):
            ownership, _ = fixtures.prepare(self.config, safe, self.root / "safe")
        self.assertEqual(ownership["backend"], "singlestore")
        self.assertTrue(any(call[:3] == ["go", "run", fixtures.HELPER] for call, _ in self.calls))
        self.assertFalse(any(call and Path(call[0]).name == "mysql" for call, _ in self.calls))

    def test_teardown_refuses_changed_ownership_and_drops_only_owned_name(self):
        with patch.object(fixtures, "_run", side_effect=self.completed):
            ownership, _ = fixtures.prepare(self.config, self.env, self.root / "owned")
            altered = dict(ownership, database="production")
            with self.assertRaisesRegex(fixtures.FixtureError, "ownership identity changed"):
                fixtures.teardown(self.config, self.env, altered, self.root / "owned")
            fixtures.teardown(self.config, self.env, ownership, self.root / "owned")
        drop = [call for call, _ in self.calls if "drop" in call]
        self.assertEqual(len(drop), 1)
        self.assertIn(ownership["database"], drop[0])
        released = json.loads((self.root / "owned" / "ownership.json").read_text())
        self.assertEqual(released["state"], "released")

    def test_diagnostic_redacts_driver_credentials_but_keeps_operation(self):
        error = fixtures._diagnostic(
            "postgres", "database creation", "127.0.0.1:5432",
            "dial postgres://user:private@127.0.0.1/db failed",
        )
        self.assertNotIn("private", str(error))
        self.assertIn("database creation", str(error))
        self.assertIn("127.0.0.1:5432", str(error))

    def test_diagnostic_ignores_go_run_exit_trailer(self):
        error = fixtures._diagnostic(
            "singlestore", "database creation", "127.0.0.1:3306",
            "validation fixture singlestore create failed: not enough disk\nexit status 2\n",
        )
        self.assertIn("not enough disk", str(error))
        self.assertNotIn("exit status", str(error))

    def test_diagnostic_redacts_driver_username(self):
        error = fixtures._diagnostic(
            "postgres", "database creation", "127.0.0.1:5432",
            "failed to connect to `user=fixture-admin database=postgres`: connection refused",
        )
        self.assertNotIn("fixture-admin", str(error))
        self.assertIn("user=[redacted]", str(error))

    def lifecycle_doubles(self, order, child_env=None):
        ownership = {"backend": "postgres", "database": "conveyor_a1_test",
                     "prepared_url_env": "CONVEYOR_TEST_DATABASE_URL", "url_env": "ROOT_URL"}
        child_env = child_env or dict(os.environ, CONVEYOR_TEST_DATABASE_URL=self.env["ROOT_URL"])
        return (
            patch.object(fixtures, "prepare", side_effect=lambda *a, **k: (order.append("prepare") or (ownership, dict(child_env)))),
            patch.object(fixtures, "probe", side_effect=lambda c, e, o, s, label: order.append(label) or {}),
            patch.object(fixtures, "teardown", side_effect=lambda *a, **k: order.append("teardown")),
        )

    def test_lifecycle_orders_snapshots_around_gate_and_teardown_on_failure(self):
        order = []
        marker = self.root / "gate-ran"
        prepare, probe, teardown = self.lifecycle_doubles(order)
        with prepare, probe, teardown:
            status = fixtures.run_lifecycle(self.config, self.root / "run",
                                            ["sh", "-c", f"touch {marker}; exit 7"])
        self.assertEqual(status, 7)
        self.assertTrue(marker.exists())
        self.assertEqual(order, ["prepare", "before-snapshot", "after-snapshot", "teardown"])
        phases = [json.loads(line) for line in (self.root / "run" / "phases.jsonl").read_text().splitlines()]
        self.assertIn(("gate", "failure"), [(p["phase"], p["outcome"]) for p in phases])
        self.assertIn(("resource-cleanup", "success"), [(p["phase"], p["outcome"]) for p in phases])
        inventories = list((self.root / "state" / "conveyor" / "manual-validation" / "invocations").iterdir())
        self.assertEqual(len(inventories), 1)
        inventory = resources.load_inventory(inventories[0])
        self.assertEqual(inventory["state"], "completed")
        self.assertTrue(all(entry["state"] in ("removed", "absent") for entry in inventory["resources"]))

    def test_gate_timeout_and_signal_stop_descendants_that_outlive_the_gate(self):
        for mode in ("timeout", "sigterm"):
            with self.subTest(mode=mode):
                order = []
                pid_file = self.root / (mode + ".pid")
                command = ["python3", "-c", "import subprocess, sys, time; "
                           "p = subprocess.Popen(['sleep', '30']); open(sys.argv[1], 'w').write(str(p.pid)); "
                           "time.sleep(30)", str(pid_file)]
                prepare, probe, teardown = self.lifecycle_doubles(order)
                timer = None
                if mode == "sigterm":
                    def interrupt():
                        deadline = time.time() + 5
                        while not pid_file.exists() and time.time() < deadline:
                            time.sleep(0.02)
                        os.kill(os.getpid(), signal.SIGTERM)
                    timer = threading.Thread(target=interrupt)
                    timer.start()
                with prepare, probe, teardown:
                    status = fixtures.run_lifecycle(self.config, self.root / mode, command,
                                                    gate_timeout=1.5 if mode == "timeout" else 0)
                if timer is not None:
                    timer.join()
                self.assertEqual(status, 124 if mode == "timeout" else 128 + signal.SIGTERM)
                self.assertFalse(_alive(int(pid_file.read_text())), "descendant survived the gate")
                self.assertEqual(order[-2:], ["after-snapshot", "teardown"])
                phases = [json.loads(line) for line in (self.root / mode / "phases.jsonl").read_text().splitlines()]
                gate = [p["outcome"] for p in phases if p["phase"] == "gate"]
                self.assertEqual(gate[-1], "timeout" if mode == "timeout" else "interrupted")

    def test_managed_postgres_feeds_owned_container_into_fixture_preparation(self):
        seen = {}

        def managed(invocation, checkout, env, external_network=None):
            return {"url": "postgres://conveyor:conveyor@127.0.0.1:20001/conveyor_test?sslmode=disable",
                    "port": 20001, "project": "conveyor-test-" + invocation.id, "container": "c" * 64,
                    "budget": {"memory": "2g", "tmpfs": "1g"}}

        def prepare(config, base_env, state, invocation=None, server=None):
            seen.update(url=base_env[config["url_env"]], server=server, invocation=invocation)
            raise fixtures.FixtureError("stop after preparation")

        with patch.object(resources, "start_managed_postgres", side_effect=managed), \
                patch.object(fixtures, "prepare", side_effect=prepare):
            status = fixtures.run_lifecycle(self.config, self.root / "managed", ["true"], managed_postgres=True)
        self.assertEqual(status, 2)
        self.assertIn("127.0.0.1:20001", seen["url"])
        self.assertEqual(seen["server"], {"server": "invocation-container", "container": "c" * 64})
        phases = (self.root / "managed" / "phases.jsonl").read_text()
        self.assertIn('"managed-container"', phases)
        self.assertIn('"not-run"', phases)

    def test_owned_database_is_registered_sealed_and_released_in_the_inventory(self):
        invocation = resources.Invocation.create("fixture-task", fixtures.ROOT, ["test"], tmp=False)
        self.addCleanup(lambda: invocation.owner and invocation.finish())
        with patch.object(fixtures, "_run", side_effect=self.completed):
            ownership, _ = fixtures.prepare(self.config, self.env, self.root / "inventory", invocation=invocation)
            entry = invocation.resource(ownership["resource"])
            self.assertEqual(entry["state"], "sealed")
            self.assertEqual(entry["identity"]["incarnation"], "16384")
            self.assertEqual(entry["identity"]["server"], "external")
            self.assertNotIn("private", json.dumps(invocation.inventory))
            fixtures.teardown(self.config, self.env, ownership, self.root / "inventory", invocation=invocation)
        self.assertEqual(invocation.resource(ownership["resource"])["state"], "removed")

    def test_postgres_production_parent_is_refused_before_any_client_or_capacity_probe(self):
        for database in ("production", "postgres", "", "production?dbname=conveyor_test"):
            with self.subTest(database=database), \
                 patch.object(fixtures, "_run") as client, \
                 patch.object(fixtures, "_check_capacity") as capacity:
                unsafe = {"ROOT_URL": "postgres://admin:private@foreign.example:5432/" + database}
                with self.assertRaisesRegex(fixtures.FixtureError, "must end in _test"):
                    fixtures.prepare(self.config, unsafe, self.root / ("unsafe-" + str(len(database))))
                client.assert_not_called()
                capacity.assert_not_called()

    def test_before_probe_failure_retains_after_snapshot_before_owned_teardown(self):
        state = self.root / "before-failure"
        original = self.completed
        probes = 0

        def client(argv, env, capture=True):
            nonlocal probes
            if "probe" in argv:
                probes += 1
                if probes == 1:
                    return subprocess.CompletedProcess(argv, 2, "", "connection refused")
            return original(argv, env, capture)

        with patch.dict(os.environ, self.env), \
             patch.object(fixtures, "_run", side_effect=client), \
             patch.object(resources, "start_process") as gate:
            self.assertEqual(fixtures.run_lifecycle(self.config, state, ["make", "gate"]), 2)
            gate.assert_not_called()
        phases = [json.loads(line) for line in (state / "phases.jsonl").read_text().splitlines()]
        self.assertEqual([(p["phase"], p["outcome"]) for p in phases if p["outcome"] in
                          ("started", "not-run")], [
            ("prepare", "started"), ("before-snapshot", "started"), ("gate", "not-run"),
            ("after-snapshot", "started"), ("teardown", "started"),
        ])
        self.assertEqual(json.loads((state / "ownership.json").read_text())["state"], "released")
        self.assertTrue(json.loads((state / "after-snapshot.json").read_text())["instance"].endswith("_test"))
        self.assertNotIn("private", (state / "phases.jsonl").read_text())

    def test_preparation_failures_include_client_timeout_and_safe_actionable_detail(self):
        for kind in ("client", "network", "capacity"):
            with self.subTest(kind=kind):
                if kind == "client":
                    failure = patch.object(fixtures, "_run", side_effect=FileNotFoundError("go executable unavailable"))
                elif kind == "network":
                    failure = patch.object(fixtures, "_run", return_value=subprocess.CompletedProcess([], 2, "", "connection refused"))
                else:
                    failure = patch.object(fixtures, "_check_capacity", side_effect=fixtures.FixtureError("capacity probe failed: 1 bytes free, 20 required"))
                with failure, self.assertRaises(fixtures.FixtureError) as caught:
                    fixtures.prepare(self.config, self.env, self.root / kind)
                diagnostic = str(caught.exception)
                self.assertIn("client=repository-go-driver", diagnostic)
                self.assertIn("tool=go-run", diagnostic)
                self.assertIn("timeout=1s", diagnostic)
                self.assertNotIn("private", diagnostic)
                self.assertIn({"client": "go executable unavailable", "network": "connection refused",
                               "capacity": "1 bytes free, 20 required"}[kind], diagnostic)

    def test_run_action_parses_options_before_reaching_fixture_preparation(self):
        lifecycle = Mock(return_value=7)
        state = self.root / "direct-make-entrypoint"
        with patch.object(fixtures, "run_lifecycle", lifecycle):
            status = fixtures.main([
                "run", "--backend", "postgres", "--url-env", "ROOT_URL",
                "--prepared-url-env", "CONVEYOR_TEST_DATABASE_URL",
                "--state", str(state), "--", "make", "_test-integration-postgres",
            ])
        self.assertEqual(status, 7)
        config, parsed_state, command = lifecycle.call_args.args
        self.assertEqual(config["backend"], "postgres")
        self.assertEqual(parsed_state, state.resolve())
        self.assertEqual(command, ["make", "_test-integration-postgres"])
        self.assertFalse(lifecycle.call_args.kwargs["managed_postgres"])

    def test_postgres_target_owns_its_container_through_one_managed_invocation(self):
        makefile = (fixtures.ROOT / "Makefile").read_text()
        target = makefile.split("test-integration: compose-check test-validation-docker vk10-runtime", 1)[1].split(
            "test-integration-ci:", 1
        )[0]
        prepared, standalone = target.split("else", 1)
        self.assertIn("_test-integration-postgres", prepared)
        self.assertNotIn("validation_fixtures.py", prepared)
        self.assertIn("--managed-postgres", standalone)
        self.assertIn("CONVEYOR_FIXTURE_STATE", standalone)
        for retired in ("test-db-up", "test-db-down", "trap", "docker compose"):
            self.assertNotIn(retired, target)

    def test_database_targets_mutate_only_a_named_invocation(self):
        makefile = (fixtures.ROOT / "Makefile").read_text()
        down = makefile.split("test-db-down:", 1)[1].split("\n\n", 1)[0]
        self.assertIn('validation_resources.py recover --invocation "$(INVOCATION)"', down)
        self.assertIn("INVOCATION=", down)
        for forbidden in ("docker compose", "--remove-orphans", "prune"):
            self.assertNotIn(forbidden, down)
        up = makefile.split("test-db-up:", 1)[1].split("\n\n", 1)[0]
        self.assertIn("validation_fixtures.py up", up)

    def test_identity_reports_pins_and_recorded_containers(self):
        self.assertTrue(fixtures.database_identity(None, {}).startswith("auto\tconveyor-test-<invocation-id>"))
        self.assertTrue(fixtures.database_identity(None, {"TEST_POSTGRES_PORT": "25432"}).startswith("25432\t"))
        invocation = resources.Invocation.create("fixture-task", fixtures.ROOT, ["test"], tmp=False)
        rid = invocation.register("container", {"project": "conveyor-test-x", "port": 25433}, role="postgres")
        invocation.seal(rid, {"id": "f" * 64})
        invocation.finish(detached=True)
        self.assertEqual(fixtures.database_identity(str(invocation.path), {}),
                         "25433\tconveyor-test-x\t" + "f" * 64)


if __name__ == "__main__":
    unittest.main()
