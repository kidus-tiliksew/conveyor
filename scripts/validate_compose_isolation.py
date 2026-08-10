#!/usr/bin/env python3
"""Fail if the default Compose topology can address development PostgreSQL."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parent.parent
TEST_PORT = "55433"


def render(compose_file: Path, project_directory: Path, port: str, project: str) -> dict:
    env = os.environ.copy()
    env["CONVEYOR_TEST_POSTGRES_PORT"] = port
    result = subprocess.run(
        [
            "docker",
            "compose",
            "-p",
            project,
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


def make_command(
    project_directory: Path,
    targets: list[str],
    env_overrides: dict[str, str] | None = None,
    dry_run: bool = False,
) -> str:
    env = os.environ.copy()
    env.pop("CONVEYOR_TEST_POSTGRES_PORT", None)
    env.pop("TEST_POSTGRES_PORT", None)
    env.update(env_overrides or {})
    makefile = project_directory / "Makefile"
    if project_directory != ROOT:
        shutil.copyfile(ROOT / "Makefile", makefile)
    args = ["make", "--no-print-directory", "-s"]
    if dry_run:
        args.append("-n")
    args.extend(["-C", str(project_directory), "-f", str(makefile), *targets])
    result = subprocess.run(
        args,
        check=True,
        capture_output=True,
        env=env,
        text=True,
    )
    return result.stdout.strip()


def make_identity(
    project_directory: Path, env_overrides: dict[str, str] | None = None
) -> tuple[str, str]:
    port, project = make_command(
        project_directory, ["test-db-identity"], env_overrides
    ).split("\t")
    return port, project


def fail(message: str) -> None:
    print(f"compose isolation check failed: {message}", file=sys.stderr)
    raise SystemExit(1)


def check_test_topology(config: dict, context: str, port: str, project: str) -> None:
    if config.get("name") != project:
        fail(f"{context}: project name is {config.get('name')!r}, want {project!r}")

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
    if port not in published_ports:
        fail(f"{context}: port {port!r} was not rendered")


def check_identity(path: Path, port: str, project: str) -> None:
    numeric_port = int(port)
    if not 20000 <= numeric_port <= 29999:
        fail(f"{path}: derived port {port!r} is outside 20000-29999")
    if project != f"conveyor-test-p{port}":
        fail(f"{path}: project {project!r} does not match derived port {port}")

    check_lifecycle(path, port, project)


def check_lifecycle(
    path: Path,
    port: str,
    project: str,
    env_overrides: dict[str, str] | None = None,
) -> None:

    lifecycle = make_command(
        path,
        ["test-db-up", "test-db-down"],
        env_overrides,
        dry_run=True,
    ).splitlines()
    expected_scope = f"docker compose -p {project} --profile test"
    if len(lifecycle) != 2 or any(expected_scope not in line for line in lifecycle):
        fail(f"{path}: database lifecycle is not scoped to {project!r}")
    expected_port = f"CONVEYOR_TEST_POSTGRES_PORT={port}"
    if any(expected_port not in line for line in lifecycle):
        fail(f"{path}: database lifecycle does not consistently use port {port}")


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
    with tempfile.TemporaryDirectory(prefix="conveyor-compose-a-") as first_dir:
        with tempfile.TemporaryDirectory(prefix="conveyor-compose-b-") as second_dir:
            first_path = Path(first_dir).resolve()
            second_path = Path(second_dir).resolve()
            first = make_identity(first_path)
            second = make_identity(second_path)
            check_identity(first_path, *first)
            check_identity(second_path, *second)
            if first == second:
                fail("two distinct canonical worktree paths derived the same identity")
            check_test_topology(
                render(ROOT / "compose.yaml", first_path, *first),
                "first worktree",
                *first,
            )
            check_test_topology(
                render(ROOT / "compose.yaml", second_path, *second),
                "second worktree",
                *second,
            )

    for variable in ("CONVEYOR_TEST_POSTGRES_PORT", "TEST_POSTGRES_PORT"):
        identity = make_identity(ROOT, {variable: TEST_PORT})
        expected = (TEST_PORT, f"conveyor-test-p{TEST_PORT}")
        if identity != expected:
            fail(f"{variable}: identity is {identity!r}, want {expected!r}")
        check_lifecycle(ROOT, *identity, {variable: TEST_PORT})
        check_test_topology(
            render(ROOT / "compose.yaml", ROOT, *identity), variable, *identity
        )

    check_dev_topology(render(ROOT / "compose.dev.yaml", ROOT, TEST_PORT, "conveyor"))
    print("compose isolation check passed")


if __name__ == "__main__":
    main()
