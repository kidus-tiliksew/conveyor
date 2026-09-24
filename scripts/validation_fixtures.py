#!/usr/bin/env python3
"""Prepare, probe, run, and tear down owned validation databases safely."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import time
from urllib.parse import urlsplit, urlunsplit


ROOT = Path(__file__).resolve().parent.parent
HELPER = "./scripts/validation-fixture-sql"
SAFE_DATABASE = re.compile(r"^[a-z][a-z0-9_]{2,62}_test$")
DEFAULT_MINIMUM_BYTES = 1024 * 1024 * 1024
SINGLESTORE_MINIMUM_BYTES = 5 * 1024 * 1024 * 1024


class FixtureError(RuntimeError):
    pass


def _safe_endpoint(backend: str, dsn: str) -> str:
    if backend == "postgres":
        parsed = urlsplit(dsn)
        if parsed.scheme not in ("postgres", "postgresql") or not parsed.hostname:
            raise FixtureError("invalid PostgreSQL URL in configured environment variable")
        return f"{parsed.hostname}:{parsed.port or 5432}"
    match = re.search(r"@tcp\(([^)]+)\)", dsn)
    if not match:
        raise FixtureError("invalid SingleStore DSN in configured environment variable")
    return match.group(1)


def _database_from_dsn(backend: str, dsn: str) -> str:
    if backend == "postgres":
        return urlsplit(dsn).path.removeprefix("/")
    tail = dsn.rsplit("/", 1)
    return tail[1].split("?", 1)[0] if len(tail) == 2 else ""


def _replace_database(backend: str, dsn: str, database: str) -> str:
    if backend == "postgres":
        parsed = urlsplit(dsn)
        return urlunsplit((parsed.scheme, parsed.netloc, "/" + database, parsed.query, parsed.fragment))
    prefix, separator, tail = dsn.rpartition("/")
    if not separator:
        raise FixtureError("invalid SingleStore DSN in configured environment variable")
    query = "?" + tail.split("?", 1)[1] if "?" in tail else ""
    return prefix + "/" + database + query


def _run(argv: list[str], env: dict[str, str], *, capture: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(argv, cwd=ROOT, env=env, check=False, text=True,
                          stdout=subprocess.PIPE if capture else None,
                          stderr=subprocess.PIPE if capture else None)


def _diagnostic(backend: str, operation: str, endpoint: str, detail: str) -> FixtureError:
    lines = [line.strip() for line in detail.splitlines() if line.strip()]
    actionable = [line for line in lines if not re.fullmatch(r"exit status [0-9]+", line)]
    safe = (actionable or lines or ["no detail returned"])[-1]
    if "://" in safe or "password=" in safe.lower() or "@tcp(" in safe.lower():
        safe = "protected client detail was redacted"
    else:
        safe = re.sub(r"(?i)\b(user|username)=\S+", r"\1=[redacted]", safe)
    return FixtureError(f"{backend} {operation} failed for {endpoint}: {safe}")


def _phase(state: Path, phase: str, outcome: str, detail: str = "") -> None:
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    record = {"phase": phase, "outcome": outcome, "at": time.time(), "detail": detail}
    path = state / "phases.jsonl"
    with path.open("a", encoding="utf-8") as target:
        os.chmod(path, 0o600)
        target.write(json.dumps(record, sort_keys=True) + "\n")
        target.flush()
        os.fsync(target.fileno())


def _write_owner(path: Path, ownership: dict, *, create: bool = False) -> None:
    target = path if create else path.with_name(".ownership." + secrets.token_hex(4) + ".tmp")
    mode = "x" if create else "w"
    with target.open(mode, encoding="utf-8") as output:
        os.chmod(target, 0o600)
        json.dump(ownership, output, sort_keys=True)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())
    if not create:
        os.replace(target, path)


def _check_capacity(state: Path, minimum_bytes: int, backend: str) -> int:
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    free = shutil.disk_usage(state).free
    if free < minimum_bytes:
        raise FixtureError(
            f"{backend} capacity probe failed: {free} bytes free, {minimum_bytes} required; "
            "free task-owned caches or select a host with sufficient disk"
        )
    return free


def _check_external_network(name: str | None, env: dict[str, str]) -> None:
    if not name:
        return
    result = _run(["docker", "network", "inspect", name, "--format", "{{.Name}}"], env)
    if result.returncode != 0 or result.stdout.strip() != name:
        raise FixtureError(
            f"external Docker network {name!r} is unavailable; create or configure it explicitly. "
            "Validation will not create, prune, or delete shared networks"
        )


def prepare(config: dict, base_env: dict[str, str], state: Path) -> tuple[dict, dict[str, str]]:
    backend = config["backend"]
    url_env = config["url_env"]
    prepared_env = config["prepared_url_env"]
    dsn = base_env.get(url_env, "")
    if not dsn:
        _phase(state, "prepare", "configuration-failure", f"{url_env} is unset")
        raise FixtureError(f"{backend} required configuration {url_env} is unset; coverage was not run")
    endpoint = _safe_endpoint(backend, dsn)
    base_database = _database_from_dsn(backend, dsn)
    if backend == "singlestore" and not base_database.endswith("_test"):
        _phase(state, "prepare", "safety-refusal", "configured database is not a _test parent")
        raise FixtureError("SingleStore configured database must end in _test; refusing a production-shaped endpoint")
    configured_minimum = int(config.get("minimum_free_bytes", 0))
    minimum = configured_minimum or (
        SINGLESTORE_MINIMUM_BYTES if backend == "singlestore" else DEFAULT_MINIMUM_BYTES
    )
    free = _check_capacity(state, minimum, backend)
    _check_external_network(base_env.get(config.get("external_network_env", "")), base_env)
    token = secrets.token_hex(6)
    prefix = re.sub(r"[^a-z0-9]+", "_", config.get("database_prefix", "conveyor").lower()).strip("_")
    database = f"{prefix}_{token}_test"[:63]
    if not SAFE_DATABASE.fullmatch(database):
        raise FixtureError("generated owned database name failed the safety policy")
    helper_env = dict(base_env)
    helper_env["CONVEYOR_FIXTURE_ADMIN_DSN"] = dsn
    prepared = _replace_database(backend, dsn, database)
    ownership = {
        "schema": 1, "backend": backend, "database": database, "token": token,
        "endpoint": endpoint, "url_env": url_env, "prepared_url_env": prepared_env,
        "url_sha256": hashlib.sha256(prepared.encode()).hexdigest(),
        "external_network": base_env.get(config.get("external_network_env", "")) or None,
        "free_bytes_at_prepare": free, "created_at": time.time(), "state": "preparing",
    }
    owner = state / "ownership.json"
    _write_owner(owner, ownership, create=True)
    argv = ["go", "run", HELPER, "--backend", backend, "--action", "create",
            "--dsn-env", "CONVEYOR_FIXTURE_ADMIN_DSN", "--database", database,
            "--timeout", config.get("timeout", "20s")]
    result = _run(argv, helper_env)
    if result.returncode:
        ownership["state"] = "creation-failed"
        _write_owner(owner, ownership)
        _phase(state, "prepare", "creation-failure", f"endpoint={endpoint} client=repository-go-driver free_bytes={free}")
        raise _diagnostic(backend, "database creation", endpoint, result.stderr)
    ownership["state"] = "owned"
    try:
        _write_owner(owner, ownership)
    except OSError:
        # The database was created but durable ownership could not be sealed.
        # Best-effort rollback uses only the in-memory generated safe name.
        drop = ["go", "run", HELPER, "--backend", backend,
                "--action", "drop", "--dsn-env", "CONVEYOR_FIXTURE_ADMIN_DSN",
                "--database", database, "--timeout", config.get("timeout", "20s")]
        _run(drop, helper_env)
        raise
    _phase(state, "prepare", "success", f"backend={backend} endpoint={endpoint} database={database} client=repository-go-driver free_bytes={free}")
    child_env = dict(base_env)
    child_env[prepared_env] = prepared
    child_env["CONVEYOR_FIXTURE_OWNERSHIP"] = str(owner)
    child_env["CONVEYOR_FIXTURE_PREPARED"] = "1"
    return ownership, child_env


def probe(config: dict, env: dict[str, str], ownership: dict, state: Path, label: str) -> dict:
    backend = ownership["backend"]
    prepared_env = ownership["prepared_url_env"]
    argv = ["go", "run", HELPER, "--backend", backend, "--action", "probe",
            "--dsn-env", prepared_env, "--timeout", config.get("timeout", "20s")]
    result = _run(argv, env)
    if result.returncode:
        _phase(state, label, "snapshot-failure", f"backend={backend} endpoint={ownership['endpoint']}")
        raise _diagnostic(backend, label + " snapshot", ownership["endpoint"], result.stderr)
    try:
        value = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise FixtureError(f"{backend} {label} snapshot returned invalid JSON") from exc
    _phase(state, label, "success", f"backend={backend} instance={ownership['database']}")
    return value


def teardown(config: dict, base_env: dict[str, str], ownership: dict, state: Path) -> None:
    owner_path = state / "ownership.json"
    try:
        current = json.loads(owner_path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise FixtureError("owned fixture teardown refused: ownership record is missing or corrupt") from exc
    if current != ownership or current.get("state") != "owned" or not SAFE_DATABASE.fullmatch(current.get("database", "")):
        raise FixtureError("owned fixture teardown refused: ownership identity changed or is unsafe")
    argv = ["go", "run", HELPER, "--backend", ownership["backend"], "--action", "drop",
            "--dsn-env", "CONVEYOR_FIXTURE_ADMIN_DSN", "--database", ownership["database"],
            "--timeout", config.get("timeout", "20s")]
    env = dict(base_env)
    env["CONVEYOR_FIXTURE_ADMIN_DSN"] = base_env[ownership["url_env"]]
    result = _run(argv, env)
    if result.returncode:
        _phase(state, "teardown", "failure", f"backend={ownership['backend']} database={ownership['database']}")
        raise _diagnostic(ownership["backend"], "owned teardown", ownership["endpoint"], result.stderr)
    current["state"] = "released"
    current["released_at"] = time.time()
    _write_owner(owner_path, current)
    _phase(state, "teardown", "success", f"backend={ownership['backend']} database={ownership['database']}")


def _terminate_process_group(process) -> None:
    if process.poll() is not None:
        process.wait()
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
    else:
        # Reap descendants that kept the process group after the direct child
        # handled TERM and exited.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def run_lifecycle(config: dict, state: Path, command: list[str]) -> int:
    base_env = dict(os.environ)
    ownership, child_env = prepare(config, base_env, state)
    status = 2
    process = None
    interrupted = None
    previous = {}

    def handle(signum, _frame):
        nonlocal interrupted
        interrupted = signum
        if process is not None and process.poll() is None:
            _terminate_process_group(process)

    for signum in (signal.SIGINT, signal.SIGTERM):
        previous[signum] = signal.signal(signum, handle)
    try:
        probe(config, child_env, ownership, state, "before-snapshot")
        _phase(state, "gate", "started", "argv begins with " + command[0])
        process = subprocess.Popen(command, cwd=ROOT, env=child_env, start_new_session=True)
        status = process.wait()
        _phase(state, "gate", "success" if status == 0 else "failure", f"exit_status={status}")
        try:
            probe(config, child_env, ownership, state, "after-snapshot")
        except FixtureError as exc:
            print(str(exc), file=sys.stderr)
            status = 2
        if interrupted is not None:
            status = 128 + interrupted
    finally:
        if process is not None and process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
        try:
            teardown(config, base_env, ownership, state)
        except FixtureError as exc:
            print(str(exc), file=sys.stderr)
            if status == 0:
                status = 2
        for signum, handler in previous.items():
            signal.signal(signum, handler)
    return status


def config_from_args(args) -> dict:
    return {
        "backend": args.backend,
        "url_env": args.url_env,
        "prepared_url_env": args.prepared_url_env,
        "external_network_env": args.external_network_env,
        "database_prefix": args.database_prefix,
        "minimum_free_bytes": args.minimum_free_bytes,
        "timeout": args.timeout,
    }


def parse_args(argv: list[str] | None = None):
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_subparsers(dest="action", required=True)
    run = actions.add_parser("run", help="prepare a fixture, run a gate, and tear the fixture down")
    run.add_argument("--backend", choices=("postgres", "singlestore"), required=True)
    run.add_argument("--url-env", required=True)
    run.add_argument("--prepared-url-env", required=True)
    run.add_argument("--state", type=Path, required=True)
    run.add_argument("--external-network-env", default="CONVEYOR_TEST_EXTERNAL_NETWORK")
    run.add_argument("--database-prefix", default="conveyor")
    run.add_argument("--minimum-free-bytes", type=int, default=0)
    run.add_argument("--timeout", default="20s")
    run.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    if args.command[:1] == ["--"]:
        args.command = args.command[1:]
    if not args.command:
        run.error("run requires a command after --")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        return run_lifecycle(config_from_args(args), args.state.resolve(), args.command)
    except (FixtureError, OSError, ValueError) as exc:
        print(f"validation fixture lifecycle failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
