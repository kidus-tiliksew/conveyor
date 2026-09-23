#!/usr/bin/env python3
"""Explicit local validation records; never a test-skipping Make prerequisite.

A policy is a reviewable input inventory, not an assertion that arbitrary code
is hermetic. Unknown inputs must be resolved before recording reusable evidence.
See docs/playbooks/conveyor-work.md. Only the Python standard library is used.
"""
import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import platform
import secrets
import shutil
import stat
import subprocess
import sys
import time


class Refused(ValueError):
    pass


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


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
    require(set(p) == {"schema", "task", "layer", "command", "environment",
                      "tools", "external_inputs", "exclude", "audit", "backend"},
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
    return p


def environment(p):
    # No ambient inheritance. Only fingerprints, never values, enter records.
    return {name: os.environ[name] for name in p["environment"] if name in os.environ}


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


def snapshot(root, p, key):
    env = environment(p)
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


def write_record(path, value, key):
    envelope = {"record": value, "hmac_sha256": mac(key, value)}
    with path.open("xb") as f:
        os.chmod(path, 0o600)
        f.write(canonical(envelope) + b"\n")
        f.flush()
        os.fsync(f.fileno())


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
    return output


def default_output(root, p):
    state_home = Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local" / "state")))
    require(state_home.is_absolute(), "XDG_STATE_HOME must be absolute")
    parent = location(root, state_home / "conveyor" / p["task"])
    attempt = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    return parent / ("attempt-" + attempt + "-" + str(os.getpid()) + "-" + secrets.token_hex(6))


def record(root, p, output):
    output = location(root, output)
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    key = os.urandom(32)
    key_path = output / "key"
    with key_path.open("xb") as f:
        os.chmod(key_path, 0o600)
        f.write(key)
    before = snapshot(root, p, key)
    started = time.time()
    env = environment(p)
    # Buffer on disk, redact every inventoried non-public environment value
    # before keeping the durable log. Do not stream raw command output to chat.
    import tempfile
    with tempfile.TemporaryFile() as raw:
        status = subprocess.run(p["command"], cwd=root, env=env, stdout=raw, stderr=subprocess.STDOUT).returncode
        raw.seek(0)
        log = raw.read()
    for name, value in env.items():
        if value and name not in ("PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "GOCACHE", "GOTMPDIR", "npm_config_cache", "PLAYWRIGHT_BROWSERS_PATH", "PYTHONDONTWRITEBYTECODE"):
            log = log.replace(value.encode(), b"[REDACTED]")
    log_path = output / "command.log"
    log_path.write_bytes(log)
    log_path.chmod(0o600)
    finished = time.time()
    after = None
    error = None
    try:
        after = snapshot(root, p, key)
    except (Refused, OSError, ValueError) as exc:
        error = str(exc)
    r = {"schema": 1, "kind": "fresh-execution", "outcome": "success" if status == 0 else "failure",
         "policy": p, "before": before, "after": after,
         "started": started, "finished": finished, "exit_status": status,
         "log": {"path": str(log_path), "sha256": digest(log), "bytes": len(log), "completeness": "complete"},
         "snapshot_error": error}
    write_record(output / "manifest.json", r, key)
    return status


def equivalent(old, new, p):
    require(old is not None and new is not None, "missing before/after snapshot")
    require(set(old) == {"files", "environment", "tools", "backend", "git", "runtime"}
            and set(old) == set(new), "incomplete snapshot")
    for field in ("files", "environment", "tools", "backend", "runtime"):
        require(old[field] == new[field], "changed " + field)
    require(old["git"]["config"] == new["git"]["config"], "changed Git configuration")
    if p["audit"]["git_metadata"] == "dependent":
        require(old["git"] == new["git"], "changed Git metadata (including VERSION/history/state)")


def check(root, p, output):
    output = location(root, output)
    require(p["audit"]["inputs_complete"] is True, "unknown inputs: fresh execution only")
    key = (output / "key").read_bytes()
    require(len(key) == 32, "missing/corrupt evidence key")
    r = read_record(output / "manifest.json", key)
    require(set(r) == {"schema", "kind", "outcome", "policy", "before", "after", "started", "finished",
                       "exit_status", "log", "snapshot_error"}, "incomplete manifest")
    require(r["schema"] == 1 and r["kind"] == "fresh-execution" and r["policy"] == p, "changed command/policy/task/layer")
    require(r["outcome"] in ("success", "failure")
            and r["outcome"] == ("success" if r["exit_status"] == 0 else "failure"), "invalid execution outcome")
    require(type(r["exit_status"]) is int and r["exit_status"] == 0 and r["snapshot_error"] is None
            and r["finished"] >= r["started"], "failed/incomplete execution")
    require(set(r["log"]) == {"path", "sha256", "bytes", "completeness"}
            and r["log"]["path"] == str(output / "command.log")
            and r["log"]["completeness"] == "complete", "incomplete log record")
    log_path = output / "command.log"
    require(log_path.is_file(), "missing durable log")
    log = log_path.read_bytes()
    require(len(log) == r["log"]["bytes"], "truncated durable log")
    require(digest(log) == r["log"]["sha256"], "corrupt durable log")
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
CACHE_ENVIRONMENT = ("GOCACHE", "GOTMPDIR", "TMPDIR", "PLAYWRIGHT_BROWSERS_PATH", "npm_config_cache")


def _inside(path, parent):
    return path == parent or path.is_relative_to(parent)


def _process_still_live(process):
    try:
        process.stat()
        return True
    except FileNotFoundError:
        return False
    except OSError:
        return None


def _inspection_failure(process, label):
    live = _process_still_live(process)
    if live is False:
        return None
    return process.name + ":ambiguous:" + label


def active_cache_users(path, proc=Path("/proc")):
    """Return live or ambiguously inspected processes that may use path."""
    path = Path(path).resolve()
    require(proc.is_dir(), "active cache ownership inspection requires /proc")
    users = []
    try:
        processes = list(proc.iterdir())
    except OSError as exc:
        raise Refused("active cache ownership inspection is ambiguous: /proc") from exc
    for process in processes:
        if not process.name.isdigit() or int(process.name) == os.getpid():
            continue
        try:
            process.stat()
        except FileNotFoundError:
            continue
        except OSError:
            users.append(process.name + ":ambiguous:process")
            continue
        process_cwd = None
        for label in ("cwd", "root"):
            candidate = process / label
            try:
                target = candidate.resolve(strict=True)
            except OSError:
                failure = _inspection_failure(process, label)
                if failure:
                    users.append(failure)
                continue
            if label == "cwd":
                process_cwd = target
            if _inside(target, path):
                users.append(process.name + ":" + label)
        descriptors = process / "fd"
        try:
            entries = list(descriptors.iterdir())
        except OSError:
            failure = _inspection_failure(process, "fd")
            if failure:
                users.append(failure)
            entries = []
        for descriptor in entries:
            try:
                raw_target = os.readlink(descriptor)
            except OSError:
                failure = _inspection_failure(process, "fd:" + descriptor.name)
                if failure:
                    users.append(failure)
                continue
            # Sockets, pipes, eventfds, and anonymous inodes are readable proc
            # entries but not filesystem paths and therefore cannot name cache
            # ownership. Absolute descriptor targets are inspected canonically.
            if not raw_target.startswith("/"):
                continue
            try:
                target = Path(raw_target.removesuffix(" (deleted)")).resolve()
            except OSError:
                users.append(process.name + ":ambiguous:fd:" + descriptor.name)
                continue
            if _inside(target, path):
                users.append(process.name + ":fd:" + descriptor.name)
        try:
            environment = (process / "environ").read_bytes()
        except OSError:
            failure = _inspection_failure(process, "environ")
            if failure:
                users.append(failure)
            continue
        for entry in environment.split(b"\0"):
            name, separator, value = entry.partition(b"=")
            if not separator or os.fsdecode(name) not in CACHE_ENVIRONMENT or not value:
                continue
            variable = os.fsdecode(name)
            candidate = Path(os.fsdecode(value))
            if not candidate.is_absolute():
                if process_cwd is None:
                    users.append(process.name + ":ambiguous:env:" + variable)
                    continue
                candidate = process_cwd / candidate
            try:
                target = candidate.resolve()
            except OSError:
                users.append(process.name + ":ambiguous:env:" + variable)
                continue
            if _inside(target, path):
                users.append(process.name + ":env:" + variable)
    return sorted(set(users), key=lambda value: (":ambiguous:" in value, value))


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
    parser.add_argument("action", choices=("run", "check", "bind", "cleanup"))
    parser.add_argument("--policy")
    parser.add_argument("--output")
    parser.add_argument("--remote", default="origin")
    parser.add_argument("--branch")
    parser.add_argument("--task")
    parser.add_argument("--task-cache")
    parser.add_argument("--reference", action="append", default=[])
    args = parser.parse_args()
    try:
        if args.action == "cleanup":
            require(args.task and args.task_cache, "cleanup requires --task and --task-cache")
            removed = cleanup_cache(args.task, args.task_cache, args.reference)
            print("Removed disposable cache children: " + (", ".join(removed) if removed else "none"))
            return 0
        require(args.policy, args.action + " requires --policy")
        root = Path(git(Path.cwd(), "rev-parse", "--show-toplevel").decode().strip()).resolve()
        p = policy_read(args.policy)
        if args.action == "run":
            output = Path(args.output).resolve() if args.output else default_output(root, p)
            status = record(root, p, output)
            print("Retained validation evidence: manifest=" + str(output / "manifest.json")
                  + " log=" + str(output / "command.log") + " outcome=" + ("success" if status == 0 else "failure"))
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
