#!/usr/bin/env python3
"""Explicit local validation records; never a test-skipping Make prerequisite.

A policy is a reviewable input inventory, not an assertion that arbitrary code
is hermetic. Unknown inputs must be resolved before recording reusable evidence.
See docs/validation-evidence.md. Only the Python standard library is used.
"""
import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import platform
import secrets
import select
import shutil
import signal
import stat
import subprocess
import sys
import time

sys.dont_write_bytecode = True
import validation_fixtures
import validation_resources


class Refused(ValueError):
    pass


class RunInterrupted(Exception):
    def __init__(self, signum):
        super().__init__("validation interrupted by signal " + str(signum))
        self.signum = signum


class FixtureTeardownFailed(Exception):
    """Fixture teardown or owned resource cleanup failed after the gate outcome was recorded."""


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def digest_file(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as source:
        while chunk := source.read(64 * 1024):
            value.update(chunk)
    return value.hexdigest()


def require(condition, reason):
    if not condition:
        raise Refused(reason)


def execute(argv, root, env):
    result = subprocess.run(argv, cwd=root, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, check=False)
    require(result.returncode == 0, "input probe failed: " + argv[0])
    return result.stdout


def git(root, *args):
    # Do not let ambient Git overrides select another repository/index/config.
    return execute(["git", *args], root, {"PATH": os.defpath, "HOME": str(root),
                   "LC_ALL": "C", "GIT_CONFIG_NOSYSTEM": "1"})


def policy_read(path):
    p = json.loads(Path(path).read_bytes())
    required = {"schema", "task", "layer", "command", "environment",
                "tools", "external_inputs", "exclude", "audit", "backend"}
    require(set(p) in (required, required | {"fixture"}),
            "policy fields missing or unknown")
    require(p["schema"] == 1 and isinstance(p["task"], str) and p["task"].strip()
            and Path(p["task"]).name == p["task"] and p["task"] not in (".", ".."), "invalid policy identity")
    require(p["layer"] in ("local", "postgres", "singlestore"), "invalid evidence layer")
    require(isinstance(p["command"], list) and len(p["command"]) >= 2
            and p["command"][0] == "make"
            and all(isinstance(x, str) and x for x in p["command"]), "command must be a complete Make argv")
    require(isinstance(p["environment"], list) and {"PATH", "HOME"} <= set(p["environment"])
            and all(isinstance(x, str) for x in p["environment"]), "explicit PATH/HOME environment inventory required")
    a = p["audit"]
    require(set(a) == {"inputs_complete", "input_rationale", "git_metadata", "git_rationale"}
            and type(a["inputs_complete"]) is bool and isinstance(a["input_rationale"], str) and a["input_rationale"].strip()
            and a["git_metadata"] in ("dependent", "independent") and isinstance(a["git_rationale"], str) and a["git_rationale"].strip(),
            "unknown inputs or missing Git/input audit")
    require(isinstance(p["tools"], dict) and {"make", "git", "python3", "sh"} <= set(p["tools"]),
            "resolved make/git/python3/sh probes required")
    for name, command in p["tools"].items():
        require(isinstance(command, list) and command and command[0] == name
                and all(isinstance(x, str) for x in command), "invalid tool probe")
    require(isinstance(p["external_inputs"], list)
            and all(isinstance(x, str) and Path(x).is_absolute() for x in p["external_inputs"]), "external inputs must be absolute paths")
    require(isinstance(p["exclude"], dict), "exclusion inventory required")
    for name, reason in p["exclude"].items():
        require(name and not Path(name).is_absolute() and ".." not in Path(name).parts
                and name in ("bin", "web/playwright-report", "web/test-results")
                and isinstance(reason, str) and reason.strip(), "invalid exclusion or missing rationale")
    if p["layer"] == "local":
        require(p["backend"] is None, "local evidence cannot represent a database run")
    else:
        require(isinstance(p["backend"], dict) and set(p["backend"]) == {"probe", "isolation"}
                and p["backend"]["isolation"] == "disposable-per-run"
                and isinstance(p["backend"]["probe"], list) and p["backend"]["probe"],
                "backend needs an identity/version/configuration probe and disposable isolation")
    fixture = p.get("fixture")
    if fixture is not None:
        require(p["layer"] in ("postgres", "singlestore"), "local evidence cannot own a database fixture")
        required_fixture = {
            "backend", "url_env", "prepared_url_env", "external_network_env",
            "database_prefix", "minimum_free_bytes", "timeout",
        }
        require(isinstance(fixture, dict) and set(fixture) in (required_fixture, required_fixture | {"managed_postgres"}),
                "fixture fields missing or unknown")
        require(fixture["backend"] == p["layer"], "fixture backend must match evidence layer")
        require(type(fixture.get("managed_postgres", False)) is bool
                and (not fixture.get("managed_postgres") or fixture["backend"] == "postgres"),
                "managed_postgres must be a boolean and applies only to the postgres layer")
        require(fixture["url_env"] in p["environment"]
                and fixture["prepared_url_env"] in p["environment"],
                "fixture URL variables must be inventoried")
        require(isinstance(fixture["minimum_free_bytes"], int)
                and fixture["minimum_free_bytes"] > 0, "fixture capacity threshold must be positive")
    return p


def environment(p, extra=None):
    # No ambient inheritance. Only fingerprints, never values, enter records.
    result = {name: os.environ[name] for name in p["environment"] if name in os.environ}
    for name, value in (extra or {}).items():
        if name in p["environment"]:
            result[name] = value
    return result


def mac(key, value):
    return hmac.new(key, canonical(value), hashlib.sha256).hexdigest()


def inventory(root, p):
    tracked = set(git(root, "ls-files", "-z").decode().split("\0")) - {""}
    for name in tracked:
        require(not any(name == x or name.startswith(x + "/") for x in p["exclude"]),
                "cannot exclude a tracked input")
    result = {}
    external = [Path(x).resolve() for x in p["external_inputs"]]

    def visit(path, label, ancestors=()):
        require(path.exists() or path.is_symlink(), "missing input: " + label)
        info = path.lstat()
        mode = stat.S_IMODE(info.st_mode)
        if path.is_symlink():
            target = path.resolve(strict=True)
            require(target not in ancestors, "cyclic input symlink")
            require(target.is_relative_to(root) or any(target == x or target.is_relative_to(x) for x in external),
                    "symlink target outside inventoried roots: " + label)
            result[label] = {"link": os.readlink(path), "mode": mode}
            visit(target, label + "->", (*ancestors, target))
        elif path.is_dir():
            result[label] = {"directory": True, "mode": mode}
            for child in sorted(path.iterdir()):
                if child == root / ".git":
                    continue
                if child.is_relative_to(root) and str(child.relative_to(root)) in p["exclude"]:
                    continue
                visit(child, label + "/" + child.name, ancestors)
        else:
            require(stat.S_ISREG(info.st_mode), "unsupported input type: " + label)
            result[label] = {"sha256": digest(path.read_bytes()), "mode": mode}

    visit(root, "repo")
    for path in external:
        visit(path, "external:" + str(path))
    # Deleted tracked inputs matter too. Index membership itself may change on
    # commit, so content identity deliberately does not classify by trackedness.
    for name in tracked:
        if not (root / name).exists() and not (root / name).is_symlink():
            result["repo/" + name] = {"missing": True}
    return result


def git_state(root):
    require(not git(root, "ls-files", "-u"), "unresolved conflict")
    for marker in ("MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"):
        path = Path(git(root, "rev-parse", "--git-path", marker).decode().strip())
        require(not (root / path).exists(), "authored conflict or history operation in progress")
    return {"head": git(root, "rev-parse", "HEAD").decode().strip(),
            "describe": git(root, "describe", "--tags", "--always", "--dirty").decode().strip(),
            "refs": digest(git(root, "show-ref")),
            "history": digest(git(root, "log", "--all", "--format=raw")),
            "status": digest(git(root, "status", "--porcelain=v1", "--untracked-files=all")),
            "config": digest(git(root, "config", "--null", "--list")),
            "reflog": git(root, "reflog", "--format=%H %gs").decode().splitlines()}


def snapshot(root, p, key, runtime_env=None):
    env = environment(p, runtime_env)
    require({"PATH", "HOME"} <= set(env), "PATH/HOME absent")
    tools = {}
    for name, command in p["tools"].items():
        binary = shutil.which(name, path=env["PATH"])
        require(binary is not None, "tool missing: " + name)
        binary = Path(binary).resolve()
        tools[name] = {"path": str(binary), "sha256": digest(binary.read_bytes()),
                       "version_digest": digest(execute(command, root, env))}
    backend = None
    if p["backend"] is not None:
        backend = json.loads(execute(p["backend"]["probe"], root, env))
        require(set(backend) == {"identity", "version", "configuration", "instance"}
                and all(backend.values()), "backend identity/version/configuration/instance missing")
        backend = {k: mac(key, v) for k, v in backend.items()}
    machine = Path("/etc/machine-id")
    return {"files": inventory(root, p), "environment": {k: mac(key, v) for k, v in env.items()},
            "tools": tools, "backend": backend, "git": git_state(root),
            "runtime": {"platform": platform.platform(), "python": sys.version,
                        "python_binary": digest(Path(sys.executable).resolve().read_bytes()),
                        "helper": digest(Path(__file__).read_bytes()),
                        "host": mac(key, machine.read_bytes().hex() if machine.exists() else platform.node()),
                        "root": str(root)}}


def write_record(path, value, key, replace=False):
    envelope = {"record": value, "hmac_sha256": mac(key, value)}
    target = path
    if replace:
        target = path.with_name("." + path.name + "." + secrets.token_hex(6) + ".tmp")
    with target.open("xb") as f:
        os.chmod(target, 0o600)
        f.write(canonical(envelope) + b"\n")
        f.flush()
        os.fsync(f.fileno())
    if replace:
        os.replace(target, path)
    directory = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def read_record(path, key):
    e = json.loads(path.read_bytes())
    require(set(e) == {"record", "hmac_sha256"}
            and hmac.compare_digest(e["hmac_sha256"], mac(key, e["record"])), "corrupt evidence")
    return e["record"]


def location(root, output):
    output = Path(output).resolve()
    require(not output.is_relative_to(root), "durable evidence must be outside the checkout")
    common = Path(git(root, "rev-parse", "--git-common-dir").decode().strip())
    common = (root / common).resolve()
    if common.name == ".git":
        require(not output.is_relative_to(common.parent), "durable evidence must be outside the primary checkout")
    cache = Path(os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache"))).resolve()
    require(not output.is_relative_to(cache), "evidence cannot live in disposable cache")
    task_cache = os.environ.get("CONVEYOR_TASK_CACHE")
    require(not task_cache or not output.is_relative_to(Path(task_cache).resolve()), "evidence cannot live in task cache")
    tmp_root = os.environ.get(validation_resources.TMP_ROOT)
    require(not tmp_root or not output.is_relative_to(Path(tmp_root).resolve()),
            "evidence cannot live in the disposable validation temporary root")
    return output


def default_output(root, p):
    state_home = Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local" / "state")))
    require(state_home.is_absolute(), "XDG_STATE_HOME must be absolute")
    parent = location(root, state_home / "conveyor" / p["task"])
    attempt = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    return parent / ("attempt-" + attempt + "-" + str(os.getpid()) + "-" + secrets.token_hex(6))


PUBLIC_ENVIRONMENT = {
    "PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "GOCACHE", "GOTMPDIR",
    "npm_config_cache", "PLAYWRIGHT_BROWSERS_PATH", "PYTHONDONTWRITEBYTECODE",
    # Fixture metadata set by the helper itself; redacting "1" would corrupt
    # every digit in the retained log.
    "CONVEYOR_FIXTURE_PREPARED", "CONVEYOR_FIXTURE_OWNERSHIP",
}


class Redactor:
    """Incrementally redact byte strings without retaining the whole output."""

    def __init__(self, values):
        self.values = sorted({value for value in values if value}, key=len, reverse=True)
        self.keep = max((len(value) for value in self.values), default=1) - 1
        self.pending = b""

    def _replace(self, value):
        for secret in self.values:
            value = value.replace(secret, b"[REDACTED]")
        return value

    def feed(self, chunk):
        data = self.pending + chunk
        emitted = bytearray()
        cursor = 0
        safe = max(0, len(data) - self.keep)
        while cursor < safe:
            match = next((secret for secret in self.values if data.startswith(secret, cursor)), None)
            if match is not None:
                emitted.extend(b"[REDACTED]")
                cursor += len(match)
            else:
                emitted.append(data[cursor])
                cursor += 1
        self.pending = data[cursor:]
        return bytes(emitted)

    def finish(self):
        emitted = self._replace(self.pending)
        self.pending = b""
        return emitted


LEGACY_MANIFEST_FIELDS = {"schema", "kind", "state", "outcome", "policy", "before", "after", "started", "finished",
                          "exit_status", "log", "snapshot_error", "interruption", "fixture", "fixture_error"}
# Records written with an invocation inventory also name it, the configured
# timeout, and the owned resource cleanup outcome. Older records stay readable.
MANIFEST_FIELDS = LEGACY_MANIFEST_FIELDS | {"invocation", "timeout", "resource_cleanup"}


def _record_template(p, log_path, started, invocation=None, timeout=None):
    return {"schema": 1, "kind": "fresh-execution", "state": "incomplete", "outcome": "incomplete",
            "policy": p, "before": None, "after": None, "started": started, "finished": None,
            "exit_status": None,
            "log": {"path": str(log_path), "sha256": None, "bytes": 0, "completeness": "incomplete"},
            "snapshot_error": None, "interruption": None,
            "fixture": None, "fixture_error": None,
            "invocation": invocation, "timeout": timeout, "resource_cleanup": None}


def _install_interrupt_handlers():
    previous = {}

    def interrupt(signum, _frame):
        raise RunInterrupted(signum)

    for signum in (signal.SIGINT, signal.SIGTERM):
        previous[signum] = signal.signal(signum, interrupt)
    return previous


def _restore_interrupt_handlers(previous):
    for signum, handler in previous.items():
        signal.signal(signum, handler)


# Readiness waits stay short so the leader and deadline are checked even
# while output is continuously readable; the final drain is bounded.
OUTPUT_POLL_SECONDS = 0.1
OUTPUT_DRAIN_SECONDS = 10.0


def _bounded_wait(deadline):
    if deadline is None:
        return OUTPUT_POLL_SECONDS
    return min(OUTPUT_POLL_SECONDS, max(0.0, deadline - time.monotonic()))


def _drain_step(descriptor, log_file, redactor, drain_until, deadline):
    """Copy at most one ready chunk; False at EOF or when the drain bound expires."""
    if drain_until is not None:
        wait = drain_until - time.monotonic()
        if wait <= 0:
            return False  # A member that survived teardown still holds the output open.
        wait = min(wait, OUTPUT_POLL_SECONDS)
    else:
        wait = _bounded_wait(deadline)
    ready, _, _ = select.select([descriptor], [], [], wait)
    if not ready:
        return True
    chunk = os.read(descriptor, 64 * 1024)
    if not chunk:
        return False
    redacted = redactor.feed(chunk)
    if redacted:
        log_file.write(redacted)
    return True


def _record(root, p, output, timeout=None):
    output = location(root, output)
    try:
        invocation = validation_resources.Invocation.enter(
            p["task"], root, p["command"], {"layer": p["layer"], "evidence": str(output)})
    except validation_resources.ResourceError as exc:
        raise Refused(str(exc)) from exc
    try:
        # Record the output on the invocation before it exists, so owner
        # teardown and default recovery never remove it; a joined runner's
        # binding may name disposable paths this environment does not.
        invocation.retain(output)
    except validation_resources.ResourceError as exc:
        if invocation.owner:
            invocation.finish(outcome="refused: " + str(exc))
        raise Refused(str(exc)) from exc
    try:
        return _record_owned(root, p, output, invocation, timeout)
    except BaseException:
        if invocation.owner and invocation.inventory["state"] == "active":
            invocation.finish(outcome="evidence-error")
        raise


def _record_owned(root, p, output, invocation, timeout):
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    key = os.urandom(32)
    key_path = output / "key"
    with key_path.open("xb") as f:
        os.chmod(key_path, 0o600)
        f.write(key)
        f.flush()
        os.fsync(f.fileno())
    started = time.time()
    env = environment(p)
    runtime_env = env
    log_path = output / "command.log"
    with log_path.open("xb") as log_file:
        os.chmod(log_path, 0o600)
        log_file.flush()
        os.fsync(log_file.fileno())
    manifest = output / "manifest.json"
    state = _record_template(p, log_path, started, str(invocation.path), timeout)
    write_record(manifest, state, key)
    process = None
    supervised = None
    ownership = None
    fixture_config = p.get("fixture")
    fixture_state = output / "fixture"
    redactor = Redactor([])
    previous_handlers = _install_interrupt_handlers()

    def phase(name, outcome, detail=""):
        if fixture_config is not None:
            validation_fixtures._phase(fixture_state, name, outcome, detail)

    def after_snapshot():
        phase("after-snapshot", "started")
        try:
            state["after"] = snapshot(root, p, key, runtime_env)
            if state["fixture"] is not None:
                state["fixture"]["after_snapshot_outcome"] = "success"
            phase("after-snapshot", "success")
        except (Refused, OSError, ValueError) as exc:
            error = "after: " + str(exc)
            state["snapshot_error"] = (state["snapshot_error"] + "; " + error
                                       if state["snapshot_error"] else error)
            if state["fixture"] is not None:
                state["fixture"]["after_snapshot_outcome"] = "failure"
            phase("after-snapshot", "failure")
        write_record(manifest, state, key, replace=True)

    try:
        if fixture_config is not None:
            try:
                server = {"server": "external"}
                if fixture_config.get("managed_postgres"):
                    phase("managed-container", "started")
                    managed = validation_resources.start_managed_postgres(
                        invocation, root, env,
                        external_network=env.get(fixture_config["external_network_env"]))
                    env = dict(env, **{fixture_config["url_env"]: managed["url"]})
                    server = {"server": "invocation-container", "container": managed["container"]}
                    phase("managed-container", "success", "project=" + managed["project"])
                ownership, runtime_env = validation_fixtures.prepare(
                    fixture_config, env, fixture_state, invocation=invocation, server=server
                )
                state["fixture"] = {
                    "backend": ownership["backend"],
                    "database": ownership["database"],
                    "endpoint": ownership["endpoint"],
                    "ownership": str(fixture_state / "ownership.json"),
                    "phases": str(fixture_state / "phases.jsonl"),
                    "prepare_outcome": "success",
                    "before_snapshot_outcome": "pending",
                    "after_snapshot_outcome": "pending",
                    "teardown_outcome": "pending",
                }
            except (validation_fixtures.FixtureError, validation_resources.ResourceError, OSError, ValueError) as exc:
                state.update(state="complete", outcome="fixture-failure", finished=time.time(),
                             fixture_error="prepare: " + str(exc))
                state["fixture"] = {"backend": fixture_config["backend"],
                                    "prepare_outcome": "failure"}
                state["log"].update(sha256=digest(b""), completeness="complete")
                write_record(manifest, state, key, replace=True)
                return 2
        phase("before-snapshot", "started")
        try:
            state["before"] = snapshot(root, p, key, runtime_env)
            if state["fixture"] is not None:
                state["fixture"]["before_snapshot_outcome"] = "success"
            phase("before-snapshot", "success")
        except (Refused, OSError, ValueError) as exc:
            state.update(state="complete", outcome="snapshot-failure", finished=time.time(),
                         snapshot_error="before: " + str(exc))
            if state["fixture"] is not None:
                state["fixture"]["before_snapshot_outcome"] = "failure"
            phase("before-snapshot", "failure")
            phase("gate", "not-run", "before snapshot failed")
            state["log"].update(sha256=digest(b""), completeness="complete")
            write_record(manifest, state, key, replace=True)
            return 2
        state["state"] = "running"
        write_record(manifest, state, key, replace=True)
        secrets_to_redact = [value.encode() for name, value in runtime_env.items()
                             if value and name not in PUBLIC_ENVIRONMENT]
        redactor = Redactor(secrets_to_redact)
        phase("gate", "started")
        # Snapshots fingerprint the inventoried environment. The managed
        # binding and invocation temporary directories reach only the child.
        child_env = dict(runtime_env, **invocation.managed_env())
        supervised = validation_resources.start_process(
            invocation, p["command"], env=child_env, cwd=root, role="gate",
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        process = supervised.process
        deadline = time.monotonic() + timeout if timeout else None
        timed_out = False
        status = None
        drain_until = None
        output_open = True
        with log_path.open("ab", buffering=0) as log_file:
            descriptor = process.stdout.fileno()
            # Output is drained with bounded waits so neither an exited leader
            # nor a configured deadline depends on output EOF: a descendant
            # holding the inherited pipe cannot postpone cleanup, and a leader
            # that closed its output is still awaited under the deadline.
            while output_open or (status is None and drain_until is None):
                if status is None and drain_until is None:
                    status = supervised.exit_status()
                    if status is not None:
                        # The direct command exited. Its unreaped leader pins
                        # the group while stop() verifies and signals the
                        # survivors, and the status is kept.
                        stopped, detail = supervised.stop()
                        if not stopped:
                            phase("gate-processes", "cleanup-failure", detail)
                        drain_until = time.monotonic() + OUTPUT_DRAIN_SECONDS
                    elif deadline is not None and time.monotonic() >= deadline:
                        timed_out = True
                        stopped, detail = supervised.stop()
                        if not stopped:
                            phase("gate-processes", "cleanup-failure", detail)
                        drain_until = time.monotonic() + OUTPUT_DRAIN_SECONDS
                if output_open:
                    output_open = _drain_step(descriptor, log_file, redactor, drain_until, deadline)
                elif drain_until is None:
                    time.sleep(_bounded_wait(deadline))
            tail = redactor.finish()
            if tail:
                log_file.write(tail)
            log_file.flush()
            os.fsync(log_file.fileno())
        if process.poll() is None:
            # Only a refused or failed group stop leaves the leader running.
            supervised.stop()
            try:
                process.wait(timeout=OUTPUT_DRAIN_SECONDS)
            except subprocess.TimeoutExpired:
                pass
        if status is None:
            status = process.returncode
        if status is None:
            status = 2  # The leader could not be stopped; its own status is unknown.
        outcome = "timeout" if timed_out else ("success" if status == 0 else "failure")
        phase("gate", outcome, f"exit_status={status}")
        state["state"] = "finalizing"
        state["exit_status"] = status
        write_record(manifest, state, key, replace=True)
        after_snapshot()
        state["log"].update(sha256=digest_file(log_path), bytes=log_path.stat().st_size,
                            completeness="complete")
        state.update(state="complete", finished=time.time(),
                     outcome="snapshot-failure" if state["snapshot_error"] else outcome)
        write_record(manifest, state, key, replace=True)
        if timed_out:
            return 124
        return status if state["outcome"] != "snapshot-failure" else 2
    except (RunInterrupted, KeyboardInterrupt) as exc:
        if process is not None:
            supervised.stop()
            if process.stdout is not None:
                with log_path.open("ab", buffering=0) as log_file:
                    drain_until = time.monotonic() + OUTPUT_DRAIN_SECONDS
                    while _drain_step(process.stdout.fileno(), log_file, redactor, drain_until, None):
                        pass
                    log_file.write(redactor.finish())
                    log_file.flush()
                    os.fsync(log_file.fileno())
        state["log"].update(sha256=digest_file(log_path), bytes=log_path.stat().st_size,
                            completeness="complete")
        state.update(state="complete", outcome="interrupted", finished=time.time(),
                     exit_status=process.returncode if process is not None else None,
                     interruption={"signal": getattr(exc, "signum", signal.SIGINT)})
        phase("gate", "interrupted" if process is not None else "not-run")
        after_snapshot()
        write_record(manifest, state, key, replace=True)
        return 128 + int(getattr(exc, "signum", signal.SIGINT))
    finally:
        if supervised is not None and process.poll() is None:
            supervised.stop()
        if process is not None and process.stdout is not None:
            process.stdout.close()
        teardown_failed = False
        if fixture_config is not None and ownership is not None:
            if state["fixture"]["after_snapshot_outcome"] == "pending":
                after_snapshot()
            try:
                validation_fixtures.teardown(fixture_config, env, ownership, fixture_state, invocation=invocation)
                state["fixture"]["teardown_outcome"] = "success"
            except (validation_fixtures.FixtureError, OSError, ValueError) as exc:
                teardown_failed = True
                state["fixture"]["teardown_outcome"] = "failure"
                state["fixture_error"] = "teardown: " + str(exc)
            # Teardown happens after the after-snapshot and never rewrites the
            # gate outcome. Persist its independent result into the manifest.
            if manifest.exists():
                if state["state"] == "complete":
                    state["finished"] = time.time()
                write_record(manifest, state, key, replace=True)
        # Owned resources (process groups, managed container and network,
        # temporary paths) are removed by sealed identity. Their outcome is
        # recorded beside, never instead of, the gate outcome.
        if invocation.owner:
            failures = invocation.finish(outcome=state["outcome"], references=[str(output)])
        else:
            failures = invocation.cleanup(rids=invocation.registered, references=[str(output)])
        state["resource_cleanup"] = {"outcome": "failure" if failures else "success", "detail": failures}
        if manifest.exists():
            write_record(manifest, state, key, replace=True)
        _restore_interrupt_handlers(previous_handlers)
        if teardown_failed or failures:
            raise FixtureTeardownFailed


def record(root, p, output, timeout=None):
    try:
        return _record(root, p, output, timeout)
    except FixtureTeardownFailed:
        return 2


def equivalent(old, new, p):
    require(old is not None and new is not None, "missing before/after snapshot")
    require(set(old) == {"files", "environment", "tools", "backend", "git", "runtime"}
            and set(old) == set(new), "incomplete snapshot")
    for field in ("files", "environment", "tools", "backend", "runtime"):
        require(old[field] == new[field], "changed " + field)
    require(old["git"]["config"] == new["git"]["config"], "changed Git configuration")
    if p["audit"]["git_metadata"] == "dependent":
        require(old["git"] == new["git"], "changed Git metadata (including VERSION/history/state)")


def inspect_record(root, output):
    output = location(root, output)
    manifest = output / "manifest.json"
    log_path = output / "command.log"
    if not manifest.is_file():
        return {"classification": "missing-evidence", "reusable": False,
                "detail": "manifest is missing"}
    key_path = output / "key"
    if not key_path.is_file() or len(key_path.read_bytes()) != 32:
        return {"classification": "missing-evidence", "reusable": False,
                "detail": "integrity key is missing or invalid"}
    try:
        record_value = read_record(manifest, key_path.read_bytes())
    except (Refused, OSError, ValueError, KeyError, TypeError) as exc:
        return {"classification": "corrupt-evidence", "reusable": False, "detail": str(exc)}
    state = record_value.get("state")
    outcome = record_value.get("outcome")
    fixture = record_value.get("fixture")
    cleanup = record_value.get("resource_cleanup")
    if isinstance(fixture, dict) and fixture.get("teardown_outcome") == "failure":
        classification = "teardown-failure-after-" + str(outcome)
    elif isinstance(cleanup, dict) and cleanup.get("outcome") == "failure":
        classification = "cleanup-failure-after-" + str(outcome)
    elif state != "complete" or outcome == "incomplete":
        classification = "abandoned-or-incomplete"
    elif outcome in ("success", "failure", "interrupted", "timeout", "snapshot-failure", "fixture-failure"):
        classification = outcome
    else:
        classification = "unknown"
    detail = "recorded outcome; command was not replayed"
    log = record_value.get("log")
    if not isinstance(log, dict) or not log_path.is_file():
        detail = "durable log is missing; command was not replayed"
    return {"classification": classification, "reusable": False, "detail": detail}


def check(root, p, output):
    output = location(root, output)
    require(p["audit"]["inputs_complete"] is True, "unknown inputs: fresh execution only")
    key = (output / "key").read_bytes()
    require(len(key) == 32, "missing/corrupt evidence key")
    r = read_record(output / "manifest.json", key)
    require(set(r) in (LEGACY_MANIFEST_FIELDS, MANIFEST_FIELDS), "incomplete manifest")
    require(r["schema"] == 1 and r["kind"] == "fresh-execution" and r["policy"] == p, "changed command/policy/task/layer")
    require(r["state"] == "complete", "abandoned/incomplete execution")
    require(r["outcome"] in ("success", "failure")
            and r["outcome"] == ("success" if r["exit_status"] == 0 else "failure"), "invalid execution outcome")
    require(type(r["exit_status"]) is int and r["exit_status"] == 0 and r["snapshot_error"] is None
            and r["interruption"] is None
            and r["finished"] >= r["started"], "failed/incomplete execution")
    if set(r) == MANIFEST_FIELDS:
        require(isinstance(r["resource_cleanup"], dict) and r["resource_cleanup"].get("outcome") == "success",
                "owned resource cleanup did not succeed")
    require(set(r["log"]) == {"path", "sha256", "bytes", "completeness"}
            and r["log"]["path"] == str(output / "command.log")
            and r["log"]["completeness"] == "complete", "incomplete log record")
    log_path = output / "command.log"
    require(log_path.is_file(), "missing durable log")
    require(log_path.stat().st_size == r["log"]["bytes"], "truncated durable log")
    require(digest_file(log_path) == r["log"]["sha256"], "corrupt durable log")
    equivalent(r["before"], r["after"], p)
    current = snapshot(root, p, key)
    equivalent(r["after"], current, p)
    # REQ-4/AC-4.2: even a content-identical authored conflict requires new
    # evidence and independent review. Only ordinary linear commits may bridge.
    old = r["before"]["git"]
    now = current["git"]
    require(r["before"]["git"] == r["after"]["git"], "Git changed during execution")
    prior = old["reflog"]
    latest = now["reflog"]
    require(prior and len(latest) >= len(prior) and latest[-len(prior):] == prior, "unknown or rewritten history")
    additions = latest[:len(latest)-len(prior)]
    require(all(line.split(" ", 1)[1].startswith("commit: ") for line in additions), "authored conflict or non-commit history change")
    if now["head"] != old["head"]:
        git(root, "merge-base", "--is-ancestor", old["head"], now["head"])
        require(not git(root, "rev-list", "--merges", old["head"] + ".." + now["head"]), "merge/conflict resolution requires fresh validation")
        require(len(git(root, "rev-list", old["head"] + ".." + now["head"]).splitlines()) == len(additions), "unaccounted history changes")
    # Mutable database state cannot be established by matching files/version.
    # Preserve backend records, but demand a fresh isolated backend run.
    require(p["layer"] == "local", "database evidence requires a fresh isolated run; no mutable backend reuse")
    return r, current, key


def bind(root, p, output, remote, branch):
    r, current, key = check(root, p, output)
    head = current["git"]["head"]
    require(not git(root, "status", "--porcelain=v1", "--untracked-files=all"), "final head must have a clean index/worktree")
    require(branch == git(root, "symbolic-ref", "--short", "HEAD").decode().strip(), "branch mismatch")
    require(remote and not remote.startswith("-") and branch and not branch.startswith("-"), "invalid remote/branch")
    # Query the actual pushed ref, not a possibly stale remote-tracking ref.
    # Authentication uses the caller's Git configuration only for this read-only
    # remote query. It cannot change the repository used by the local snapshots.
    auth_env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    refs = execute(["git", "ls-remote", "--exit-code", remote, "refs/heads/" + branch], root, auth_env).decode().split()
    require(refs == [head, "refs/heads/" + branch], "remote head differs or was not pushed")
    require(snapshot(root, p, key) == current, "inputs changed while binding the remote head")
    binding = {"schema": 1, "kind": "reused-execution", "head": head, "remote": remote, "branch": branch,
               "bound_at": time.time(), "manifest_sha256": digest((Path(output) / "manifest.json").read_bytes()),
               "reason": "Successful unchanged before/after and current inputs; audited Git independence permits only ordinary linear commits. Local reuse is neither CI nor review approval.",
               "snapshot": current, "original_started": r["started"]}
    write_record(Path(output) / ("binding-" + head + ".json"), binding, key)
    return head


DISPOSABLE_CACHE_CHILDREN = ("go-build", "go-tmp", "tmp", "playwright", "npm")


def active_cache_users(path, proc=validation_resources.PROC, backend=None):
    """Return live or ambiguously inspected processes that may use path."""
    try:
        return validation_resources.active_cache_users(path, proc, backend=backend)
    except validation_resources.Refusal as exc:
        raise Refused(str(exc)) from exc


def cleanup_cache(task, task_cache, references):
    require(Path(task).name == task and task not in ("", ".", ".."), "invalid task identity")
    cache_home = Path(os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache")))
    require(cache_home.is_absolute(), "XDG_CACHE_HOME must be absolute")
    expected = (cache_home / "conveyor" / task).resolve()
    supplied = Path(task_cache)
    require(not supplied.is_symlink(), "task cache cannot be a symlink")
    require(supplied.resolve() == expected, "task cache must be the current task child of the Conveyor cache base")
    require(expected.is_dir() and expected.stat().st_uid == os.getuid(), "task cache ownership is missing or ambiguous")
    refs = [Path(value).resolve() for value in references]
    removable = []
    for name in DISPOSABLE_CACHE_CHILDREN:
        child = expected / name
        if not child.exists() and not child.is_symlink():
            continue
        require(not child.is_symlink(), "disposable cache child cannot be a symlink: " + name)
        resolved = child.resolve()
        require(resolved.parent == expected and resolved.is_dir() and resolved.stat().st_uid == os.getuid(),
                "disposable cache child ownership is missing or ambiguous: " + name)
        require(not any(ref == resolved or ref.is_relative_to(resolved) for ref in refs),
                "referenced evidence is inside disposable cache child: " + name)
        users = active_cache_users(resolved)
        detail = users[:20]
        if len(users) > len(detail):
            detail.append("... " + str(len(users) - len(detail)) + " more")
        require(not users, "disposable cache child is active: " + name + " (" + ", ".join(detail) + ")")
        removable.append((name, resolved))
    for _, child in removable:
        shutil.rmtree(child)
    return [name for name, _ in removable]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("run", "inspect", "check", "bind", "cleanup"))
    parser.add_argument("--policy")
    parser.add_argument("--output")
    parser.add_argument("--remote", default="origin")
    parser.add_argument("--branch")
    parser.add_argument("--task")
    parser.add_argument("--task-cache")
    parser.add_argument("--reference", action="append", default=[])
    parser.add_argument("--timeout", type=float, default=0,
                        help="run only: stop the supervised gate after this many seconds")
    args = parser.parse_args()
    try:
        if args.action == "cleanup":
            require(args.task and args.task_cache, "cleanup requires --task and --task-cache")
            removed = cleanup_cache(args.task, args.task_cache, args.reference)
            print("Removed disposable cache children: " + (", ".join(removed) if removed else "none"))
            return 0
        root = Path(git(Path.cwd(), "rev-parse", "--show-toplevel").decode().strip()).resolve()
        if args.action == "inspect":
            require(args.output, "inspect requires --output")
            print(json.dumps(inspect_record(root, args.output), sort_keys=True))
            return 0
        require(args.policy, args.action + " requires --policy")
        p = policy_read(args.policy)
        if args.action == "run":
            output = Path(args.output).resolve() if args.output else default_output(root, p)
            status = record(root, p, output, args.timeout or None)
            print("Retained validation evidence: manifest=" + str(output / "manifest.json")
                  + " log=" + str(output / "command.log") + " outcome="
                  + inspect_record(root, output)["classification"])
            if (output / "manifest.json").is_file():
                try:
                    invocation = read_record(output / "manifest.json", (output / "key").read_bytes()).get("invocation")
                except (Refused, OSError, ValueError):
                    invocation = None
                if invocation:
                    print("Invocation inventory: " + invocation)
            return status
        require(args.output, args.action + " requires --output")
        if args.action == "check":
            check(root, p, args.output)
            print("Eligible local evidence; no command was rerun.")
        else:
            require(args.branch, "bind requires the assigned branch")
            print("Bound prior execution to pushed head " + bind(root, p, args.output, args.remote, args.branch))
        return 0
    except (Refused, OSError, ValueError, KeyError, TypeError) as exc:
        print("Evidence refused; run the affected validation again: " + str(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
