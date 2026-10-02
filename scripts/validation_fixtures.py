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

sys.dont_write_bytecode = True
import validation_resources as resources


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


def _diagnostic(backend: str, operation: str, endpoint: str, detail: str, timeout: str = "20s") -> FixtureError:
    lines = [line.strip() for line in detail.splitlines() if line.strip()]
    actionable = [line for line in lines if not re.fullmatch(r"exit status [0-9]+", line)]
    safe = (actionable or lines or ["no detail returned"])[-1]
    if "://" in safe or "password=" in safe.lower() or "@tcp(" in safe.lower():
        safe = "protected client detail was redacted"
    else:
        safe = re.sub(r"(?i)\b(user|username)=\S+", r"\1=[redacted]", safe)
    return FixtureError(f"{backend} {operation} failed for {endpoint} "
                        f"(client=repository-go-driver tool=go-run timeout={timeout}): {safe}")


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


def prepare(config: dict, base_env: dict[str, str], state: Path, *, invocation=None,
            server: dict | None = None) -> tuple[dict, dict[str, str]]:
    """Create the owned database. With an invocation, register and seal it."""
    _phase(state, "prepare", "started")
    endpoint = "configured endpoint"
    try:
        if base_env.get(config["url_env"]):
            endpoint = _safe_endpoint(config["backend"], base_env[config["url_env"]])
        return _prepare(config, base_env, state, invocation, server or {"server": "external"})
    except (FixtureError, OSError, ValueError) as exc:
        error = _diagnostic(config["backend"], "preparation", endpoint,
                            str(exc), config.get("timeout", "20s"))
        _phase(state, "prepare", "failure", str(error))
        raise error from exc


def _prepare(config: dict, base_env: dict[str, str], state: Path, invocation, server: dict) -> tuple[dict, dict[str, str]]:
    backend = config["backend"]
    url_env = config["url_env"]
    prepared_env = config["prepared_url_env"]
    dsn = base_env.get(url_env, "")
    if not dsn:
        _phase(state, "prepare", "configuration-failure", f"{url_env} is unset")
        raise FixtureError(f"{backend} required configuration {url_env} is unset; coverage was not run")
    endpoint = _safe_endpoint(backend, dsn)
    base_database = _database_from_dsn(backend, dsn)
    if not base_database.endswith("_test"):
        _phase(state, "prepare", "safety-refusal", "configured database is not a _test parent")
        raise FixtureError(f"{backend} configured database must end in _test; refusing a production-shaped endpoint")
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
    rid = None
    if invocation is not None:
        # Register intent before creation; the inventory never holds a DSN.
        rid = invocation.register("database", dict(server, backend=backend, endpoint=endpoint, database=database,
                                                   url_env=url_env), role="fixture-database")
        ownership["invocation"] = str(invocation.path)
        ownership["resource"] = rid
    owner = state / "ownership.json"
    _write_owner(owner, ownership, create=True)
    argv = ["go", "run", HELPER, "--backend", backend, "--action", "create",
            "--dsn-env", "CONVEYOR_FIXTURE_ADMIN_DSN", "--database", database,
            "--timeout", config.get("timeout", "20s")]
    result = _run(argv, helper_env)
    if result.returncode:
        ownership["state"] = "creation-failed"
        _write_owner(owner, ownership)
        if rid is not None:
            invocation.mark(rid, "ambiguous", "database creation failed; existence is unverified")
        _phase(state, "prepare", "creation-failure", f"endpoint={endpoint} client=repository-go-driver free_bytes={free}")
        raise _diagnostic(backend, "database creation", endpoint, result.stderr, config.get("timeout", "20s"))
    ownership["state"] = "owned"
    try:
        _write_owner(owner, ownership)
        if rid is not None:
            invocation.seal(rid, {"incarnation": _incarnation(backend, helper_env, database, config)})
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


def _incarnation(backend: str, helper_env: dict[str, str], database: str, config: dict) -> str | None:
    # PostgreSQL's database OID identifies this incarnation for recovery. A
    # backend without one leaves an orphaned database for operator handling.
    if backend != "postgres":
        return None
    result = _run(["go", "run", HELPER, "--backend", backend, "--action", "incarnation",
                   "--dsn-env", "CONVEYOR_FIXTURE_ADMIN_DSN", "--database", database,
                   "--timeout", config.get("timeout", "20s")], helper_env)
    try:
        return json.loads(result.stdout).get("incarnation") if result.returncode == 0 else None
    except (ValueError, AttributeError):
        return None


def probe(config: dict, env: dict[str, str], ownership: dict, state: Path, label: str) -> dict:
    _phase(state, label, "started")
    try:
        return _probe(config, env, ownership, state, label)
    except (FixtureError, OSError, ValueError) as exc:
        error = _diagnostic(ownership["backend"], label, ownership["endpoint"],
                            str(exc), config.get("timeout", "20s"))
        _phase(state, label, "failure", str(error))
        raise error from exc


def _probe(config: dict, env: dict[str, str], ownership: dict, state: Path, label: str) -> dict:
    backend = ownership["backend"]
    prepared_env = ownership["prepared_url_env"]
    argv = ["go", "run", HELPER, "--backend", backend, "--action", "probe",
            "--dsn-env", prepared_env, "--timeout", config.get("timeout", "20s")]
    result = _run(argv, env)
    if result.returncode:
        _phase(state, label, "snapshot-failure", f"backend={backend} endpoint={ownership['endpoint']}")
        raise _diagnostic(backend, label + " snapshot", ownership["endpoint"], result.stderr, config.get("timeout", "20s"))
    try:
        value = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise FixtureError(f"{backend} {label} snapshot returned invalid JSON") from exc
    # The maintained SQL helper returns only non-secret backend metadata.
    _write_owner(state / (label + ".json"), value)
    _phase(state, label, "success", f"backend={backend} instance={ownership['database']}")
    return value


def teardown(config: dict, base_env: dict[str, str], ownership: dict, state: Path, *, invocation=None) -> None:
    _phase(state, "teardown", "started")
    try:
        _teardown(config, base_env, ownership, state)
        if invocation is not None and ownership.get("resource"):
            invocation.mark(ownership["resource"], "removed", "owned fixture teardown dropped the database")
    except (FixtureError, OSError, ValueError) as exc:
        error = _diagnostic(ownership["backend"], "owned teardown", ownership["endpoint"],
                            str(exc), config.get("timeout", "20s"))
        _phase(state, "teardown", "failure", str(error))
        raise error from exc


def _teardown(config: dict, base_env: dict[str, str], ownership: dict, state: Path) -> None:
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
        raise _diagnostic(ownership["backend"], "owned teardown", ownership["endpoint"], result.stderr, config.get("timeout", "20s"))
    current["state"] = "released"
    current["released_at"] = time.time()
    _write_owner(owner_path, current)
    _phase(state, "teardown", "success", f"backend={ownership['backend']} database={ownership['database']}")


def _invocation_argv(config: dict, command: list[str]) -> list[str]:
    return ["validation_fixtures.py", "run", "--backend", config["backend"], "--", *command]


def run_lifecycle(config: dict, state: Path, command: list[str], *, task: str = "manual-validation",
                  managed_postgres: bool = False, gate_timeout: float = 0) -> int:
    base_env = dict(os.environ)
    invocation = resources.Invocation.enter(task, ROOT, _invocation_argv(config, command),
                                            {"backend": config["backend"], "managed_postgres": managed_postgres})
    status = 2
    supervised = None
    interrupted = None
    ownership = None
    child_env = None
    previous = {}

    def handle(signum, _frame):
        # Only record the signal; the main flow stops the sealed group so no
        # inventory mutation runs inside a signal handler.
        nonlocal interrupted
        interrupted = signum

    for signum in (signal.SIGINT, signal.SIGTERM):
        previous[signum] = signal.signal(signum, handle)
    gate_started = False
    try:
        try:
            server = {"server": "external"}
            if managed_postgres:
                _phase(state, "managed-container", "started")
                managed = resources.start_managed_postgres(
                    invocation, ROOT, base_env, external_network=base_env.get(config.get("external_network_env", "")))
                base_env[config["url_env"]] = managed["url"]
                server = {"server": "invocation-container", "container": managed["container"]}
                _phase(state, "managed-container", "success",
                       f"project={managed['project']} port={managed['port']} memory={managed['budget']['memory']} "
                       f"tmpfs={managed['budget']['tmpfs']}")
            ownership, child_env = prepare(config, base_env, state, invocation=invocation, server=server)
        except (resources.ResourceError, OSError) as exc:
            _phase(state, "prepare", "failure", str(exc))
            raise FixtureError(str(exc)) from exc
        child_env.update(invocation.managed_env())
        probe(config, child_env, ownership, state, "before-snapshot")
        if interrupted is None:
            _phase(state, "gate", "started", "argv begins with " + command[0])
            gate_started = True
            supervised = resources.start_process(invocation, command, env=child_env, cwd=ROOT, role="gate")
            deadline = time.monotonic() + gate_timeout if gate_timeout else None
            outcome = None
            while outcome is None:
                try:
                    status = supervised.process.wait(timeout=0.1)
                    outcome = "success" if status == 0 else "failure"
                except subprocess.TimeoutExpired:
                    if interrupted is not None:
                        outcome = "interrupted"
                    elif deadline is not None and time.monotonic() >= deadline:
                        outcome = "timeout"
                        status = 124
            stopped, detail = supervised.stop()
            if outcome in ("interrupted", "timeout") and supervised.process.returncode is not None:
                status = status if outcome == "timeout" else supervised.process.returncode
            _phase(state, "gate", outcome, f"exit_status={status}")
            if not stopped:
                _phase(state, "gate-processes", "cleanup-failure", detail)
    except (FixtureError, OSError, ValueError) as exc:
        print(str(exc), file=sys.stderr)
        if gate_started:
            _phase(state, "gate", "failure", "gate could not complete")
    finally:
        if supervised is not None and supervised.process.poll() is None:
            supervised.stop()
        if not gate_started:
            _phase(state, "gate", "not-run", "fixture preparation or before snapshot failed, or execution was interrupted")
        if ownership is not None:
            # The owned fixture is still reachable even when the before probe failed.
            try:
                probe(config, child_env, ownership, state, "after-snapshot")
            except (FixtureError, OSError, ValueError) as exc:
                print(str(exc), file=sys.stderr)
                status = 2
            try:
                teardown(config, base_env, ownership, state, invocation=invocation)
            except (FixtureError, OSError, ValueError) as exc:
                print(str(exc), file=sys.stderr)
                if status == 0:
                    status = 2
        # Remaining sealed resources (the managed container and network, the
        # temporary child) are removed by exact identity. A cleanup failure
        # fails the invocation without rewriting the recorded gate outcome.
        if invocation.owner:
            failures = invocation.finish(outcome="interrupted" if interrupted else ("success" if status == 0 else "failure"))
        else:
            failures = invocation.cleanup(rids=invocation.registered)
        _phase(state, "resource-cleanup", "failure" if failures else "success", "; ".join(failures))
        for failure in failures:
            print("validation resource cleanup failed: " + failure, file=sys.stderr)
        if failures and status == 0:
            status = 2
        for signum, handler in previous.items():
            signal.signal(signum, handler)
    if interrupted is not None:
        status = 128 + interrupted
    return status


def start_database(task: str, env: dict[str, str]) -> int:
    """make test-db-up: a detached invocation whose container awaits test-db-down."""
    invocation = resources.Invocation.create(task, ROOT, ["validation_fixtures.py", "up"], {"role": "test-db-up"},
                                             env, tmp=False)
    try:
        managed = resources.start_managed_postgres(
            invocation, ROOT, env, external_network=env.get("CONVEYOR_TEST_EXTERNAL_NETWORK"))
    except BaseException:
        for failure in invocation.finish(outcome="failure"):
            print("validation resource cleanup failed: " + failure, file=sys.stderr)
        raise
    invocation.finish(outcome="started", detached=True)
    print(f"invocation={invocation.path}")
    print(f"project={managed['project']}")
    print(f"container={managed['container']}")
    print(f"port={managed['port']}")
    print(f"url={managed['url']}")
    print(f"Run `make test-db-down INVOCATION={invocation.path}` to remove only this database.")
    return 0


def database_identity(reference: str | None, env: dict[str, str]) -> str:
    if reference:
        inventory = resources.load_inventory(Path(reference))
        for entry in inventory["resources"]:
            if entry["kind"] == "container":
                identity = entry["identity"]
                return f"{identity.get('port')}\t{identity.get('project')}\t{identity.get('id')}"
        raise FixtureError("invocation has no managed PostgreSQL container")
    budget = resources.postgres_budget(env)
    pinned = env.get("CONVEYOR_TEST_POSTGRES_PORT") or env.get("TEST_POSTGRES_PORT") or "auto"
    return f"{pinned}\tconveyor-test-<invocation-id>\tmemory={budget['memory']} tmpfs={budget['tmpfs']}"


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
    run.add_argument("--task", default=os.environ.get(resources.TASK_ENV) or "manual-validation")
    run.add_argument("--managed-postgres", action="store_true",
                     help="create this invocation's own PostgreSQL container and point --url-env at it")
    run.add_argument("--gate-timeout", type=float, default=0)
    run.add_argument("command", nargs=argparse.REMAINDER)
    up = actions.add_parser("up", help="start a detached managed PostgreSQL invocation (make test-db-up)")
    up.add_argument("--task", default=os.environ.get(resources.TASK_ENV) or "manual-validation")
    identity = actions.add_parser("identity", help="print managed PostgreSQL identity (make test-db-identity)")
    identity.add_argument("--invocation")
    args = parser.parse_args(argv)
    if args.action == "run":
        if args.command[:1] == ["--"]:
            args.command = args.command[1:]
        if not args.command:
            run.error("run requires a command after --")
        if args.managed_postgres and args.backend != "postgres":
            run.error("--managed-postgres requires --backend postgres")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        if args.action == "up":
            return start_database(args.task, dict(os.environ))
        if args.action == "identity":
            print(database_identity(args.invocation, dict(os.environ)))
            return 0
        return run_lifecycle(config_from_args(args), args.state.resolve(), args.command, task=args.task,
                             managed_postgres=args.managed_postgres, gate_timeout=args.gate_timeout)
    except (FixtureError, resources.ResourceError, OSError, ValueError) as exc:
        print(f"validation fixture lifecycle failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
