#!/usr/bin/env python3
"""Fail if the test Compose topology could share, adopt, or broadly remove resources.

Checks (component-verification-strategy, "Validation resource ownership and
recovery"): the default topology cannot address development PostgreSQL;
each managed invocation renders its own project, labels, and port; the
container has finite memory and tmpfs budgets that match the helper defaults;
and Make recipes never use `compose down`, `--remove-orphans`, or prune.
"""

import json
import os
from pathlib import Path
import re
import subprocess
import sys

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
import validation_resources as resources  # noqa: E402


ROOT = Path(__file__).resolve().parent.parent
TEST_PORT = "55433"
INVOCATIONS = ("20261001t000000z-aaaaaaaaaaaa", "20261001t000000z-bbbbbbbbbbbb")


def render(compose_file: Path, project: str, overrides: dict[str, str] | None = None) -> dict:
    env = os.environ.copy()
    for name in ("CONVEYOR_TEST_POSTGRES_MEMORY", "CONVEYOR_TEST_POSTGRES_TMPFS_SIZE",
                 "CONVEYOR_TEST_NETWORK_NAME", "CONVEYOR_TEST_NETWORK_EXTERNAL"):
        env.pop(name, None)
    env.update(overrides or {})
    result = subprocess.run(
        ["docker", "compose", "-p", project, "-f", str(compose_file), "--project-directory", str(ROOT),
         "--profile", "test", "config", "--format", "json"],
        check=True, capture_output=True, env=env, text=True,
    )
    return json.loads(result.stdout)


def invocation_env(invocation: str, port: str) -> dict[str, str]:
    project = "conveyor-test-" + invocation
    return {
        "CONVEYOR_TEST_POSTGRES_PORT": port,
        "CONVEYOR_TEST_NETWORK_NAME": project + "_default",
        "CONVEYOR_TEST_NETWORK_EXTERNAL": "false",
        "CONVEYOR_VALIDATION_INVOCATION_ID": invocation,
        "CONVEYOR_VALIDATION_TASK_LABEL": "compose-check",
    }


def make(targets: list[str], overrides: dict[str, str] | None = None, dry_run: bool = False,
         check: bool = True) -> subprocess.CompletedProcess:
    env = os.environ.copy()
    for name in ("CONVEYOR_TEST_POSTGRES_PORT", "TEST_POSTGRES_PORT", "INVOCATION"):
        env.pop(name, None)
    env.update(overrides or {})
    args = ["make", "--no-print-directory", "-s", *(["-n"] if dry_run else []), "-C", str(ROOT), *targets]
    return subprocess.run(args, check=check, capture_output=True, env=env, text=True)


def fail(message: str) -> None:
    print(f"compose isolation check failed: {message}", file=sys.stderr)
    raise SystemExit(1)


def tmpfs_size(service: dict, context: str) -> str:
    for entry in service.get("tmpfs", []):
        path, _, options = entry.partition(":")
        if path == "/var/lib/postgresql/data":
            match = re.search(r"(?:^|,)size=([^,]+)", options)
            if not match:
                fail(f"{context}: postgres-test data tmpfs has no size bound")
            return match.group(1)
    fail(f"{context}: postgres-test data is not tmpfs-backed")
    return ""


def check_test_topology(config: dict, context: str, invocation: str, port: str,
                        memory: str, tmpfs: str) -> None:
    project = "conveyor-test-" + invocation
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
    service = services["postgres-test"]
    labels = service.get("labels", {})
    if labels.get(resources.LABEL_INVOCATION) != invocation:
        fail(f"{context}: container does not carry the invocation label")
    network = config.get("networks", {}).get("default", {})
    if network.get("name") != project + "_default" or network.get("labels", {}).get(resources.LABEL_INVOCATION) != invocation:
        fail(f"{context}: network is not scoped and labelled for the invocation")
    if str(service.get("mem_limit")) != str(resources.parse_size(memory, "memory")):
        fail(f"{context}: mem_limit is {service.get('mem_limit')!r}, want finite {memory}")
    if tmpfs_size(service, context) != tmpfs:
        fail(f"{context}: data tmpfs size is not {tmpfs}")
    published = {str(entry.get("published")) for entry in service.get("ports", [])}
    if port not in published:
        fail(f"{context}: port {port!r} was not rendered")


def check_make_lifecycle() -> None:
    text = (ROOT / "Makefile").read_text()
    for target in re.findall(r"^(test[\w-]*|_test[\w-]*):", text, flags=re.M):
        body = text.split("\n" + target + ":", 1)[1].split("\n\n", 1)[0]
        if "--remove-orphans" in body or re.search(r"docker compose[^\n]*\bdown\b", body) or "prune" in body:
            fail(f"Make target {target} uses broad Compose/Docker cleanup")
    up = make(["test-db-up"], dry_run=True).stdout
    if "validation_fixtures.py up" not in up:
        fail("test-db-up does not create a managed invocation")
    down = make(["test-db-down", "INVOCATION=/state/invocations/example"], dry_run=True).stdout
    if 'validation_resources.py recover --invocation "/state/invocations/example"' not in down:
        fail("test-db-down does not recover the named invocation")
    if make(["test-db-down"], check=False).returncode == 0:
        fail("test-db-down accepted a missing INVOCATION reference")
    # Text inspection: a dry run would still execute recipe lines naming $(MAKE).
    integration = text.split("\ntest-integration:", 1)[1].split("\n\n", 1)[0]
    if "--managed-postgres" not in integration or "test-db-up" in integration or "trap" in integration:
        fail("test-integration does not own its PostgreSQL container through the managed invocation")
    auto = make(["test-db-identity"]).stdout.split("\t")
    if auto[:2] != ["auto", "conveyor-test-<invocation-id>"]:
        fail(f"test-db-identity default is {auto!r}")
    for variable in ("CONVEYOR_TEST_POSTGRES_PORT", "TEST_POSTGRES_PORT"):
        pinned = make(["test-db-identity"], {variable: TEST_PORT}).stdout.split("\t")
        if pinned[0] != TEST_PORT:
            fail(f"{variable}: pinned identity is {pinned!r}")


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
    default_memory = resources.DEFAULT_POSTGRES_MEMORY
    default_tmpfs = resources.DEFAULT_POSTGRES_TMPFS
    resources.postgres_budget({})
    first, second = INVOCATIONS
    configs = [render(ROOT / "compose.yaml", "conveyor-test-" + ident, invocation_env(ident, port))
               for ident, port in ((first, "20001"), (second, "20002"))]
    check_test_topology(configs[0], "first invocation", first, "20001", default_memory, default_tmpfs)
    check_test_topology(configs[1], "second invocation", second, "20002", default_memory, default_tmpfs)
    if configs[0]["name"] == configs[1]["name"]:
        fail("two invocations in one checkout rendered the same project")
    overridden = render(ROOT / "compose.yaml", "conveyor-test-" + first,
                        dict(invocation_env(first, TEST_PORT), CONVEYOR_TEST_POSTGRES_MEMORY="3g",
                             CONVEYOR_TEST_POSTGRES_TMPFS_SIZE="1536m"))
    check_test_topology(overridden, "budget override", first, TEST_PORT, "3g", "1536m")
    check_make_lifecycle()
    check_dev_topology(render(ROOT / "compose.dev.yaml", "conveyor", {"CONVEYOR_TEST_POSTGRES_PORT": TEST_PORT}))
    print("compose isolation check passed")


if __name__ == "__main__":
    main()
