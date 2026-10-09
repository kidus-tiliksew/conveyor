#!/usr/bin/env python3
"""Owned validation resources: invocation inventory, supervision, and recovery.

Implements component-verification-strategy "Validation resource ownership and
recovery". Each managed invocation owns a durable, owner-only inventory under
$XDG_STATE_HOME/conveyor/<task>/invocations/<id>/. Every process group,
container, network, database, and disposable path is registered as pending
before creation and sealed with its exact identity before workload starts.
Teardown and explicit recovery act only on sealed identities that still match.
Nothing here discovers cleanup candidates by name, prefix, or age. Only the
Python standard library is used.
"""

from __future__ import annotations

import argparse
import errno
import fcntl
import functools
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import re
import secrets
import select
import shutil
import signal
import socket
import stat as stat_module
import struct
import subprocess
import sys
import time

sys.dont_write_bytecode = True

BINDING = "CONVEYOR_VALIDATION_INVOCATION"
TMP_ROOT = "CONVEYOR_VALIDATION_TMP_ROOT"
ALLOW_RAM_TMP = "CONVEYOR_VALIDATION_ALLOW_RAM_TMP"
TASK_ENV = "CONVEYOR_VALIDATION_TASK"
CRASH_AT = "CONVEYOR_VALIDATION_CRASH_AT"
LABEL_INVOCATION = "sh.conveyor.validation.invocation"
LABEL_TASK = "sh.conveyor.validation.task"
COMPOSE_PROJECT_LABEL = "com.docker.compose.project"
SCHEMA = 1
KIND = "conveyor-validation-invocation"
RESOURCE_KINDS = ("process-group", "container", "network", "database", "path")
# Teardown order: supervised processes, the container, the network, databases
# on external servers, then disposable paths.
TEARDOWN_ORDER = {kind: index for index, kind in enumerate(RESOURCE_KINDS)}
RAM_FILESYSTEMS = {"tmpfs", "ramfs"}
SAFE_TASK = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
SAFE_INVOCATION = re.compile(r"^[0-9]{8}t[0-9]{6}z-[0-9a-f]{12}$")
SECRETISH = re.compile(r"://|@tcp\(|passw|token|secret|credential", re.IGNORECASE)
PROC = Path("/proc")
HELPER = Path(__file__).resolve()
ROOT = HELPER.parent.parent

# Managed PostgreSQL budgets. compose.yaml repeats these values as its
# interpolation fallbacks, and scripts/validate_compose_isolation.py keeps the
# two in agreement. The complete integration suite peaked at about 660 MiB of
# data and 950 MiB of container memory on 2026-10-01; tmpfs pages count
# against the memory limit, which leaves room for a full tmpfs plus server.
DEFAULT_POSTGRES_MEMORY = "2g"
DEFAULT_POSTGRES_TMPFS = "1g"
SIZE = re.compile(r"^([1-9][0-9]{0,6})([mg])$")


class ResourceError(RuntimeError):
    pass


class Refusal(ResourceError):
    pass


def _crash_point(name: str) -> None:
    # Reproducible forced-termination fixtures select a lifecycle boundary.
    if os.environ.get(CRASH_AT) == name:
        os.kill(os.getpid(), signal.SIGKILL)


def state_home(env=None) -> Path:
    env = os.environ if env is None else env
    value = env.get("XDG_STATE_HOME") or str(Path(env.get("HOME") or Path.home()) / ".local" / "state")
    path = Path(value)
    if not path.is_absolute():
        raise ResourceError("XDG_STATE_HOME must be an absolute path")
    return path


def invocations_root(task: str, env=None) -> Path:
    if not SAFE_TASK.fullmatch(task or ""):
        raise ResourceError("invalid validation task identity")
    return state_home(env) / "conveyor" / task / "invocations"


def sanitize_argv(argv) -> list[str]:
    return ["[redacted]" if SECRETISH.search(str(value)) else str(value) for value in argv]


# ---------------------------------------------------------------------------
# Process backends
#
# Linux process facts come from /proc. macOS facts come from libproc, sysctl,
# and statfs through ctypes. A host with neither backend keeps the refusals
# that protect process-group teardown and cache cleanup.

PROC_BACKEND = "proc"
DARWIN_BACKEND = "darwin"
UNAVAILABLE = "unavailable"


def process_backend(proc: Path = PROC, backend: str | None = None) -> str:
    """Select the backend for process facts.

    An explicit backend wins. A readable proc directory selects the /proc
    parser, including test fixtures. macOS libproc serves only the host
    default, so an injected proc path that does not exist stays unavailable.
    """
    if backend is not None:
        return backend
    if Path(proc).is_dir():
        return PROC_BACKEND
    if Path(proc) == PROC and darwin() is not None:
        return DARWIN_BACKEND
    return UNAVAILABLE


def boot_id(proc: Path = PROC) -> str | None:
    if process_backend(proc) == DARWIN_BACKEND:
        return darwin().boot_id()
    try:
        return (proc / "sys" / "kernel" / "random" / "boot_id").read_text().strip() or None
    except OSError:
        return None


def _proc_stat(pid: int, proc: Path = PROC):
    """Parse /proc/<pid>/stat, or return None when it is unreadable or malformed.

    The command name is the text between the first "(" and the last ")"; the
    kernel does not escape it, so it may itself contain parentheses or spaces.
    """
    try:
        raw = (proc / str(pid) / "stat").read_text()
    except (OSError, UnicodeDecodeError):
        return None
    head, separator, tail = raw.rpartition(")")
    opening = head.find("(")
    if not separator or opening < 0 or head[:opening].strip() != str(pid):
        return None
    fields = tail.split()
    # Fields after the command name start at field 3 (state); pgrp is field 5
    # and starttime is field 22 in proc(5).
    try:
        return {"command": head[opening + 1:], "state": fields[0], "ppid": int(fields[1]),
                "pgrp": int(fields[2]), "session": int(fields[3]), "start": int(fields[19])}
    except (IndexError, ValueError):
        return None


def process_birth(pid: int, proc: Path = PROC) -> dict | None:
    if process_backend(proc) == DARWIN_BACKEND:
        return darwin().birth(pid)
    info = _proc_stat(pid, proc)
    if info is None:
        return None
    return {"start_ticks": info["start"], "boot_id": boot_id(proc)}


def current_ticks(proc: Path = PROC) -> int | None:
    """Clock ticks since boot, comparable with process start ticks.

    macOS records process start as wall-clock microseconds, so its current
    value uses the same clock.
    """
    if process_backend(proc) == DARWIN_BACKEND:
        return time.time_ns() // 1000
    try:
        uptime = float((proc / "uptime").read_text().split()[0])
    except (OSError, ValueError, IndexError):
        return None
    return int(uptime * os.sysconf("SC_CLK_TCK"))


# macOS structure layouts from <sys/proc_info.h>, <sys/sysctl.h>, and
# <sys/mount.h> (64-bit, little-endian). Every parser rejects a buffer of the
# wrong size, so an ABI change reads as an unestablished fact rather than a
# partial parse.
BSDINFO_SIZE = 136  # struct proc_bsdinfo
KINFO_PROC_SIZE = 648  # struct kinfo_proc
VNODE_INFO_SIZE = 152  # struct vnode_info
VNODE_INFO_PATH_SIZE = VNODE_INFO_SIZE + 1024  # struct vnode_info_path: vnode_info, char[MAXPATHLEN]
VNODEPATHINFO_SIZE = 2 * VNODE_INFO_PATH_SIZE  # struct proc_vnodepathinfo: cdir, rdir
PROC_FILEINFO_SIZE = 24  # struct proc_fileinfo
FDVNODEPATH_SIZE = PROC_FILEINFO_SIZE + VNODE_INFO_PATH_SIZE  # struct vnode_fdinfowithpath
STATFS_SIZE = 2168  # struct statfs with 64-bit inodes
SZOMB = 5
PROX_FDTYPE_VNODE = 1
PROC_PIDLISTFDS = 1
PROC_PIDTBSDINFO = 3
PROC_PIDVNODEPATHINFO = 9
PROC_PIDFDVNODEPATHINFO = 2
CTL_KERN, KERN_PROC, KERN_PROCARGS2 = 1, 14, 49
KERN_PROC_PID, KERN_PROC_PGRP, KERN_PROC_UID = 1, 2, 5
DARWIN_DISK_FILESYSTEMS = {"apfs", "hfs"}
HDIUTIL = "/usr/bin/hdiutil"
DISKUTIL = "/usr/sbin/diskutil"
WHOLE_DISK = re.compile(r"(disk[0-9]+)(?:s[0-9]+)*")


def _cstring(raw: bytes) -> str:
    return os.fsdecode(raw.split(b"\0", 1)[0])


def parse_bsdinfo(raw: bytes) -> dict | None:
    """Parse struct proc_bsdinfo. Start time is wall-clock microseconds."""
    if len(raw) != BSDINFO_SIZE:
        return None
    status, _exit_status, pid, ppid, uid = struct.unpack_from("<5I", raw, 4)
    (pgid,) = struct.unpack_from("<I", raw, 100)
    seconds, microseconds = struct.unpack_from("<2Q", raw, 120)
    return {"status": status, "pid": pid, "ppid": ppid, "uid": uid, "pgid": pgid,
            "start": seconds * 1_000_000 + microseconds}


def parse_kinfo_procs(raw: bytes) -> list[tuple[int, int]] | None:
    """Parse a struct kinfo_proc array into (pid, p_stat) pairs."""
    if len(raw) % KINFO_PROC_SIZE:
        return None
    # extern_proc places p_stat at offset 36 and p_pid at offset 40.
    return [(struct.unpack_from("<i", raw, offset + 40)[0], raw[offset + 36])
            for offset in range(0, len(raw), KINFO_PROC_SIZE)]


def parse_procargs2(raw: bytes) -> tuple[list[str], list[bytes]] | None:
    """Parse KERN_PROCARGS2: argc, the exec path, NUL padding, argv, then the environment.

    The kernel withholds the environment of platform binaries such as /bin/sh;
    it then parses as empty.
    """
    if len(raw) < 4:
        return None
    (argc,) = struct.unpack_from("<i", raw, 0)
    position = raw.find(b"\0", 4)
    if argc < 0 or position < 0:
        return None
    while position < len(raw) and raw[position] == 0:
        position += 1
    strings = raw[position:].split(b"\0")
    if len(strings) < argc:
        return None
    environment = []
    for entry in strings[argc:]:
        if not entry:
            break  # Apple strings follow the environment after an empty entry.
        environment.append(entry)
    return [os.fsdecode(value) for value in strings[:argc]], environment


def parse_vnodepathinfo(raw: bytes) -> tuple[str, str] | None:
    """Parse struct proc_vnodepathinfo into (cwd, root). Root is empty unless the process chrooted."""
    if len(raw) != VNODEPATHINFO_SIZE:
        return None
    return (_cstring(raw[VNODE_INFO_SIZE:VNODE_INFO_PATH_SIZE]),
            _cstring(raw[VNODE_INFO_PATH_SIZE + VNODE_INFO_SIZE:]))


def parse_fdlist(raw: bytes) -> list[tuple[int, int]] | None:
    """Parse a struct proc_fdinfo array into (fd, type) pairs."""
    if len(raw) % 8:
        return None
    return [struct.unpack_from("<iI", raw, offset) for offset in range(0, len(raw), 8)]


def parse_fd_vnodepath(raw: bytes) -> str | None:
    """Parse struct vnode_fdinfowithpath into the descriptor's path."""
    if len(raw) != FDVNODEPATH_SIZE:
        return None
    return _cstring(raw[PROC_FILEINFO_SIZE + VNODE_INFO_SIZE:])


def parse_statfs(raw: bytes) -> dict | None:
    if len(raw) != STATFS_SIZE:
        return None
    return {"fstype": _cstring(raw[72:88]), "mounted_on": _cstring(raw[88:1112]),
            "mounted_from": _cstring(raw[1112:2136])}


def ram_disk_entities(raw: bytes) -> set[str] | None:
    """Device entries and mount points of attached ram:// images in `hdiutil info -plist` output."""
    try:
        info = plistlib.loads(raw)
    except Exception:  # plistlib raises several parser types; any of them means unknown.
        return None
    if not isinstance(info, dict) or not isinstance(info.get("images", []), list):
        return None
    entities = set()
    for image in info.get("images", []):
        if not isinstance(image, dict) or not str(image.get("image-path", "")).startswith("ram://"):
            continue
        for entity in image.get("system-entities", []):
            if isinstance(entity, dict):
                entities.update(str(entity[key]) for key in ("dev-entry", "mount-point") if entity.get(key))
    return entities


def disk_topology(raw: bytes) -> set[str] | None:
    """Devices backing one volume from `diskutil info -plist`: the volume, its whole disk, and APFS physical stores.

    An APFS volume lives in a synthesized container whose physical store is
    the real device, so the store and its whole disk are part of the backing.
    Missing topology is None, never an empty backing.
    """
    try:
        info = plistlib.loads(raw)
    except Exception:  # plistlib raises several parser types; any of them means unknown.
        return None
    if not isinstance(info, dict):
        return None
    identifier, parent = info.get("DeviceIdentifier"), info.get("ParentWholeDisk")
    if not all(isinstance(value, str) and WHOLE_DISK.fullmatch(value) for value in (identifier, parent)):
        return None
    devices = {identifier, parent}
    if info.get("FilesystemType") == "apfs" or info.get("APFSContainerReference"):
        stores = info.get("APFSPhysicalStores")
        if not isinstance(stores, list) or not stores:
            return None
        for store in stores:
            name = store.get("APFSPhysicalStore") if isinstance(store, dict) else None
            match = WHOLE_DISK.fullmatch(name) if isinstance(name, str) else None
            if match is None:
                return None
            devices.update({name, match.group(1)})
    return {"/dev/" + device for device in devices}


class Darwin:
    """macOS process and filesystem facts through libproc, sysctl, and statfs."""

    def __init__(self, ctypes):
        self._ctypes = ctypes
        libc = ctypes.CDLL(None, use_errno=True)
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        self._sysctl = libc.sysctl
        self._sysctl.argtypes = [ctypes.POINTER(ctypes.c_int), ctypes.c_uint, ctypes.c_void_p,
                                 ctypes.POINTER(ctypes.c_size_t), ctypes.c_void_p, ctypes.c_size_t]
        self._sysctlbyname = libc.sysctlbyname
        self._sysctlbyname.argtypes = [ctypes.c_char_p, ctypes.c_void_p, ctypes.POINTER(ctypes.c_size_t),
                                       ctypes.c_void_p, ctypes.c_size_t]
        # x86_64 keeps the 32-bit-inode statfs under the plain symbol name.
        self._statfs = getattr(libc, "statfs$INODE64" if platform.machine() == "x86_64" else "statfs")
        self._statfs.argtypes = [ctypes.c_char_p, ctypes.c_void_p]
        self._pidinfo = libproc.proc_pidinfo
        self._pidinfo.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_uint64, ctypes.c_void_p, ctypes.c_int]
        self._pidfdinfo = libproc.proc_pidfdinfo
        self._pidfdinfo.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_void_p, ctypes.c_int]

    def _sysctl_bytes(self, mib) -> bytes | None:
        ctypes = self._ctypes
        name = (ctypes.c_int * len(mib))(*mib)
        for _ in range(4):
            size = ctypes.c_size_t(0)
            if self._sysctl(name, len(mib), None, ctypes.byref(size), None, 0) != 0:
                return None
            if size.value == 0:
                return b""
            size = ctypes.c_size_t(size.value + size.value // 4 + 4096)  # Room for processes started meanwhile.
            buffer = ctypes.create_string_buffer(size.value)
            if self._sysctl(name, len(mib), buffer, ctypes.byref(size), None, 0) == 0:
                return buffer.raw[:size.value]
            if ctypes.get_errno() != errno.ENOMEM:
                return None
        return None

    def boot_id(self) -> str | None:
        ctypes = self._ctypes
        buffer = ctypes.create_string_buffer(64)
        size = ctypes.c_size_t(len(buffer))
        if self._sysctlbyname(b"kern.bootsessionuuid", buffer, ctypes.byref(size), None, 0) != 0:
            return None
        return _cstring(buffer.raw[:size.value]) or None

    def bsdinfo(self, pid: int) -> dict | None:
        buffer = self._ctypes.create_string_buffer(BSDINFO_SIZE)
        if self._pidinfo(pid, PROC_PIDTBSDINFO, 0, buffer, BSDINFO_SIZE) != BSDINFO_SIZE:
            return None  # Gone, a zombie, or another user's process.
        info = parse_bsdinfo(buffer.raw)
        return info if info is not None and info["pid"] == pid else None

    def birth(self, pid: int) -> dict | None:
        info = self.bsdinfo(pid)
        if info is None:
            return None
        return {"start_ticks": info["start"], "boot_id": self.boot_id()}

    def processes(self, selector: int, value: int) -> list[tuple[int, int]] | None:
        """(pid, p_stat) for KERN_PROC_PID, KERN_PROC_PGRP, or KERN_PROC_UID, including zombies."""
        raw = self._sysctl_bytes([CTL_KERN, KERN_PROC, selector, value])
        return None if raw is None else parse_kinfo_procs(raw)

    def state(self, pid: int) -> str | None:
        """Return "gone", "zombie", or "live"; None when the state cannot be established."""
        processes = self.processes(KERN_PROC_PID, pid)
        if processes is None or (processes and processes[0][0] != pid):
            return None
        if not processes:
            return "gone"
        return "zombie" if processes[0][1] == SZOMB else "live"

    def procargs(self, pid: int) -> tuple[list[str], list[bytes]] | None:
        raw = self._sysctl_bytes([CTL_KERN, KERN_PROCARGS2, pid])
        return parse_procargs2(raw) if raw else None

    def vnode_paths(self, pid: int) -> tuple[str, str] | None:
        buffer = self._ctypes.create_string_buffer(VNODEPATHINFO_SIZE)
        if self._pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, buffer, VNODEPATHINFO_SIZE) != VNODEPATHINFO_SIZE:
            return None
        return parse_vnodepathinfo(buffer.raw)

    def descriptors(self, pid: int) -> list[tuple[int, int]] | None:
        ctypes = self._ctypes
        ctypes.set_errno(0)
        size = self._pidinfo(pid, PROC_PIDLISTFDS, 0, None, 0)
        if size <= 0:
            return [] if size == 0 and ctypes.get_errno() == 0 else None
        size += 64 * 8  # Room for descriptors opened meanwhile.
        buffer = ctypes.create_string_buffer(size)
        ctypes.set_errno(0)
        filled = self._pidinfo(pid, PROC_PIDLISTFDS, 0, buffer, size)
        if filled <= 0:
            return [] if filled == 0 and ctypes.get_errno() == 0 else None
        return parse_fdlist(buffer.raw[:filled])

    def descriptor_path(self, pid: int, fd: int) -> tuple[str | None, int]:
        """Return (path, 0), or (None, errno) when the descriptor cannot be read."""
        ctypes = self._ctypes
        buffer = ctypes.create_string_buffer(FDVNODEPATH_SIZE)
        ctypes.set_errno(0)
        if self._pidfdinfo(pid, fd, PROC_PIDFDVNODEPATHINFO, buffer, FDVNODEPATH_SIZE) != FDVNODEPATH_SIZE:
            return None, ctypes.get_errno()
        return parse_fd_vnodepath(buffer.raw), 0

    def statfs(self, path: Path) -> dict | None:
        buffer = self._ctypes.create_string_buffer(STATFS_SIZE)
        if self._statfs(os.fsencode(str(path)), buffer) != 0:
            return None
        return parse_statfs(buffer.raw)


@functools.cache
def darwin() -> Darwin | None:
    """The macOS backend, or None on other hosts or when libproc cannot be loaded."""
    if sys.platform != "darwin":
        return None
    try:
        import ctypes
        return Darwin(ctypes)
    except (ImportError, OSError, AttributeError):
        return None


def host_identity() -> dict:
    machine = Path("/etc/machine-id")
    try:
        machine_id = machine.read_text().strip()
    except OSError:
        machine_id = ""
    hostname = socket.gethostname()
    return {"hostname": hostname,
            "machine": hashlib.sha256((machine_id or hostname).encode()).hexdigest()}


def _fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def write_json(path: Path, value, *, create: bool = False) -> None:
    data = (json.dumps(value, sort_keys=True, indent=1) + "\n").encode()
    target = path if create else path.with_name("." + path.name + "." + secrets.token_hex(6) + ".tmp")
    descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC, 0o600)
    try:
        os.write(descriptor, data)
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    if not create:
        os.replace(target, path)
    _fsync_directory(path.parent)


def append_jsonl(path: Path, value) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_CLOEXEC, 0o600)
    try:
        os.write(descriptor, (json.dumps(value, sort_keys=True) + "\n").encode())
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def load_inventory(path: Path) -> dict:
    try:
        value = json.loads((path / "inventory.json").read_text())
    except (OSError, ValueError) as exc:
        raise Refusal("invocation inventory is missing or corrupt") from exc
    if not isinstance(value, dict) or value.get("schema") != SCHEMA or value.get("kind") != KIND:
        raise Refusal("invocation inventory is a legacy or unknown record without sealed identities")
    required = {"task", "invocation", "checkout", "host", "owner", "resources", "state"}
    if not required <= set(value) or not isinstance(value["resources"], list):
        raise Refusal("invocation inventory lacks required identity fields")
    if value["invocation"] != path.name or not SAFE_INVOCATION.fullmatch(value["invocation"]):
        raise Refusal("invocation inventory identity does not match its directory")
    for resource in value["resources"]:
        if (not isinstance(resource, dict)
                or not {"id", "kind", "state", "identity"} <= set(resource)
                or not isinstance(resource["identity"], dict)):
            raise Refusal("invocation inventory has a resource without identity")
    # Retained evidence references were added after the first schema 1
    # records; their absence reads as none, a malformed list fails closed.
    references = value.get("references", [])
    if not isinstance(references, list) or not all(isinstance(item, str) and item for item in references):
        raise Refusal("invocation inventory has malformed retained references")
    return value


def retained_references(inventory: dict, extra=()) -> list[str]:
    """Durably recorded retained evidence references plus explicit ones.

    Records written before the references list named their evidence output
    only in configuration.evidence, which is honored as a reference too.
    """
    values = list(inventory.get("references") or [])
    evidence = (inventory.get("configuration") or {}).get("evidence")
    if isinstance(evidence, str) and evidence:
        values.append(evidence)
    values += [str(value) for value in extra]
    return list(dict.fromkeys(values))


def disposable_paths(inventory: dict) -> list[Path]:
    """The temporary root and every path an invocation may remove."""
    paths = [Path(entry["identity"]["path"]) for entry in inventory["resources"]
             if entry["kind"] == "path" and entry["identity"].get("path")]
    root = (inventory.get("tmp_root") or {}).get("path")
    if root:
        paths.append(Path(root))
    return paths


def _lock_held(path: Path) -> bool:
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_CLOEXEC)
    except FileNotFoundError:
        return False
    try:
        fcntl.flock(descriptor, fcntl.LOCK_SH | fcntl.LOCK_NB)
    except BlockingIOError:
        return True
    finally:
        os.close(descriptor)
    return False


def owner_active(path: Path, inventory: dict) -> bool:
    """The owner lock is the liveness signal; PID and birth corroborate it.

    A finished or detached owner released its lock deliberately and is not
    active even while its process continues running.
    """
    return _lock_held(path / "owner.lock") or _owner_process_running(inventory)


def _owner_process_running(inventory: dict) -> bool:
    if inventory.get("state") != "active":
        return False
    owner = inventory.get("owner") or {}
    birth = owner.get("birth")
    pid = owner.get("pid")
    return bool(birth and isinstance(pid, int) and process_birth(pid) == birth)


# ---------------------------------------------------------------------------
# Temporary root

def _unescape_mount(value: str) -> str:
    return re.sub(r"\\([0-7]{3})", lambda match: chr(int(match.group(1), 8)), value)


MOUNTINFO = PROC / "self" / "mountinfo"


def _darwin_mounts(mountinfo: Path) -> bool:
    """macOS statfs answers for the host default; an injected mount table is always parsed."""
    return Path(mountinfo) == MOUNTINFO and not Path(mountinfo).exists() and darwin() is not None


def backing_filesystem(path: Path, mountinfo: Path = MOUNTINFO) -> str | None:
    if _darwin_mounts(mountinfo):
        info = darwin().statfs(path)
        return (info or {}).get("fstype") or None
    try:
        lines = Path(mountinfo).read_text().splitlines()
    except OSError:
        return None
    best = None
    for line in lines:
        left, separator, right = line.partition(" - ")
        fields = left.split()
        if not separator or len(fields) < 5 or not right.split():
            continue
        point = Path(_unescape_mount(fields[4]))
        if path == point or path.is_relative_to(point):
            # A later entry for the same mount point shadows an earlier one.
            if best is None or len(point.parts) >= len(best[0].parts):
                best = (point, right.split()[0])
    return best[1] if best else None


def _plist_output(argv) -> bytes | None:
    try:
        result = subprocess.run(argv, capture_output=True, check=False, timeout=30)
    except (OSError, subprocess.TimeoutExpired):
        return None
    return result.stdout if result.returncode == 0 else None


def _darwin_image_backing(root: Path) -> str:
    """Classify an apfs or hfs root as RAM- or disk-backed; anything unestablished is unknown.

    The root is RAM-backed when its mount point, device, whole disk, or APFS
    physical store appears among the entities of an attached ram:// image.
    """
    info = darwin().statfs(root)
    if info is None or not info["mounted_from"].startswith("/dev/"):
        return "unknown"
    topology = _plist_output([DISKUTIL, "info", "-plist", info["mounted_from"]])
    devices = None if topology is None else disk_topology(topology)
    images = _plist_output([HDIUTIL, "info", "-plist"])
    entities = None if images is None else ram_disk_entities(images)
    if devices is None or info["mounted_from"] not in devices or entities is None:
        return "unknown"
    return "ram" if ({info["mounted_on"]} | devices) & entities else "disk"


def classify_backing(root: Path, filesystem: str | None, darwin_host: bool) -> str:
    if filesystem is None:
        return "unknown"
    if filesystem in RAM_FILESYSTEMS:
        return "ram"
    if not darwin_host:
        return "disk"
    if filesystem not in DARWIN_DISK_FILESYSTEMS:
        return "unknown"
    return _darwin_image_backing(root)


def resolve_tmp_root(task: str, checkout: Path, env=None, mountinfo: Path = MOUNTINFO) -> dict:
    env = os.environ if env is None else env
    cache_home = Path(env.get("XDG_CACHE_HOME") or Path(env.get("HOME") or Path.home()) / ".cache")
    configured = env.get(TMP_ROOT) or env.get("CONVEYOR_TASK_CACHE") or str(cache_home / "conveyor" / task)
    root = Path(configured)
    if not root.is_absolute():
        raise Refusal(f"{TMP_ROOT} must be an absolute path, got {configured!r}")
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    root = root.resolve()
    durable = (state_home(env) / "conveyor").resolve()
    if root == durable or root.is_relative_to(durable):
        raise Refusal(f"temporary root {root} is inside durable validation state; choose a disposable cache")
    checkout = Path(checkout).resolve()
    if root == checkout or root.is_relative_to(checkout):
        raise Refusal(f"temporary root {root} is inside the checkout {checkout}")
    filesystem = backing_filesystem(root, mountinfo)
    backing = classify_backing(root, filesystem, _darwin_mounts(mountinfo))
    decision = {"path": str(root), "filesystem": filesystem, "backing": backing, "override": False, "warning": None}
    if backing != "disk":
        reason = "RAM-backed" if backing == "ram" else "on a filesystem whose backing cannot be established"
        message = (f"temporary root {root} is {reason} (filesystem={filesystem or 'unknown'}); "
                   f"set {TMP_ROOT} to a disk-backed directory, or set {ALLOW_RAM_TMP}=1 to accept it explicitly")
        if env.get(ALLOW_RAM_TMP) != "1":
            raise Refusal(message)
        decision.update(override=True, warning=message)
        print("warning: " + message, file=sys.stderr)
    return decision


# ---------------------------------------------------------------------------
# Live-use inspection shared with guarded cache cleanup

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


def active_cache_users(path, proc=PROC, uid=None, created_after=None, sessions=None, backend=None,
                       disregarded=None, manager_query=None, clock_ticks=None):
    """Return live or ambiguously inspected processes that may use path.

    With uid, only that user's processes are inspected. Callers pass it only
    for owner-only (0700) directories, which other unprivileged users cannot
    enter. Every readable process is inspected in full. For an uninspectable
    (for example non-dumpable) process, created_after (start ticks) drops one
    that started before the path existed, and sessions drops one outside the
    sessions that could have inherited the path. On Linux an uninspectable
    OpenSSH session process of the invoking user is also dropped when it has
    no readable reference and a live root-owned SSH parent (see
    _ssh_session), and so is the invoking user's systemd manager when the
    local system manager authenticates it (see _user_manager), and that
    manager's (sd-pam) PAM helper (see _user_manager_helper). disregarded,
    when given, is a list that receives one record per such process so
    callers can report it. manager_query and clock_ticks replace the system
    manager query and CLK_TCK for fixtures; by default the module's
    query_user_manager and system_clock_ticks are looked up at call time.
    The macOS backend inspects only the invoking user's processes and
    applies none of the Linux rules.
    """
    path = Path(path).resolve()
    selected = process_backend(proc, backend)
    if selected == DARWIN_BACKEND:
        def started(pid):
            info = darwin().bsdinfo(int(pid))
            try:
                return None if info is None else (info["start"], os.getsid(int(pid)))
            except OSError:
                return None
        return _without_unrelated(_darwin_cache_users(path, uid), started, created_after, sessions)
    if selected == UNAVAILABLE or not Path(proc).is_dir():
        raise Refusal("active cache ownership inspection requires /proc or macOS libproc")
    users = []
    try:
        processes = list(Path(proc).iterdir())
    except OSError as exc:
        raise Refusal("active cache ownership inspection is ambiguous: /proc") from exc
    managers, helpers = [], []
    for process in processes:
        if not process.name.isdigit() or int(process.name) == os.getpid():
            continue
        try:
            owner = process.stat().st_uid
        except FileNotFoundError:
            continue
        except OSError:
            users.append(process.name + ":ambiguous:process")
            continue
        if uid is not None and owner != uid:
            continue
        info = _proc_stat(int(process.name), Path(proc))
        if info is not None and info["state"] in ("Z", "X"):
            continue  # An exited, unreaped process holds no cwd, root, or descriptors.
        # The user-manager and helper rules take their first query and reads
        # before the process's entries are inspected and their second ones
        # after every process in this scan has been inspected.
        manager = _user_manager_first(process, owner, info, Path(proc),
                                      query_user_manager if manager_query is None else manager_query,
                                      system_clock_ticks if clock_ticks is None else clock_ticks)
        helper = _user_manager_helper_first(process, owner, info, Path(proc))
        found = _linux_process_users(process, path)
        if found and all(":ambiguous:" in value for value in found):
            session = _ssh_session(process, owner, info, Path(proc))
            if session is not None:
                if disregarded is not None:
                    disregarded.append(session)
                continue
            if manager is not None:
                managers.append((process, owner, info, manager, found))
                continue
            if helper is not None:
                helpers.append((process, owner, info, helper, found))
                continue
        users += found
    # Managers are resolved before helpers, so a helper is judged against the
    # completed proof of this scan whatever order /proc listed them in.
    authenticated = {}
    for process, owner, info, manager, found in managers:
        record = _user_manager(process, owner, info, Path(proc), manager)
        if record is None:
            users += found
            continue
        authenticated[record["pid"]] = (record, manager)
        if disregarded is not None:
            disregarded.append(record)
    for process, owner, info, helper, found in helpers:
        record = _user_manager_helper(process, owner, info, Path(proc), helper, authenticated)
        if record is None:
            users += found
        elif disregarded is not None:
            disregarded.append(record)

    def started(pid):
        info = _proc_stat(int(pid), Path(proc))
        return None if info is None else (info["start"], info["session"])
    return _without_unrelated(users, started, created_after, sessions)


def _linux_process_users(process: Path, path: Path) -> list[str]:
    """Inspect one /proc entry's cwd, root, descriptors, and cache environment."""
    users = []
    process_cwd = None
    for label in ("cwd", "root"):
        candidate = process / label
        try:
            # A deleted directory reads as "<path> (deleted)"; inspect the
            # recorded path instead of treating it as uninspectable.
            target = Path(os.readlink(candidate).removesuffix(" (deleted)")).resolve()
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
        except FileNotFoundError:
            continue  # The descriptor closed during inspection.
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
        return users
    return users + _environment_users(process.name, environment.split(b"\0"), process_cwd, path)


# OpenSSH runs each connection's unprivileged session process as the connected
# user and marks it non-dumpable, so its cwd, root, fd, and environ cannot be
# read; its parent is the root-owned privilege-separation monitor. OpenSSH 9.8
# and later name both "sshd-session"; earlier releases name both "sshd".
SSH_SESSION_COMMANDS = frozenset(("sshd-session", "sshd"))
SSH_DISREGARD_REASON = "same-user-uninspectable-with-live-root-owned-ssh-parent"


def _ssh_session(process: Path, owner: int, first, proc: Path) -> dict | None:
    """Return a disregard record when an uninspectable process is an OpenSSH session.

    The caller has found no readable reference for the process. The rule
    holds only when every fact below is read and still holds after a second
    read: the process runs as the invoking user, its stat command is exactly
    sshd-session or sshd, and its stat parent is a live process owned by uid 0
    with the same kind of command that started no later than the process.
    Ownership comes from the numeric /proc directory and every other fact from
    stat, both readable for a non-dumpable process. A same-user process can
    rename itself (prctl PR_SET_NAME) but cannot acquire a root-owned SSH
    parent, and a reparented process names init or a subreaper instead. Any
    unreadable, malformed, exited, or changed fact leaves the process
    ambiguous (component-validation-tooling).
    """
    if owner != os.getuid() or first is None or first["command"] not in SSH_SESSION_COMMANDS:
        return None
    pid, ppid = int(process.name), first["ppid"]
    if ppid <= 0 or ppid == pid:
        return None
    parent_entry = proc / str(ppid)
    try:
        if parent_entry.stat().st_uid != 0:
            return None
    except OSError:
        return None
    parent = _proc_stat(ppid, proc)
    if (parent is None or parent["command"] not in SSH_SESSION_COMMANDS or parent["state"] in ("Z", "X")
            or parent["start"] > first["start"]):
        return None
    # Read every fact again: an exit, reparenting, or PID reuse during
    # inspection changes the start tick, parent, or owner.
    again = _proc_stat(pid, proc)
    parent_again = _proc_stat(ppid, proc)
    try:
        owners = (process.stat().st_uid, parent_entry.stat().st_uid)
    except OSError:
        return None
    if again is None or parent_again is None or owners != (owner, 0):
        return None
    if again["state"] in ("Z", "X") or parent_again["state"] in ("Z", "X"):
        return None
    if ((again["command"], again["ppid"], again["start"]) != (first["command"], ppid, first["start"])
            or (parent_again["command"], parent_again["start"]) != (parent["command"], parent["start"])):
        return None
    return {"pid": pid, "ppid": ppid, "command": first["command"], "parent_command": parent["command"],
            "start": first["start"], "reason": SSH_DISREGARD_REASON}


# The invoking user's systemd manager (systemd --user) is non-dumpable and
# lives for the whole login. Its command name and parent prove nothing: a
# same-user process can rename itself "systemd", make itself non-dumpable, and
# be orphaned to PID 1 or a subreaper, and the user@<uid>.service cgroup
# subtree, init.scope included, is delegated to the user. The root-controlled
# system manager's record of the service's main process, bound to the
# process's start tick, is what authenticates it (component-validation-tooling).
USER_MANAGER_KIND = "user-manager"
USER_MANAGER_COMMAND = "systemd"
USER_MANAGER_REASON = "same-user-uninspectable-system-manager-reported-mainpid"
SYSTEMCTL = "/usr/bin/systemctl"
USER_MANAGER_PROPERTIES = ("MainPID", "ExecMainStartTimestampMonotonic", "ActiveState")
USER_MANAGER_QUERY_TIMEOUT = 2.0
USER_MANAGER_OUTPUT_LIMIT = 16 * 1024
# Only a fixed locale: no inherited bus address, loader, or systemctl variable
# can redirect the query.
USER_MANAGER_ENVIRONMENT = {"LC_ALL": "C"}


def user_manager_service(uid: int) -> str:
    return f"user@{uid}.service"


def user_manager_argv(uid: int) -> list[str]:
    """The fixed read-only system-manager query; no shell, PATH lookup, user bus, or remote host."""
    argv = [SYSTEMCTL, "--system", "--no-pager", "--no-ask-password", "show", user_manager_service(uid)]
    for name in USER_MANAGER_PROPERTIES:
        argv += ["-p", name]
    return argv


def root_controlled_executable(path, lstat=os.lstat) -> bool:
    """True when path is an executable regular file that only root can replace.

    The file and every ancestor directory must not be a symlink, must be
    owned by uid 0, and must be writable by neither group nor others. Any
    unreadable component fails.
    """
    path = Path(path)
    if not path.is_absolute() or ".." in path.parts:
        return False
    components = [Path(*path.parts[:index]) for index in range(1, len(path.parts) + 1)]
    for index, component in enumerate(components):
        try:
            info = lstat(component)
        except (OSError, ValueError):
            return False
        last = index == len(components) - 1
        kind = stat_module.S_ISREG(info.st_mode) if last else stat_module.S_ISDIR(info.st_mode)
        if not kind or info.st_uid != 0 or info.st_mode & 0o022:
            return False
        if last and not info.st_mode & 0o111:
            return False
    return True


def bounded_output(argv, env, timeout, limit, popen=subprocess.Popen, clock=time.monotonic):
    """Run argv directly; return (status, combined output), or None.

    None means it could not start, outlived timeout seconds, or wrote more
    than limit bytes to standard output and standard error together. The
    child is killed and reaped on every path.
    """
    deadline = clock() + timeout
    try:
        child = popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                      env=env, close_fds=True)
    except (OSError, ValueError):
        return None
    output = bytearray()
    try:
        descriptor = child.stdout.fileno()
        while True:
            remaining = deadline - clock()
            if remaining <= 0:
                return None
            ready, _, _ = select.select([descriptor], [], [], remaining)
            if not ready:
                continue
            chunk = os.read(descriptor, 4096)
            if not chunk:
                break
            output += chunk
            if len(output) > limit:
                return None
        remaining = deadline - clock()
        if remaining <= 0:
            return None
        status = child.wait(timeout=remaining)
    except (OSError, ValueError, subprocess.TimeoutExpired):
        return None
    finally:
        if child.poll() is None:
            child.kill()
        child.wait()
        child.stdout.close()
    return status, bytes(output)


def _positive_integer(value: str) -> int | None:
    if not value.isascii() or not value.isdigit() or value != str(int(value)) or int(value) <= 0:
        return None
    return int(value)


def parse_user_manager(raw: bytes) -> dict | None:
    """Parse systemctl show output: each property exactly once, nothing else."""
    try:
        text = raw.decode("ascii")
    except UnicodeDecodeError:
        return None
    if not text.endswith("\n"):
        return None  # Empty or truncated.
    values = {}
    for line in text[:-1].split("\n"):
        key, separator, value = line.partition("=")
        if not separator or key not in USER_MANAGER_PROPERTIES or key in values:
            return None
        values[key] = value
    if len(values) != len(USER_MANAGER_PROPERTIES):
        return None
    main_pid = _positive_integer(values["MainPID"])
    monotonic = _positive_integer(values["ExecMainStartTimestampMonotonic"])
    if main_pid is None or monotonic is None:
        return None
    return {"main_pid": main_pid, "monotonic_usec": monotonic, "active_state": values["ActiveState"]}


def query_user_manager(uid: int, run=bounded_output, trusted=root_controlled_executable) -> dict | None:
    """Ask the local system manager for user@<uid>.service; None unless it is active and well formed."""
    if not trusted(SYSTEMCTL):
        return None
    result = run(user_manager_argv(uid), dict(USER_MANAGER_ENVIRONMENT), USER_MANAGER_QUERY_TIMEOUT,
                 USER_MANAGER_OUTPUT_LIMIT)
    if result is None or result[0] != 0:
        return None
    service = parse_user_manager(result[1])
    if service is None or service["active_state"] != "active":
        return None
    return service


def system_clock_ticks(sysconf=os.sysconf) -> int | None:
    """CLK_TCK as a positive integer, or None."""
    try:
        value = sysconf("SC_CLK_TCK")
    except (OSError, ValueError, TypeError):
        return None
    return value if type(value) is int and value > 0 else None


def start_matches(start_ticks: int, monotonic_usec: int, hz: int) -> bool:
    """Whether a start tick and a monotonic microsecond timestamp agree within one tick (integers only)."""
    return abs(start_ticks * 1_000_000 - monotonic_usec * hz) <= 1_000_000


def _system_manager(proc: Path):
    """The live root-owned PID 1 named systemd: (owner, command, start), or None."""
    try:
        owner = (proc / "1").stat().st_uid
    except OSError:
        return None
    info = _proc_stat(1, proc)
    if owner != 0 or info is None or info["command"] != USER_MANAGER_COMMAND or info["state"] in ("Z", "X"):
        return None
    return owner, info["command"], info["start"]


def _user_manager_first(process: Path, owner: int, first, proc: Path, query, ticks) -> dict | None:
    """The first proof for a user-manager candidate, taken before its entries are inspected, or None."""
    if owner != os.getuid() or first is None or first["command"] != USER_MANAGER_COMMAND:
        return None
    pid = int(process.name)
    if pid == 1 or first["state"] in ("Z", "X"):
        return None
    system = _system_manager(proc)
    if system is None:
        return None
    hz = ticks()
    if type(hz) is not int or hz <= 0:
        return None
    service = query(owner)
    if (service is None or service.get("active_state") != "active" or service.get("main_pid") != pid
            or not start_matches(first["start"], service["monotonic_usec"], hz)):
        return None
    return {"system": system, "service": service, "hz": hz, "query": query}


def _user_manager(process: Path, owner: int, first, proc: Path, proof: dict) -> dict | None:
    """Return a disregard record when an uninspectable process is the authenticated user manager.

    The caller has found no readable reference for the process, and
    _user_manager_first has proved before inspection that the invoking user's
    process named systemd is the active MainPID of user@<uid>.service as
    reported by a live root-owned PID 1 systemd, with a start tick within one
    clock tick of the service's monotonic start. A second, independent query
    and second reads of both stat files and owners must return the same
    facts; any unavailable, failed, or changed fact leaves the process
    ambiguous (component-validation-tooling).
    """
    pid = int(process.name)
    service = proof["query"](owner)
    if service != proof["service"]:
        return None
    again = _proc_stat(pid, proc)
    try:
        current_owner = process.stat().st_uid
    except OSError:
        return None
    if (again is None or current_owner != owner or again["state"] in ("Z", "X")
            or (again["command"], again["start"]) != (first["command"], first["start"])):
        return None
    if _system_manager(proc) != proof["system"]:
        return None
    return {"kind": USER_MANAGER_KIND, "pid": pid, "start": first["start"], "uid": owner,
            "service": user_manager_service(owner), "main_pid": service["main_pid"],
            "monotonic_usec": service["monotonic_usec"], "reason": USER_MANAGER_REASON}


# Before it executes the manager binary for user@<uid>.service, systemd forks
# a child that runs as the user, renames itself "(sd-pam)", and waits to close
# the PAM session at logout. It is non-dumpable and lives as long as the
# manager. A same-user process cannot make itself a child of the authenticated
# manager, because an orphan is reparented only to PID 1 or a subreaper, but
# the manager forks every user service it starts. The start window therefore
# excludes services started later; a service started within one second of the
# manager that renames itself "(sd-pam)" and becomes non-dumpable would still
# qualify. The operator accepted that residual risk on task 261009-227582: the
# guard prevents accidental deletion of a cache in use, and a same-user
# process can already delete the cache directly (component-validation-tooling).
USER_MANAGER_HELPER_KIND = "user-manager-helper"
USER_MANAGER_HELPER_COMMAND = "(sd-pam)"
USER_MANAGER_HELPER_REASON = "same-user-uninspectable-sd-pam-child-of-authenticated-user-manager-within-one-second"


def helper_start_in_window(helper_start: int, manager_start: int, hz) -> bool:
    """Whether a helper started no earlier than its manager and at most one second (hz ticks) after it."""
    if type(hz) is not int or hz <= 0 or type(helper_start) is not int or type(manager_start) is not int:
        return False
    return manager_start <= helper_start <= manager_start + hz


def _user_manager_helper_first(process: Path, owner: int, first, proc: Path) -> dict | None:
    """The first reads for a helper candidate and its parent, taken before its entries are inspected, or None."""
    if owner != os.getuid() or first is None or first["command"] != USER_MANAGER_HELPER_COMMAND:
        return None
    pid, ppid = int(process.name), first["ppid"]
    if pid == 1 or ppid <= 1 or ppid == pid or first["state"] in ("Z", "X"):
        return None
    try:
        parent_owner = (proc / str(ppid)).stat().st_uid
    except OSError:
        return None
    parent = _proc_stat(ppid, proc)
    if parent is None or parent["state"] in ("Z", "X"):
        return None
    return {"ppid": ppid, "parent_owner": parent_owner, "parent_command": parent["command"],
            "parent_start": parent["start"]}


def _user_manager_helper(process: Path, owner: int, first, proc: Path, helper: dict,
                         authenticated: dict) -> dict | None:
    """Return a disregard record when an uninspectable process is the authenticated manager's PAM helper.

    The caller has found no readable reference for the process, and
    authenticated maps each user manager that _user_manager disregarded in
    this scan, after every process was inspected, to its record and proof.
    The process must run as the invoking user with the stat command exactly
    "(sd-pam)", its stat parent must be one of those managers with the same
    owner, command, and start tick that _user_manager_helper_first read before
    inspection, and its start tick S_h must satisfy S_m <= S_h <= S_m + H for
    the manager's start tick S_m and the proof's CLK_TCK H. Second reads of
    both stat files and owners and of PID 1 must return the same facts; any
    unavailable, failed, or changed fact leaves the process ambiguous
    (component-validation-tooling).
    """
    pid, ppid = int(process.name), helper["ppid"]
    entry = authenticated.get(ppid)
    if entry is None or first is None or first["ppid"] != ppid:
        return None
    manager, proof = entry
    if (manager["uid"] != owner
            or (helper["parent_owner"], helper["parent_command"], helper["parent_start"])
            != (owner, USER_MANAGER_COMMAND, manager["start"])
            or not helper_start_in_window(first["start"], manager["start"], proof["hz"])):
        return None
    # Read every fact again: an exit, reparenting, or PID reuse during
    # inspection changes the start tick, parent, or owner.
    again = _proc_stat(pid, proc)
    parent_again = _proc_stat(ppid, proc)
    try:
        owners = (process.stat().st_uid, (proc / str(ppid)).stat().st_uid)
    except OSError:
        return None
    if again is None or parent_again is None or owners != (owner, owner):
        return None
    if again["state"] in ("Z", "X") or parent_again["state"] in ("Z", "X"):
        return None
    if ((again["command"], again["ppid"], again["start"]) != (first["command"], ppid, first["start"])
            or (parent_again["command"], parent_again["start"]) != (USER_MANAGER_COMMAND, manager["start"])):
        return None
    if _system_manager(proc) != proof["system"]:
        return None
    return {"kind": USER_MANAGER_HELPER_KIND, "pid": pid, "ppid": ppid, "uid": owner, "command": first["command"],
            "start": first["start"], "parent_start": manager["start"], "clk_tck": proof["hz"],
            "reason": USER_MANAGER_HELPER_REASON}


def disregard_line(record: dict) -> str:
    """The retained output line for one disregarded SSH session, user manager, or manager helper process."""
    if record.get("kind") == USER_MANAGER_HELPER_KIND:
        return (f"Disregarded Linux user manager helper: pid={record['pid']} parent={record['ppid']} "
                f"uid={record['uid']} command={record['command']} start={record['start']} "
                f"parent_start={record['parent_start']} clk_tck={record['clk_tck']} reason={record['reason']}")
    if record.get("kind") == USER_MANAGER_KIND:
        return (f"Disregarded Linux user manager: pid={record['pid']} uid={record['uid']} "
                f"service={record['service']} main_pid={record['main_pid']} start={record['start']} "
                f"monotonic_usec={record['monotonic_usec']} reason={record['reason']}")
    return (f"Disregarded Linux SSH session: pid={record['pid']} parent={record['ppid']} "
            f"command={record['command']} parent_command={record['parent_command']} reason={record['reason']}")


def distinct_disregards(records, seen: set) -> list[dict]:
    """Return records, in PID order, for processes not yet in seen (pid and start tick), adding them to it."""
    fresh = []
    for record in sorted(records, key=lambda value: (value["pid"], value["start"])):
        key = (record["pid"], record["start"])
        if key not in seen:
            seen.add(key)
            fresh.append(record)
    return fresh


def _environment_users(name: str, entries, process_cwd, path: Path) -> list[str]:
    users = []
    for entry in entries:
        key, separator, value = entry.partition(b"=")
        if not separator or os.fsdecode(key) not in CACHE_ENVIRONMENT or not value:
            continue
        variable = os.fsdecode(key)
        candidate = Path(os.fsdecode(value))
        if not candidate.is_absolute():
            if process_cwd is None:
                users.append(name + ":ambiguous:env:" + variable)
                continue
            candidate = process_cwd / candidate
        try:
            target = candidate.resolve()
        except OSError:
            users.append(name + ":ambiguous:env:" + variable)
            continue
        if _inside(target, path):
            users.append(name + ":env:" + variable)
    return users


def _without_unrelated(users, started, created_after, sessions) -> list[str]:
    """Drop ambiguity for a process that started before the path or outside its sessions.

    started(pid) returns (start, session), or None when unknown; an unknown
    process stays ambiguous.
    """
    if created_after is not None or sessions is not None:
        def unrelated(pid):
            info = started(pid)
            if info is None:
                return False
            start, session = info
            return ((created_after is not None and start < created_after)
                    or (sessions is not None and session not in sessions))
        users = [value for value in users
                 if ":ambiguous:" not in value or not unrelated(value.split(":", 1)[0])]
    return sorted(set(users), key=lambda value: (":ambiguous:" in value, value))


def _darwin_cache_users(path: Path, uid) -> list[str]:
    """macOS live-use inspection of the invoking user's processes.

    The kernel lists the user's processes (KERN_PROC_UID). libproc reports
    each one's cwd, root, and vnode descriptors, and KERN_PROCARGS2 its
    environment. A failed read of a still-live process is ambiguous. An
    environment the kernel withholds (platform binaries) parses as empty and
    binds no cache variable, and a descriptor whose path the kernel refuses
    (EPERM) names a protected vnode outside any user-removable directory.
    Other users' processes cannot be inspected without privileges and are
    outside this check.
    """
    host = darwin()
    processes = host.processes(KERN_PROC_UID, os.getuid() if uid is None else uid)
    if processes is None:
        raise Refusal("active cache ownership inspection is ambiguous: process list")
    users = []
    for pid, status in processes:
        if pid == os.getpid() or status == SZOMB:
            continue  # An exited, unreaped process holds no cwd, root, or descriptors.
        name = str(pid)

        def failure(label):
            if host.state(pid) in ("gone", "zombie"):
                return []
            return [name + ":ambiguous:" + label]

        process_cwd = None
        paths = host.vnode_paths(pid)
        if paths is None:
            users += failure("cwd")
        else:
            for label, value in zip(("cwd", "root"), paths):
                if not value:
                    continue  # rdir has no path unless the process chrooted.
                target = Path(value).resolve()
                if label == "cwd":
                    process_cwd = target
                if _inside(target, path):
                    users.append(name + ":" + label)
        descriptors = host.descriptors(pid)
        if descriptors is None:
            users += failure("fd")
            descriptors = []
        for fd, kind in descriptors:
            if kind != PROX_FDTYPE_VNODE:
                continue  # Sockets, pipes, and kqueues name no filesystem path.
            value, error = host.descriptor_path(pid, fd)
            if value is None:
                # EBADF: the descriptor closed during inspection. EPERM for one
                # descriptor of an inspectable process: the kernel protects that
                # vnode (data vault or SIP), so it cannot lie under a directory
                # this user created and may remove (operator direction 2026-10-05).
                if error not in (errno.EBADF, errno.EPERM):
                    users += failure("fd:" + str(fd))
                continue
            if not value:
                continue  # An unlinked file has no path left to remove.
            if _inside(Path(value).resolve(), path):
                users.append(name + ":fd:" + str(fd))
        arguments = host.procargs(pid)
        if arguments is None:
            users += failure("environ")
            continue
        users += _environment_users(name, arguments[1], process_cwd, path)
    return users


# ---------------------------------------------------------------------------
# Invocation inventory

class Invocation:
    """One durable invocation inventory, as its owner or as a joined launcher."""

    def __init__(self, path: Path, inventory: dict, owner_lock: int | None):
        self.path = path
        self.inventory = inventory
        self._owner_lock = owner_lock
        self.registered: list[str] = []

    @property
    def id(self) -> str:
        return self.inventory["invocation"]

    @property
    def task(self) -> str:
        return self.inventory["task"]

    @property
    def owner(self) -> bool:
        return self._owner_lock is not None

    @classmethod
    def create(cls, task: str, checkout, argv, configuration=None, env=None, tmp: bool = True) -> "Invocation":
        env = os.environ if env is None else env
        parent = invocations_root(task, env)
        parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        ident = time.strftime("%Y%m%dt%H%M%Sz", time.gmtime()) + "-" + secrets.token_hex(6)
        path = parent / ident
        path.mkdir(mode=0o700)
        lock = os.open(path / "owner.lock", os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC, 0o600)
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        os.close(os.open(path / "inventory.lock", os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC, 0o600))
        pid = os.getpid()
        inventory = {
            "schema": SCHEMA, "kind": KIND, "task": task, "invocation": ident,
            "created_at": time.time(), "checkout": str(Path(checkout).resolve()),
            "host": host_identity(),
            "owner": {"uid": os.getuid(), "pid": pid, "birth": process_birth(pid)},
            "argv": sanitize_argv(argv), "configuration": configuration or {},
            "tmp_root": None, "tmp": None, "state": "active", "detached": False,
            "finished_at": None, "outcome": None, "cleanup": None, "resources": [], "references": [],
        }
        write_json(path / "inventory.json", inventory, create=True)
        invocation = cls(path, inventory, lock)
        if tmp:
            try:
                invocation._prepare_tmp(env)
            except BaseException as exc:
                invocation.finish(outcome="refused:" + str(exc))
                raise
        return invocation

    @classmethod
    def join(cls, reference, checkout=None) -> "Invocation":
        path = Path(reference)
        if not path.is_absolute() or path.is_symlink() or not path.is_dir():
            raise Refusal(f"{BINDING} must name an existing invocation directory")
        inventory = load_inventory(path)
        if not owner_active(path, inventory):
            raise Refusal(f"bound invocation {path} has no active owner")
        if inventory["owner"].get("uid") != os.getuid():
            raise Refusal(f"bound invocation {path} is owned by another user")
        # Nested launches in one checkout share a single owner even when their
        # task labels differ (recursive Make defaults to manual-validation).
        # An inherited binding from another checkout never adopts this run.
        if checkout is not None and inventory["checkout"] != str(Path(checkout).resolve()):
            raise Refusal(f"bound invocation {path} belongs to checkout {inventory['checkout']}")
        return cls(path, inventory, None)

    @classmethod
    def enter(cls, task: str, checkout, argv, configuration=None, env=None, tmp: bool = True) -> "Invocation":
        """Join the bound active invocation, or create a new owned one."""
        env = os.environ if env is None else env
        reference = env.get(BINDING)
        if reference:
            try:
                return cls.join(reference, checkout)
            except Refusal as exc:
                print(f"warning: ignoring {BINDING}: {exc}; creating a new invocation", file=sys.stderr)
        return cls.create(task, checkout, argv, configuration, env, tmp)

    def _prepare_tmp(self, env) -> None:
        decision = resolve_tmp_root(self.task, Path(self.inventory["checkout"]), env)
        child = Path(decision["path"]) / "invocations" / self.id
        rid = self.register("path", {"path": str(child)}, role="tmp")
        child.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        child.mkdir(mode=0o700)
        for name in ("tmp", "go-tmp"):
            (child / name).mkdir(mode=0o700)
        info = child.lstat()
        self.seal(rid, {"path": str(child.resolve()), "device": info.st_dev, "inode": info.st_ino,
                        "created_ticks": current_ticks(), "boot_id": boot_id()})

        def update(inventory):
            inventory["tmp_root"] = decision
            inventory["tmp"] = {"TMPDIR": str(child / "tmp"), "GOTMPDIR": str(child / "go-tmp")}
        self._mutate(update)

    def managed_env(self) -> dict:
        values = {BINDING: str(self.path)}
        values.update(self.inventory.get("tmp") or {})
        return values

    def _mutate(self, change, write: bool = True):
        descriptor = os.open(self.path / "inventory.lock", os.O_RDWR | os.O_CLOEXEC)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX)
            current = load_inventory(self.path)
            result = change(current)
            if write:
                write_json(self.path / "inventory.json", current)
            self.inventory = current
            return result
        finally:
            os.close(descriptor)

    def refresh(self) -> dict:
        """Reload the inventory, including entries written by joined launchers."""
        return self._mutate(lambda inventory: inventory, write=False)

    def retain(self, reference) -> str:
        """Durably record a retained evidence reference before it is written.

        Owners and joined launchers both record here, so normal teardown and
        explicit recovery protect it without repeated --reference arguments.
        Evidence inside a disposable path is refused before anything exists.
        """
        target = Path(reference).resolve()

        def change(inventory):
            for disposable in disposable_paths(inventory):
                disposable = disposable.resolve()
                if _inside(target, disposable) or _inside(disposable, target):
                    raise Refusal(f"retained evidence {target} cannot live in disposable path {disposable}")
            references = inventory.setdefault("references", [])
            if str(target) not in references:
                references.append(str(target))
        self._mutate(change)
        return str(target)

    def register(self, kind: str, identity: dict, role: str | None = None) -> str:
        if kind not in RESOURCE_KINDS:
            raise ResourceError("unknown resource kind " + kind)
        rid = kind + "-" + secrets.token_hex(6)
        pid = os.getpid()
        entry = {"id": rid, "kind": kind, "role": role, "state": "pending", "identity": dict(identity),
                 "registered_at": time.time(), "registered_by": {"pid": pid, "birth": process_birth(pid)},
                 "sealed_at": None, "finished_at": None, "detail": None}
        self._mutate(lambda inventory: inventory["resources"].append(entry))
        self.registered.append(rid)
        return rid

    def resource(self, rid: str) -> dict:
        for entry in self.inventory["resources"]:
            if entry["id"] == rid:
                return entry
        raise ResourceError("unknown resource " + rid)

    def _update_resource(self, rid: str, **fields) -> dict:
        def change(inventory):
            for entry in inventory["resources"]:
                if entry["id"] == rid:
                    identity = fields.pop("identity", None)
                    if identity:
                        entry["identity"].update(identity)
                    entry.update(fields)
                    return dict(entry)
            raise ResourceError("unknown resource " + rid)
        return self._mutate(change)

    def seal(self, rid: str, identity: dict) -> dict:
        return self._update_resource(rid, identity=identity, state="sealed", sealed_at=time.time())

    def mark(self, rid: str, state: str, detail: str | None = None) -> dict:
        return self._update_resource(rid, state=state, detail=detail, finished_at=time.time())

    def cleanup(self, rids=None, references=(), grace: float = 5.0) -> list[str]:
        """Tear down sealed resources in teardown order. Return failure details.

        Durably recorded retained references always join the explicit ones.
        """
        self.refresh()
        references = retained_references(self.inventory, references)
        selected = [entry for entry in self.inventory["resources"]
                    if (rids is None or entry["id"] in rids) and entry["state"] in ("sealed", "cleanup-failed")]
        ordered = sorted(enumerate(selected), key=lambda item: (TEARDOWN_ORDER[item[1]["kind"]], -item[0]))
        failures = []
        for _, entry in ordered:
            entry = self.resource(entry["id"])
            if entry["state"] not in ("sealed", "cleanup-failed"):
                continue  # Removed with its container earlier in this pass.
            try:
                state, detail = teardown_resource(self, entry, references=references, grace=grace)
                self.mark(entry["id"], state, detail)
            except (Refusal, ResourceError, OSError) as exc:
                failures.append(f"{entry['id']}: {exc}")
                self.mark(entry["id"], "cleanup-failed", str(exc))
        return failures

    def finish(self, outcome: str | None = None, detached: bool = False, references=()) -> list[str]:
        failures = [] if detached else self.cleanup(references=references)
        if not detached:
            # Pending or ambiguous entries were never sealed, so nothing may
            # remove them automatically; report them as unfinished cleanup.
            failures += [f"{entry['id']}: {entry['state']} resource has no sealed identity"
                         for entry in self.inventory["resources"] if entry["state"] in ("pending", "ambiguous")]

        def change(inventory):
            inventory["state"] = "detached" if detached else ("cleanup-failed" if failures else "completed")
            inventory["detached"] = detached
            inventory["finished_at"] = time.time()
            inventory["outcome"] = outcome
            inventory["cleanup"] = {"outcome": "failure" if failures else "success", "detail": failures}
        self._mutate(change)
        if self._owner_lock is not None:
            os.close(self._owner_lock)
            self._owner_lock = None
        return failures


# ---------------------------------------------------------------------------
# Process supervision

def _exec_trampoline(argv: list[str]) -> int:
    """Child half of the launch handshake: wait for the sealed record, then exec."""
    descriptor = int(argv[0])
    command = argv[2:] if argv[1:2] == ["--"] else argv[1:]
    go = os.read(descriptor, 1)
    os.close(descriptor)
    if go != b"1" or not command:
        os._exit(125)
    try:
        os.execvp(command[0], command)
    except OSError as exc:
        print(f"validation launch could not execute {command[0]}: {exc.strerror}", file=sys.stderr)
        os._exit(127)
    return 127


class Supervised:
    def __init__(self, invocation: Invocation, rid: str, process: subprocess.Popen):
        self.invocation = invocation
        self.rid = rid
        self.process = process

    @property
    def pid(self) -> int:
        return self.process.pid

    def exit_status(self) -> int | None:
        """Return the leader's exit status, or None while it runs, without reaping it.

        An unreaped leader keeps its PID, and therefore its process group ID,
        out of reuse until stop() has inspected and signaled the group.
        """
        if self.process.returncode is not None:
            return self.process.returncode
        try:
            result = os.waitid(os.P_PID, self.process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
        except ChildProcessError:
            return self.process.poll()
        if result is None:
            return None
        # Popen reports a signal death as the negative signal number.
        return result.si_status if result.si_code == os.CLD_EXITED else -result.si_status

    def stop(self, grace: float = 5.0) -> tuple[bool, str]:
        entry = self.invocation.resource(self.rid)
        if entry["state"] not in ("sealed", "cleanup-failed"):
            return True, "already " + entry["state"]
        ok, detail = stop_group(entry["identity"], str(self.invocation.path), process=self.process, grace=grace)
        if ok:
            try:
                self.process.wait(timeout=grace + 5)  # Reap the exited leader.
            except subprocess.TimeoutExpired:
                pass
        self.invocation.mark(self.rid, "removed" if ok else "cleanup-failed", detail)
        return ok, detail


def start_process(invocation: Invocation, argv, *, env, cwd, role: str, stdout=None, stderr=None) -> Supervised:
    """Launch argv in its own sealed process group (launch handshake)."""
    rid = invocation.register("process-group", {"argv0": Path(argv[0]).name}, role=role)
    env = dict(env)
    env[BINDING] = str(invocation.path)
    read_fd, write_fd = os.pipe()
    try:
        process = subprocess.Popen([sys.executable, "-B", str(HELPER), "_exec", str(read_fd), "--", *argv],
                                   cwd=cwd, env=env, stdout=stdout, stderr=stderr,
                                   start_new_session=True, pass_fds=(read_fd,))
    except BaseException as exc:
        os.close(write_fd)
        invocation.mark(rid, "absent", "launch failed before execution: " + str(exc))
        raise
    finally:
        os.close(read_fd)
    try:
        _crash_point("process-before-seal")
        invocation.seal(rid, {"pgid": process.pid, "leader": process.pid,
                              "birth": process_birth(process.pid), "uid": os.getuid()})
    except BaseException:
        # Closing the pipe without the go byte makes the blocked child exit
        # without executing the command; the unreaped leader keeps its group.
        os.close(write_fd)
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
        try:
            invocation.mark(rid, "absent", "record could not be sealed; command never executed")
        except (ResourceError, OSError):
            pass
        raise
    os.write(write_fd, b"1")
    os.close(write_fd)
    return Supervised(invocation, rid, process)


def group_members(pgid: int, proc: Path = PROC) -> list[int]:
    members = []
    for entry in Path(proc).iterdir():
        if not entry.name.isdigit():
            continue
        info = _proc_stat(int(entry.name), proc)
        if info and info["pgrp"] == pgid and info["state"] not in ("Z", "X"):
            members.append(int(entry.name))
    return sorted(members)


def _member_verified(pid: int, identity: dict, reference: str, proc: Path) -> bool:
    if pid == identity.get("leader"):
        birth = identity.get("birth")
        return bool(birth) and process_birth(pid, proc) == birth
    try:
        if (Path(proc) / str(pid)).stat().st_uid != identity.get("uid"):
            return False
        environment = (Path(proc) / str(pid) / "environ").read_bytes()
    except OSError:
        return False
    return (BINDING + "=" + reference).encode() in environment.split(b"\0")


def _darwin_group_members(pgid: int) -> list[int] | None:
    """Live members of pgid from KERN_PROC_PGRP, or None when the kernel list is unavailable."""
    processes = darwin().processes(KERN_PROC_PGRP, pgid)
    if processes is None:
        return None
    return sorted(pid for pid, status in processes if status != SZOMB)


def _leader_pinned(identity: dict, process) -> bool:
    """Whether the sealed leader still holds its PID, so the group ID cannot be reused.

    The caller's own unreaped leader holds it whether it runs or has exited;
    waitid with WNOWAIT observes that without reaping. Otherwise only a live
    leader whose birth matches the seal holds it.
    """
    leader = identity.get("leader")
    if not isinstance(leader, int) or leader != identity.get("pgid"):
        return False
    if process is not None and process.pid == leader and process.returncode is None:
        try:
            os.waitid(os.P_PID, leader, os.WEXITED | os.WNOHANG | os.WNOWAIT)
            return True
        except ChildProcessError:
            pass
    birth = identity.get("birth")
    return bool(birth) and process_birth(leader) == birth


def _darwin_member_verified(pid: int, identity: dict, reference: str, pinned: bool) -> bool:
    """Verify one member: the leader by birth; any other by owner, group, session, and binding or pin.

    macOS withholds the environment of platform binaries such as /bin/sh, so
    their invocation binding is unreadable. While the leader pins the group
    ID, every member of that group descends from the sealed session and needs
    no binding; without the pin such a member stays unverified.
    """
    if pid == identity.get("leader"):
        birth = identity.get("birth")
        return bool(birth) and process_birth(pid) == birth
    info = darwin().bsdinfo(pid)
    if info is None or info["uid"] != identity.get("uid") or info["pgid"] != identity.get("pgid"):
        return False
    try:
        if os.getsid(pid) != identity.get("pgid"):
            return False
    except OSError:
        return False
    if pinned:
        return True
    arguments = darwin().procargs(pid)
    return arguments is not None and (BINDING + "=" + reference).encode() in arguments[1]


def _stop_darwin_group(identity: dict, reference: str, pgid: int, process, grace: float,
                       kill_wait: float) -> tuple[bool, str]:
    for sig, wait in ((signal.SIGTERM, grace), (signal.SIGKILL, kill_wait)):
        members = _darwin_group_members(pgid)
        if members is None:
            return False, f"refused to signal process group {pgid}: its members cannot be listed"
        if not members:
            return True, f"process group {pgid} has no remaining members"
        pinned = _leader_pinned(identity, process)
        unverified = [pid for pid in members if not _darwin_member_verified(pid, identity, reference, pinned)]
        if unverified:
            # A member that exits between listing and verification fails the
            # check without being a member any longer; re-list before refusing.
            current = _darwin_group_members(pgid)
            if current is None:
                return False, f"refused to signal process group {pgid}: its members cannot be listed"
            unverified = [pid for pid in unverified
                          if pid in current and not _darwin_member_verified(pid, identity, reference, pinned)]
        if unverified:
            return False, (f"refused to signal process group {pgid}: members {unverified} "
                           "do not match the sealed birth identity, invocation binding, or pinned group")
        try:
            os.killpg(pgid, sig)
        except ProcessLookupError:
            return True, f"process group {pgid} exited"
        except PermissionError:
            # macOS refuses a group signal once only an unreaped zombie leader remains.
            if _darwin_group_members(pgid) == []:
                return True, f"process group {pgid} exited"
            return False, f"signal to process group {pgid} was refused"
        deadline = time.monotonic() + wait
        while time.monotonic() < deadline:
            if _darwin_group_members(pgid) == []:
                return True, f"process group {pgid} stopped by {signal.Signals(sig).name}"
            time.sleep(0.05)
    remaining = _darwin_group_members(pgid)
    if remaining != []:
        return False, f"process group {pgid} members {remaining} survived SIGKILL"
    return True, f"process group {pgid} stopped by SIGKILL"


def stop_group(identity: dict, reference: str, *, process=None, grace: float = 5.0,
               kill_wait: float = 5.0, proc: Path = PROC, backend: str | None = None) -> tuple[bool, str]:
    """TERM, then KILL, a verified sealed process group with bounded waits."""
    pgid = identity.get("pgid")
    if not isinstance(pgid, int) or pgid <= 1:
        raise Refusal("process group identity is missing")
    selected = process_backend(proc, backend)
    if selected == DARWIN_BACKEND:
        return _stop_darwin_group(identity, reference, pgid, process, grace, kill_wait)
    if selected == UNAVAILABLE or not Path(proc).is_dir():
        return _stop_group_without_proc(pgid, process, grace, kill_wait)
    for sig, wait in ((signal.SIGTERM, grace), (signal.SIGKILL, kill_wait)):
        if process is not None:
            process.poll()
        members = group_members(pgid, proc)
        if not members:
            return True, f"process group {pgid} has no remaining members"
        unverified = [pid for pid in members if not _member_verified(pid, identity, reference, proc)]
        if unverified:
            return False, (f"refused to signal process group {pgid}: members {unverified} "
                           "do not match the sealed birth identity or invocation binding")
        try:
            os.killpg(pgid, sig)
        except ProcessLookupError:
            return True, f"process group {pgid} exited"
        deadline = time.monotonic() + wait
        while time.monotonic() < deadline:
            if process is not None:
                process.poll()
            if not group_members(pgid, proc):
                return True, f"process group {pgid} stopped by {signal.Signals(sig).name}"
            time.sleep(0.05)
    remaining = group_members(pgid, proc)
    if remaining:
        return False, f"process group {pgid} members {remaining} survived SIGKILL"
    return True, f"process group {pgid} stopped by SIGKILL"


def _stop_group_without_proc(pgid, process, grace, kill_wait) -> tuple[bool, str]:
    # Without /proc only an unreaped leader keeps the group identity certain.
    if process is None or process.returncode is not None or process.poll() is not None:
        try:
            os.killpg(pgid, 0)
        except ProcessLookupError:
            return True, f"process group {pgid} has no remaining members"
        return False, f"surviving members of process group {pgid} cannot be verified without /proc"
    for sig, wait in ((signal.SIGTERM, grace), (signal.SIGKILL, kill_wait)):
        try:
            os.killpg(pgid, sig)
        except ProcessLookupError:
            break
        try:
            process.wait(timeout=wait)
            break
        except subprocess.TimeoutExpired:
            continue
    return _stop_group_without_proc(pgid, None, 0, 0)


# ---------------------------------------------------------------------------
# Docker resources

def docker(args, *, env=None, cwd=None, timeout: float = 120) -> subprocess.CompletedProcess:
    try:
        return subprocess.run(["docker", *args], env=env, cwd=cwd, capture_output=True,
                              text=True, check=False, timeout=timeout)
    except FileNotFoundError as exc:
        raise ResourceError("docker executable is unavailable") from exc
    except subprocess.TimeoutExpired as exc:
        raise ResourceError(f"docker {args[0]} timed out after {timeout}s") from exc


def _docker_inspect(kind: str, identifier: str):
    args = ["inspect", "--type", "container", identifier] if kind == "container" else ["network", "inspect", identifier]
    result = docker(args)
    if result.returncode != 0:
        if "no such" in result.stderr.lower() or "not found" in result.stderr.lower():
            return None
        raise ResourceError(f"docker {kind} inspection failed: {result.stderr.strip()[-200:]}")
    values = json.loads(result.stdout or "[]")
    return values[0] if values else None


def _labels(info: dict, kind: str) -> dict:
    return ((info.get("Config") or {}).get("Labels") if kind == "container" else info.get("Labels")) or {}


def _verify_docker_identity(kind: str, info: dict, identity: dict, invocation_id: str) -> None:
    labels = _labels(info, kind)
    if info.get("Id") != identity.get("id"):
        raise Refusal(f"{kind} identity changed: recorded {identity.get('id')}, found {info.get('Id')}")
    if labels.get(LABEL_INVOCATION) != invocation_id:
        raise Refusal(f"{kind} {identity.get('id')} does not carry this invocation's label")
    if labels.get(COMPOSE_PROJECT_LABEL) != identity.get("project"):
        raise Refusal(f"{kind} {identity.get('id')} is not bound to project {identity.get('project')}")


def remove_container(identity: dict, invocation_id: str) -> tuple[str, str]:
    info = _docker_inspect("container", identity.get("id") or "")
    if info is None:
        return "absent", "container already absent"
    _verify_docker_identity("container", info, identity, invocation_id)
    result = docker(["rm", "--force", "--volumes", identity["id"]])
    if result.returncode != 0:
        raise ResourceError("container removal failed: " + result.stderr.strip()[-200:])
    return "removed", "removed container " + identity["id"]


def remove_network(identity: dict, invocation_id: str) -> tuple[str, str]:
    if identity.get("external"):
        raise Refusal("external networks are configuration and are never removed")
    info = _docker_inspect("network", identity.get("id") or "")
    if info is None:
        return "absent", "network already absent"
    _verify_docker_identity("network", info, identity, invocation_id)
    result = docker(["network", "rm", identity["id"]])
    if result.returncode != 0:
        raise ResourceError("network removal failed: " + result.stderr.strip()[-200:])
    return "removed", "removed network " + identity["id"]


def parse_size(value: str, name: str) -> int:
    match = SIZE.fullmatch(value or "")
    if not match:
        raise Refusal(f"{name} must be a positive integer with an m or g suffix, got {value!r}")
    return int(match.group(1)) * (1024 ** 2 if match.group(2) == "m" else 1024 ** 3)


def postgres_budget(env=None) -> dict:
    env = os.environ if env is None else env
    memory = env.get("CONVEYOR_TEST_POSTGRES_MEMORY") or DEFAULT_POSTGRES_MEMORY
    tmpfs = env.get("CONVEYOR_TEST_POSTGRES_TMPFS_SIZE") or DEFAULT_POSTGRES_TMPFS
    memory_bytes = parse_size(memory, "CONVEYOR_TEST_POSTGRES_MEMORY")
    tmpfs_bytes = parse_size(tmpfs, "CONVEYOR_TEST_POSTGRES_TMPFS_SIZE")
    if tmpfs_bytes >= memory_bytes:
        raise Refusal("CONVEYOR_TEST_POSTGRES_TMPFS_SIZE must be smaller than CONVEYOR_TEST_POSTGRES_MEMORY "
                      "because tmpfs pages count against the container memory limit")
    return {"memory": memory, "memory_bytes": memory_bytes, "tmpfs": tmpfs, "tmpfs_bytes": tmpfs_bytes}


def _port_available(port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        try:
            probe.bind(("127.0.0.1", port))
        except OSError:
            return False
    return True


def select_port(env=None) -> tuple[int, bool]:
    env = os.environ if env is None else env
    pinned = env.get("CONVEYOR_TEST_POSTGRES_PORT") or env.get("TEST_POSTGRES_PORT")
    if pinned:
        if not pinned.isdigit() or not 1 <= int(pinned) <= 65535:
            raise Refusal(f"pinned PostgreSQL port must be 1-65535, got {pinned!r}")
        if not _port_available(int(pinned)):
            raise Refusal(f"pinned PostgreSQL port {pinned} is occupied; validation never attaches to or reuses it")
        return int(pinned), True
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1], False


def project_name(invocation: Invocation) -> str:
    return "conveyor-test-" + invocation.id


def _labelled(kind: str, project: str, invocation_id: str) -> list[str]:
    filters = ["--filter", f"label={COMPOSE_PROJECT_LABEL}={project}",
               "--filter", f"label={LABEL_INVOCATION}={invocation_id}"]
    args = (["ps", "--all", "--quiet", "--no-trunc", *filters] if kind == "container"
            else ["network", "ls", "--quiet", "--no-trunc", *filters])
    result = docker(args)
    if result.returncode != 0:
        raise ResourceError(f"docker {kind} listing failed: {result.stderr.strip()[-200:]}")
    return [line for line in result.stdout.split() if line]


def _project_in_use(project: str) -> bool:
    for args in (["ps", "--all", "--quiet", "--filter", f"label={COMPOSE_PROJECT_LABEL}={project}"],
                 ["network", "ls", "--quiet", "--filter", f"label={COMPOSE_PROJECT_LABEL}={project}"]):
        result = docker(args)
        if result.returncode != 0:
            raise ResourceError("docker project inspection failed: " + result.stderr.strip()[-200:])
        if result.stdout.strip():
            return True
    return False


def _seal_docker(invocation: Invocation, rid: str, kind: str, project: str, extra=None) -> dict | None:
    found = _labelled(kind, project, invocation.id)
    if not found:
        invocation.mark(rid, "absent", f"no {kind} was created")
        return None
    if len(found) != 1:
        invocation._update_resource(rid, state="ambiguous", detail=f"{len(found)} {kind}s carry the project label")
        raise ResourceError(f"{kind} identity for project {project} is ambiguous")
    info = _docker_inspect(kind, found[0])
    labels = _labels(info or {}, kind)
    if info is None or labels.get(LABEL_INVOCATION) != invocation.id or labels.get(COMPOSE_PROJECT_LABEL) != project:
        invocation._update_resource(rid, state="ambiguous", detail=f"{kind} labels could not be verified")
        raise ResourceError(f"{kind} for project {project} could not be sealed")
    identity = {"id": info["Id"], "project": project, "labels": {
        LABEL_INVOCATION: labels.get(LABEL_INVOCATION), LABEL_TASK: labels.get(LABEL_TASK)}}
    if kind == "container":
        host = info.get("HostConfig") or {}
        identity.update(image=(info.get("Config") or {}).get("Image"), memory=host.get("Memory"),
                        tmpfs=host.get("Tmpfs"), name=info.get("Name", "").lstrip("/"))
    else:
        identity.update(name=info.get("Name"), external=False)
    identity.update(extra or {})
    invocation.seal(rid, identity)
    return identity


def start_managed_postgres(invocation: Invocation, checkout, env=None, *, external_network: str | None = None,
                           ready_timeout: float = 90) -> dict:
    """Create, seal, and then start this invocation's PostgreSQL container."""
    env = dict(os.environ if env is None else env)
    checkout = Path(checkout).resolve()
    budget = postgres_budget(env)
    port, pinned = select_port(env)
    project = project_name(invocation)
    if _project_in_use(project):
        raise Refusal(f"Compose project {project} already has containers or networks; refusing to adopt them")
    labels = {LABEL_INVOCATION: invocation.id, LABEL_TASK: invocation.task}
    container_rid = invocation.register("container", {"project": project, "service": "postgres-test",
                                                      "labels": labels, "port": port}, role="postgres")
    network_rid = None
    if not external_network:
        network_rid = invocation.register("network", {"project": project, "name": project + "_default"},
                                          role="postgres-network")
    compose_env = dict(env)
    compose_env.update({
        "CONVEYOR_TEST_POSTGRES_PORT": str(port),
        "CONVEYOR_TEST_NETWORK_NAME": external_network or project + "_default",
        "CONVEYOR_TEST_NETWORK_EXTERNAL": "true" if external_network else "false",
        "CONVEYOR_TEST_POSTGRES_MEMORY": budget["memory"],
        "CONVEYOR_TEST_POSTGRES_TMPFS_SIZE": budget["tmpfs"],
        "CONVEYOR_VALIDATION_INVOCATION_ID": invocation.id,
        "CONVEYOR_VALIDATION_TASK_LABEL": invocation.task,
    })
    created = docker(["compose", "-p", project, "-f", str(checkout / "compose.yaml"),
                      "--project-directory", str(checkout), "--profile", "test", "create", "postgres-test"],
                     env=compose_env, cwd=checkout, timeout=300)
    _crash_point("container-after-create")
    container = _seal_docker(invocation, container_rid, "container", project,
                             {"port": port, "pinned_port": pinned, "budget": budget})
    if network_rid is not None:
        _seal_docker(invocation, network_rid, "network", project)
    if created.returncode != 0 or container is None:
        raise ResourceError("managed PostgreSQL container creation failed: " + created.stderr.strip()[-300:])
    if container.get("memory") != budget["memory_bytes"]:
        raise ResourceError(f"managed PostgreSQL memory limit was not applied: {container.get('memory')}")
    _crash_point("container-after-seal")
    started = docker(["start", container["id"]])
    if started.returncode != 0:
        raise ResourceError("managed PostgreSQL container did not start: " + started.stderr.strip()[-300:])
    deadline = time.monotonic() + ready_timeout
    status = "unknown"
    while time.monotonic() < deadline:
        info = _docker_inspect("container", container["id"]) or {}
        state = info.get("State") or {}
        status = (state.get("Health") or {}).get("Status") or state.get("Status") or "unknown"
        if status == "healthy":
            break
        if state.get("Status") in ("exited", "dead"):
            raise ResourceError(f"managed PostgreSQL container exited before readiness (exit {state.get('ExitCode')})")
        time.sleep(0.5)
    else:
        raise ResourceError(f"managed PostgreSQL container was not healthy within {ready_timeout}s (status={status})")
    return {"url": f"postgres://conveyor:conveyor@127.0.0.1:{port}/conveyor_test?sslmode=disable",
            "port": port, "project": project, "container": container["id"], "container_resource": container_rid,
            "budget": budget}


# ---------------------------------------------------------------------------
# Databases and paths

def drop_database(identity: dict, checkout: Path, env=None) -> tuple[str, str]:
    if identity.get("server") == "invocation-container":
        raise Refusal("database lives in an invocation container; remove the container instead")
    if identity.get("backend") != "postgres" or not identity.get("incarnation"):
        raise Refusal(f"{identity.get('backend')} database {identity.get('database')} exposes no verified "
                      "incarnation identity; operator handling is required")
    env = dict(os.environ if env is None else env)
    url_env = identity.get("url_env") or ""
    if not env.get(url_env):
        raise Refusal(f"configure {url_env or 'the source URL variable'} to recover database {identity.get('database')}")
    from urllib.parse import urlsplit
    parsed = urlsplit(env[url_env])
    if f"{parsed.hostname}:{parsed.port or 5432}" != identity.get("endpoint"):
        raise Refusal("configured PostgreSQL endpoint differs from the recorded fixture endpoint")
    env["CONVEYOR_FIXTURE_ADMIN_DSN"] = env[url_env]
    result = subprocess.run(["go", "run", "./scripts/validation-fixture-sql", "--backend", "postgres",
                             "--action", "drop", "--dsn-env", "CONVEYOR_FIXTURE_ADMIN_DSN",
                             "--database", identity["database"], "--expect-incarnation", str(identity["incarnation"])],
                            cwd=checkout, env=env, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        detail = result.stderr.strip().splitlines()[-1:] or ["no detail"]
        if "incarnation mismatch" in detail[0]:
            raise Refusal("database incarnation changed; refusing to drop " + identity["database"])
        raise ResourceError("database drop failed: " + detail[0])
    if '"absent"' in result.stdout:
        return "absent", "database already absent"
    return "removed", "dropped database " + identity["database"]


def remove_path(identity: dict, references=(), created_after=None, sessions=None) -> tuple[str, str]:
    path = Path(identity.get("path") or "")
    if not path.is_absolute():
        raise Refusal("recorded path is not absolute")
    if not path.exists() and not path.is_symlink():
        return "absent", "path already absent"
    if path.is_symlink() or path.resolve() != path:
        raise Refusal(f"recorded path {path} was substituted by a symlink")
    info = path.lstat()
    if info.st_dev != identity.get("device") or info.st_ino != identity.get("inode"):
        raise Refusal(f"recorded path {path} was substituted (device or inode changed)")
    if info.st_uid != os.getuid():
        raise Refusal(f"recorded path {path} is owned by another user")
    if info.st_mode & 0o077:
        raise Refusal(f"recorded path {path} is no longer owner-only")
    for reference in references:
        resolved = Path(reference).resolve()
        if resolved == path or resolved.is_relative_to(path) or path.is_relative_to(resolved):
            raise Refusal(f"recorded path {path} contains or is named by retained reference {reference}")
    seen, disregards = set(), []

    def scan():
        found = []
        users = active_cache_users(path, uid=os.getuid(), created_after=created_after, sessions=sessions,
                                   disregarded=found)
        disregards.extend(distinct_disregards(found, seen))
        return users

    users = scan()
    for _ in range(3):
        # Readable evidence refuses at once. Inspection races (a process in
        # exec or exiting) are rechecked; only persistent ambiguity refuses.
        if not users or any(":ambiguous:" not in value for value in users):
            break
        time.sleep(0.2)
        again = set(scan())
        users = [value for value in users if value in again]
    # Disregarded SSH session, user manager, and manager helper processes are
    # named in the refusal or in the resource detail, which the inventory and
    # recovery.jsonl retain. Every rescan queries the system manager afresh.
    reported = "".join("; " + disregard_line(record) for record in disregards)
    if users:
        raise Refusal(f"recorded path {path} is in use: " + ", ".join(users[:10]) + reported)
    shutil.rmtree(path)
    return "removed", "removed disposable path " + str(path) + reported


def teardown_resource(invocation: Invocation, entry: dict, references=(), grace: float = 5.0) -> tuple[str, str]:
    kind = entry["kind"]
    identity = entry["identity"]
    if kind == "process-group":
        ok, detail = stop_group(identity, str(invocation.path), grace=grace)
        if not ok:
            raise Refusal(detail)
        return "removed", detail
    if kind == "container":
        state, detail = remove_container(identity, invocation.id)
        for other in invocation.inventory["resources"]:
            if (other["kind"] == "database" and other["state"] == "sealed"
                    and other["identity"].get("server") == "invocation-container"
                    and other["identity"].get("container") == identity.get("id")):
                invocation.mark(other["id"], "removed", "removed with invocation container")
        return state, detail
    if kind == "network":
        return remove_network(identity, invocation.id)
    if kind == "database":
        return drop_database(identity, Path(invocation.inventory["checkout"]))
    if kind == "path":
        same_boot = identity.get("boot_id") and identity.get("boot_id") == boot_id()
        # Supervised groups start their own sessions (setsid), so a process
        # that inherited the temporary child stays in one of these sessions.
        sessions = {entry["identity"].get("pgid") for entry in invocation.inventory["resources"]
                    if entry["kind"] == "process-group" and entry["identity"].get("pgid")}
        return remove_path(identity, references, identity.get("created_ticks") if same_boot else None,
                           sessions if same_boot else None)
    raise Refusal("unknown resource kind " + kind)


# ---------------------------------------------------------------------------
# Inspection and recovery

def _classify(entry: dict, active: bool) -> str:
    state = entry["state"]
    if state in ("pending", "ambiguous", "cleanup-failed"):
        return state
    if state in ("removed", "absent"):
        return "completed"
    if state == "sealed":
        return "active" if active else "abandoned"
    return "unknown"


def inspect_invocation(path) -> dict:
    path = Path(path).absolute()
    inventory = load_inventory(path)
    active = owner_active(path, inventory)
    resources = [{"id": entry["id"], "kind": entry["kind"], "role": entry.get("role"),
                  "classification": _classify(entry, active), "recorded_state": entry["state"],
                  "identity": entry["identity"], "detail": entry.get("detail")}
                 for entry in inventory["resources"]]
    recovery = path / "recovery.jsonl"
    return {"invocation": str(path), "task": inventory["task"], "state": inventory["state"],
            "owner_active": active, "detached": inventory.get("detached", False),
            "outcome": inventory.get("outcome"), "cleanup": inventory.get("cleanup"),
            "tmp_root": inventory.get("tmp_root"), "references": retained_references(inventory),
            "resources": resources,
            "recovery_entries": len(recovery.read_text().splitlines()) if recovery.is_file() else 0}


def inspect_task(task: str, env=None) -> list[dict]:
    root = invocations_root(task, env)
    summaries = []
    if not root.is_dir():
        return summaries
    for path in sorted(root.iterdir()):
        if not path.is_dir() or path.is_symlink():
            continue
        try:
            detail = inspect_invocation(path)
        except Refusal as exc:
            summaries.append({"invocation": str(path), "classification": "corrupt", "detail": str(exc)})
            continue
        counts = {}
        for resource in detail["resources"]:
            counts[resource["classification"]] = counts.get(resource["classification"], 0) + 1
        summaries.append({"invocation": detail["invocation"], "state": detail["state"],
                          "owner_active": detail["owner_active"], "detached": detail["detached"],
                          "resources": counts})
    return summaries


def recover(reference, references=(), grace: float = 5.0) -> tuple[bool, list[dict]]:
    """Recover one named invocation whose owner is no longer active."""
    path = Path(reference)
    if not path.is_absolute() or path.is_symlink() or not path.is_dir():
        raise Refusal("recovery requires the absolute invocation directory, not a symlink")
    inventory = load_inventory(path)
    if inventory["host"].get("machine") != host_identity()["machine"]:
        raise Refusal("invocation was recorded on another host")
    if inventory["owner"].get("uid") != os.getuid():
        raise Refusal("invocation is owned by another user")
    lock = os.open(path / "owner.lock", os.O_RDWR | os.O_CLOEXEC)
    try:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise Refusal("invocation owner is active; recovery refused") from exc
        if _owner_process_running(inventory):
            raise Refusal("invocation owner process is still running; recovery refused")
        invocation = Invocation(path, inventory, None)
        invocation.refresh()
        # Recorded references protect retained evidence even when the
        # operator repeats none on the command line.
        references = retained_references(invocation.inventory, references)
        actions = []

        def log(entry, action, detail):
            record = {"at": time.time(), "resource": entry["id"], "kind": entry["kind"],
                      "action": action, "identity": entry["identity"], "detail": detail,
                      "recovered_by": {"pid": os.getpid(), "uid": os.getuid()}}
            append_jsonl(path / "recovery.jsonl", record)
            actions.append(record)

        ordered = sorted(enumerate(invocation.inventory["resources"]),
                         key=lambda item: (TEARDOWN_ORDER.get(item[1]["kind"], 99), -item[0]))
        for _, entry in ordered:
            current = invocation.resource(entry["id"])
            if current["state"] in ("removed", "absent"):
                continue
            if current["state"] in ("pending", "ambiguous"):
                log(current, "refused", f"{current['state']} resource has no sealed identity; operator handling required")
                continue
            if current["kind"] not in RESOURCE_KINDS:
                log(current, "refused", "unknown resource kind")
                continue
            try:
                state, detail = teardown_resource(invocation, current, references=references, grace=grace)
            except (Refusal, ResourceError, OSError) as exc:
                log(current, "refused" if isinstance(exc, Refusal) else "failed", str(exc))
                invocation._update_resource(current["id"], detail="recovery: " + str(exc))
                continue
            invocation.mark(current["id"], state, "recovery: " + detail)
            log(current, state, detail)
        unfinished = [entry for entry in invocation.inventory["resources"] if entry["state"] not in ("removed", "absent")]

        def change(value):
            value["state"] = "recovery-incomplete" if unfinished else "recovered"
            value["recovered_at"] = time.time()
        invocation._mutate(change)
        return not unfinished, actions
    finally:
        os.close(lock)


# ---------------------------------------------------------------------------
# Command line

def launch(args) -> int:
    command = args.command
    task = args.task or os.environ.get(TASK_ENV) or "manual-validation"
    checkout = Path(args.checkout).resolve() if args.checkout else ROOT
    invocation = Invocation.enter(task, checkout, command, {"role": args.role})
    stop_reason = None

    def handle(signum, _frame):
        nonlocal stop_reason
        stop_reason = signum

    previous = {signum: signal.signal(signum, handle) for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
    env = dict(os.environ)
    env.update(invocation.managed_env())
    status = 1
    supervised = None
    outcome = "failure"
    try:
        supervised = start_process(invocation, command, env=env, cwd=os.getcwd(), role=args.role)
        parent = os.getppid()
        deadline = time.monotonic() + args.timeout if args.timeout else None
        while True:
            returncode = supervised.exit_status()
            if returncode is not None:
                status = returncode if returncode >= 0 else 128 - returncode
                outcome = "success" if returncode == 0 else "failure"
                break
            if stop_reason is not None:
                status, outcome = 128 + stop_reason, "interrupted"
                break
            if os.getppid() != parent:
                status, outcome = 129, "parent-exited"
                break
            if deadline is not None and time.monotonic() >= deadline:
                status, outcome = 124, "timeout"
                print(f"validation launch timed out after {args.timeout}s", file=sys.stderr)
                break
            time.sleep(0.1)
    finally:
        failures = []
        if supervised is not None:
            ok, detail = supervised.stop(grace=args.grace)
            if not ok:
                failures.append(detail)
        if invocation.owner:
            failures += invocation.finish(outcome=outcome)
        else:
            failures += invocation.cleanup(rids=invocation.registered)
        for signum, handler in previous.items():
            signal.signal(signum, handler)
    for failure in failures:
        print("validation cleanup failed: " + failure, file=sys.stderr)
    if failures and status == 0:
        status = 2
    return status


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    actions = parser.add_subparsers(dest="action", required=True)
    inspect = actions.add_parser("inspect", help="report invocation resources without mutation")
    target = inspect.add_mutually_exclusive_group(required=True)
    target.add_argument("--invocation")
    target.add_argument("--task")
    recover_parser = actions.add_parser("recover", help="recover one named abandoned invocation")
    recover_parser.add_argument("--invocation", required=True)
    recover_parser.add_argument("--reference", action="append", default=[],
                                help="additional retained evidence path that recovery must not remove; "
                                     "references recorded in the inventory always apply")
    recover_parser.add_argument("--grace", type=float, default=5.0)
    launch_parser = actions.add_parser("launch", help="run a command in a sealed, supervised process group")
    launch_parser.add_argument("--task")
    launch_parser.add_argument("--role", default="command")
    launch_parser.add_argument("--checkout")
    launch_parser.add_argument("--timeout", type=float, default=0)
    launch_parser.add_argument("--grace", type=float, default=5.0)
    launch_parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    if args.action == "launch":
        if args.command[:1] == ["--"]:
            args.command = args.command[1:]
        if not args.command:
            launch_parser.error("launch requires a command after --")
    return args


def main(argv=None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    if argv[:1] == ["_exec"]:
        return _exec_trampoline(argv[1:])
    args = parse_args(argv)
    try:
        if args.action == "inspect":
            value = inspect_invocation(args.invocation) if args.invocation else inspect_task(args.task)
            print(json.dumps(value, indent=1, sort_keys=True))
            return 0
        if args.action == "recover":
            complete, actions = recover(args.invocation, args.reference, args.grace)
            for action in actions:
                print(f"{action['action']}: {action['kind']} {action['resource']}: {action['detail']}")
            if not complete:
                print("recovery incomplete; remaining resources require operator handling", file=sys.stderr)
                return 2
            print("recovery complete")
            return 0
        return launch(args)
    except (Refusal, ResourceError, OSError) as exc:
        print(f"validation resources refused: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
