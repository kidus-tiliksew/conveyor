#!/usr/bin/env python3
"""Fail if the default Compose topology can address development PostgreSQL."""

import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parent.parent
TEST_PORT = "55433"


def render(compose_file: Path, project_directory: Path) -> dict:
    env = os.environ.copy()
    env["CONVEYOR_TEST_POSTGRES_PORT"] = TEST_PORT
    result = subprocess.run(
        [
            "docker",
            "compose",
            "-f",
            str(compose_file),
            "--project-directory",
            str(project_directory),
            "--profile",
            "test",
            "config",
            "--format",
            "json",
        ],
        check=True,
        capture_output=True,
        env=env,
        text=True,
    )
    return json.loads(result.stdout)


def fail(message: str) -> None:
    print(f"compose isolation check failed: {message}", file=sys.stderr)
    raise SystemExit(1)


def check_test_topology(config: dict, context: str) -> None:
    if config.get("name") != "conveyor-test":
        fail(f"{context}: project name is {config.get('name')!r}, want 'conveyor-test'")

    services = config.get("services", {})
    if set(services) != {"postgres-test"}:
        fail(f"{context}: services are {sorted(services)}, want only postgres-test")

    if config.get("volumes"):
        fail(f"{context}: default topology declares persistent volumes")

    rendered = json.dumps(config, sort_keys=True)
    for forbidden in ("conveyor-postgres", "conveyor-postgres-data"):
        if forbidden in rendered:
            fail(f"{context}: rendered config contains {forbidden!r}")

    postgres_test = services["postgres-test"]
    if "/var/lib/postgresql/data" not in postgres_test.get("tmpfs", []):
        fail(f"{context}: postgres-test data is not tmpfs-backed")

    published_ports = {
        str(port.get("published")) for port in postgres_test.get("ports", [])
    }
    if TEST_PORT not in published_ports:
        fail(f"{context}: CONVEYOR_TEST_POSTGRES_PORT override was not rendered")


def check_dev_topology(config: dict) -> None:
    if config.get("name") != "conveyor":
        fail(f"dev: project name is {config.get('name')!r}, want 'conveyor'")

    services = config.get("services", {})
    if set(services) != {"postgres"}:
        fail(f"dev: services are {sorted(services)}, want only postgres")
    if services["postgres"].get("container_name") != "conveyor-postgres":
        fail("dev: persistent database container identity changed")

    volumes = config.get("volumes", {})
    if volumes.get("postgres-data", {}).get("name") != "conveyor-postgres-data":
        fail("dev: persistent database volume identity changed")


def main() -> None:
    check_test_topology(render(ROOT / "compose.yaml", ROOT), "repository")
    check_test_topology(
        render(ROOT / "compose.yaml", ROOT.parent), "alternate worktree path"
    )
    check_dev_topology(render(ROOT / "compose.dev.yaml", ROOT))
    print("compose isolation check passed")


if __name__ == "__main__":
    main()
