"""Deterministic tests for owned validation fixture lifecycle and diagnostics."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

import validation_fixtures as fixtures


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

    def completed(self, argv, env, capture=True):
        self.calls.append((list(argv), dict(env)))
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

    def test_lifecycle_orders_snapshots_around_gate_and_teardown_on_failure(self):
        order = []
        ownership = {"backend": "postgres", "database": "conveyor_a1_test",
                     "prepared_url_env": "CONVEYOR_TEST_DATABASE_URL", "url_env": "ROOT_URL"}
        child_env = dict(self.env, CONVEYOR_TEST_DATABASE_URL=self.env["ROOT_URL"])

        class Process:
            returncode = 7
            def wait(self):
                order.append("gate")
                return 7
            def poll(self):
                return 7

        with patch.object(fixtures, "prepare", side_effect=lambda *a: (order.append("prepare") or (ownership, child_env))), \
             patch.object(fixtures, "probe", side_effect=lambda c, e, o, s, label: order.append(label) or {}), \
             patch.object(fixtures, "teardown", side_effect=lambda *a: order.append("teardown")), \
             patch("subprocess.Popen", return_value=Process()):
            self.assertEqual(fixtures.run_lifecycle(self.config, self.root / "run", ["make", "gate"]), 7)
        self.assertEqual(order, ["prepare", "before-snapshot", "gate", "after-snapshot", "teardown"])

    def test_owned_process_group_is_terminated(self):
        process = subprocess.Popen(["sh", "-c", "sleep 30 & wait"], start_new_session=True)
        self.addCleanup(lambda: process.poll() is None and process.kill())
        fixtures._terminate_process_group(process)
        self.assertIsNotNone(process.poll())

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

    def test_postgres_target_leaves_outer_owned_fixture_reachable_for_after_snapshot(self):
        makefile = (fixtures.ROOT / "Makefile").read_text()
        target = makefile.split("test-integration: compose-check vk10-runtime", 1)[1].split(
            "test-integration-ci:", 1
        )[0]
        self.assertNotIn("test-integration: compose-check vk10-runtime test-db-up", makefile)
        prepared, standalone = target.split("else", 1)
        self.assertIn("_test-integration-postgres", prepared)
        self.assertNotIn("test-db-down", prepared)
        self.assertIn("test-db-up", standalone)
        self.assertIn("test-db-down", standalone)
        self.assertIn("CONVEYOR_FIXTURE_STATE", standalone)

    def test_postgres_teardown_removes_only_the_scoped_project_topology(self):
        makefile = (fixtures.ROOT / "Makefile").read_text()
        target = makefile.split("test-db-down:", 1)[1].split("\n\n", 1)[0]
        self.assertIn("docker compose -p $(TEST_COMPOSE_PROJECT) --profile test down", target)
        self.assertNotIn("prune", target)


if __name__ == "__main__":
    unittest.main()
