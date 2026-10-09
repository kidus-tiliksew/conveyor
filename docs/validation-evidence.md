# Local validation evidence

This document is the Conveyor repository's own procedure for task scratch
space, validation sessions and their retained evidence, and validation
resource ownership. It applies to agents working this repository under a live
claim; the distributed [execution-loop playbook](playbooks/conveyor-work.md)
owns the claim lifecycle and does not ship these repository-specific
instructions.

## Keep scratch data outside checkouts

At the start of every claimed loop, create one task-specific scratch root with
the shape `$XDG_CACHE_HOME/conveyor/<task-id>` (defaulting to
`$HOME/.cache/conveyor/<task-id>`). It must be outside both the shared primary
checkout and the dedicated task worktree, and it must live on a disk-backed
filesystem rather than `tmpfs`, `ramfs`, or another RAM-backed temporary
mount. Resolve the candidate and both checkout paths to canonical absolute
paths before use; stop and report the problem if the candidate is inside a
checkout or its backing filesystem cannot be established as disk-backed.

On Linux, `findmnt -T "$CONVEYOR_TASK_CACHE" -o TARGET,SOURCE,FSTYPE,OPTIONS`
provides the required mount check. On macOS, `df "$CONVEYOR_TASK_CACHE"` names
the device and mount point, the matching `mount` line gives the filesystem type
(`apfs` or `hfs` is disk-backed), `diskutil info` names the device's whole disk
and, for APFS, its physical store, and `hdiutil info` lists attached `ram://`
images, which are RAM disks. Use the platform's equivalent mount or filesystem
inspection on other hosts. Do not silently fall back to `/tmp`.

Create the task root and its separate children with the supported command,
and export every cache variable before any build or test command. Preserve the
operator's `XDG_CACHE_HOME`; derive the task root from it instead of replacing
it:

```sh
task_id='<task-id>'
cache_base="${XDG_CACHE_HOME:-$HOME/.cache}/conveyor"
export CONVEYOR_TASK_CACHE="$cache_base/$task_id"
python3 scripts/validation_evidence.py prepare-cache --task "$task_id" \
  --task-cache "$CONVEYOR_TASK_CACHE"

export GOCACHE="$CONVEYOR_TASK_CACHE/go-build"
export GOTMPDIR="$CONVEYOR_TASK_CACHE/go-tmp"
export TMPDIR="$CONVEYOR_TASK_CACHE/tmp"
export PLAYWRIGHT_BROWSERS_PATH="$CONVEYOR_TASK_CACHE/playwright"
export npm_config_cache="$CONVEYOR_TASK_CACHE/npm"
```

`PLAYWRIGHT_BROWSERS_PATH` controls the browser download used by this
repository's `npx playwright install` command, while `npm_config_cache` routes
npm/npx package cache data. The task cache contains only unreferenced scratch.
Any complete success or failure log, report, or other file relied upon for
acceptance belongs in a durable per-attempt evidence bundle described below;
copying a tail into a transcript does not preserve the original log.

Register the supported cleanup command for normal exit, command failure, and
catchable interruption, and run it explicitly when the claim concludes. Pass
every retained reference named in progress, submission, or review material:

```sh
python3 scripts/validation_evidence.py cleanup --task "$task_id" \
  --task-cache "$CONVEYOR_TASK_CACHE" --reference "$retained_manifest" \
  --reference "$retained_log"
```

The command canonicalizes the cache root, requires the exact current task child
of the selected `conveyor` cache base, checks local ownership, refuses symlinks,
referenced files, and children used by live processes, and removes only the
known disposable children. It never removes the task root, a sibling task
cache, an unknown child, or durable state. Live use is read from `/proc` on
Linux and from libproc and `sysctl` on macOS, where only the invoking user's
processes are inspected. On other systems active ownership cannot be
established and cleanup refuses.

A process whose working directory, root, open files, or cache variables can be
read and point into a child always blocks cleanup. A process that cannot be
inspected blocks it too, unless owner isolation, a creation bound, the Linux
SSH session rule, the Linux user-manager rule, or the Linux manager helper
rule below rules it out. When the
child
or one of its canonical ancestors is a directory owned by the invoking user
with neither group nor other execute permission, such as a mode 0700
`$XDG_CACHE_HOME`, other unprivileged users cannot reach the child, so only the
invoking user's processes are inspected. Privileged processes are outside this
assumption. When no such directory exists, every process is inspected.

`prepare-cache` applies the same task-identity and canonical-containment
checks as cleanup and also refuses a symlinked task root. It creates a missing
task root with mode 0700 and the five disposable children `go-build`, `go-tmp`,
`tmp`, `playwright`, and `npm`, and refuses a symlinked, foreign-owned, or
non-directory child before creating anything. It never changes the
permissions of an existing directory. On Linux it writes an owner-only marker,
`.conveyor-cache.json`, in the task root. The marker records the boot ID and,
for each child the command created, the child's device, inode, and a
boot-relative tick sampled from `/proc/uptime` before the child's `mkdir`. The
tick is therefore a lower bound on the child's creation. No wall-clock time or
filesystem timestamp is involved. When the command's own `mkdir` creates a
child, the new entry replaces any earlier entry for that name. A child the
command did not create, because it already existed or another actor created it
after the preflight, keeps its existing entry or receives none, so repeated
preparation never rewrites or back-dates the entry of a child it did not
create. A marker that is invalid or belongs to another boot is
preserved and receives no new entries; the command reports this. On macOS, and
when the boot ID or tick is unavailable, the command creates the directories
without recording entries.

Cleanup uses a child's recorded tick as a creation bound only when all of these
hold:

- the process backend is Linux `/proc`;
- the marker is a regular file owned by the invoking user, has neither group
  nor other permissions, is not a symlink, and has valid content;
- the marker's boot ID equals the current boot ID;
- the child's current device and inode equal its recorded entry.

With a bound, an uninspectable process with a known start tick strictly before
the recorded tick is disregarded: it started before the child existed. This
covers the invoking user's own non-dumpable processes on an SSH development
host, such as `sshd-session` or the `systemd --user` manager. A process that
started at or after the tick, or whose start is unknown, still blocks cleanup,
and a readable reference always blocks it. In every other case no creation
filter applies, and an uninspectable process that remains after owner
isolation keeps cleanup refusing. A filesystem birth time is never used as a
bound: it is a wall-clock reading, and ordering it against boot-relative process
start ticks would need the history of clock steps between the two events,
which no host records.

On Linux an uninspectable OpenSSH session process of the invoking user does
not block cleanup, whatever its start tick and whether or not a bound exists.
OpenSSH runs each connection's session process (`sshd-session: <user>` or
`sshd-session: <user>@notty`) as the connected user and marks it
non-dumpable, so its cwd, root, descriptors, and environment cannot be read.
A remote client that keeps opening connections therefore always leaves some
session processes newer than the cache. The guard disregards such a process
only when all of these hold, read from `/proc/<pid>/stat` and the ownership of
the `/proc/<pid>` directory, which stay readable for a non-dumpable process:

- the process runs as the invoking user;
- its `stat` command name is exactly `sshd-session` or `sshd`;
- none of its cwd, root, descriptors, or cache variables can be read into the
  child, since any readable reference still blocks;
- its `stat` parent PID names a live process owned by uid 0 whose command name
  is exactly `sshd-session` or `sshd` and which started no later than the
  process;
- a second read of both `stat` files and both owners, after the process's
  entries were inspected, returns the same facts.

A missing, unreadable, malformed, exited, or changed fact leaves the process
ambiguous. A same-user process can rename itself, but it cannot acquire a
root-owned SSH parent, and a session process reparented to init fails the
parent check. Shells and builds started under SSH are separate processes and
are inspected individually. Apart from the user-manager and helper rules
below, no other process class is disregarded, and the macOS backend applies
none of the three rules.

On Linux the invoking user's systemd manager (`systemd --user`) does not block
cleanup either, whatever its start tick, when the local system manager vouches
for it. The manager is non-dumpable and runs for the whole login, so without a
creation bound it would block every cleanup. Its command name, its PID 1
parent, and its `user@<uid>.service/init.scope` cgroup prove nothing on their
own: a same-user process can rename itself `systemd`, make itself
non-dumpable, and be orphaned to PID 1 or a subreaper, and the manager's
cgroup subtree is delegated to the user. The guard therefore disregards such a
process only when all of these hold:

- the process runs as the invoking user, its `stat` is readable, it is not a
  zombie, and its command name is exactly `systemd`;
- `/proc/1` is live, owned by uid 0, and named `systemd`, so a PID namespace
  with another init (a sandbox, for example) never qualifies;
- `/usr/bin/systemctl --system --no-pager --no-ask-password show
  user@<uid>.service -p MainPID -p ExecMainStartTimestampMonotonic -p
  ActiveState`, for the invoking uid, reports each property exactly once,
  `ActiveState=active`, the process's PID as `MainPID`, and a positive
  integer timestamp;
- the process's `stat` start tick S matches the timestamp M (microseconds)
  within one clock tick at `CLK_TCK` H, computed in integers as
  `|S * 1000000 - M * H| <= 1000000`;
- none of its cwd, root, descriptors, or cache variables can be read into the
  child;
- a second query and second reads of both `stat` files and owners, after
  every process in the scan was inspected, return the same facts as the first
  ones, which were taken before the process's entries were inspected.

The query runs that fixed executable directly, with no shell, `PATH` lookup,
user bus, remote host, or privilege, and an environment holding only
`LC_ALL=C`. The executable must be a root-owned executable regular file under
root-owned directories, none of them a symlink or writable by group or
others. Each query has a two-second deadline and a 16 KiB output limit. Any
unavailable, failed, malformed, mismatched, or changed fact leaves the
process ambiguous, and cleanup keeps refusing. On a host that suspended
before the manager started, `/proc` start ticks (which count boot time) and
the monotonic timestamp (which does not) disagree, and the rule refuses.

On Linux the authenticated manager's PAM helper does not block cleanup
either, whatever its start tick. Before systemd executes the manager binary
for `user@<uid>.service`, it forks a child that runs as the user, renames
itself `(sd-pam)`, and waits to close the PAM session at logout. The helper is
non-dumpable and lives as long as the manager, so without this rule every
cleanup on a host with a login session would still refuse. The guard
disregards such a process only when all of these hold:

- the process runs as the invoking user, its `stat` is readable, it is not a
  zombie, and its command name is exactly `(sd-pam)`, parentheses included;
- its `stat` parent PID names the manager that the user-manager rule
  disregarded in the same scan, through that rule's own two queries and second
  reads, so a parent of PID 1, a subreaper, or a different `systemd` never
  qualifies;
- with the manager's start tick S_m, the helper's start tick S_h, and the
  `CLK_TCK` H of the manager's proof, `S_m <= S_h <= S_m + H` in integer
  ticks: the helper started no earlier than the manager and at most one
  second after it;
- none of its cwd, root, descriptors, or cache variables can be read into the
  child;
- a second read of both `stat` files, both owners, and PID 1, after every
  process in the scan was inspected and the manager's second query, returns
  the same facts as the first reads, which were taken before the helper's
  entries were inspected.

Any unavailable, malformed, changed, or out-of-window fact, or an
unauthenticated parent, leaves the process ambiguous, and cleanup keeps
refusing. The helper adds no query. The window is a heuristic, not identity
proof. A same-user process cannot become a child of the authenticated
manager, because an orphan is reparented only to PID 1 or a subreaper, but
the manager forks every user service it starts. A user service started within
one second of the manager's own start that renames itself `(sd-pam)` and
makes itself non-dumpable is therefore disregarded when it exposes no readable
reference. The operator accepted this residual risk: the guard prevents
accidental deletion of a cache in use, and a same-user process can already
delete the cache directly. On the development host observed on 2026-10-09 the
manager started at tick 1168414290, its helper at 1168414292, and the
manager's earliest other child at 1200011223.

Cleanup prints one line per disregarded process to standard output as it
inspects each child, before any later refusal, and then the usual summary:

```text
Disregarded Linux SSH session: pid=666814 parent=666711 command=sshd-session parent_command=sshd-session reason=same-user-uninspectable-with-live-root-owned-ssh-parent
Disregarded Linux user manager: pid=2586874 uid=1000 service=user@1000.service main_pid=2586874 start=1168414290 monotonic_usec=11684142907759 reason=same-user-uninspectable-system-manager-reported-mainpid
Disregarded Linux user manager helper: pid=2586876 parent=2586874 uid=1000 command=(sd-pam) start=1168414292 parent_start=1168414290 clk_tck=100 reason=same-user-uninspectable-sd-pam-child-of-authenticated-user-manager-within-one-second
Removed disposable cache children: go-build, go-tmp, tmp, playwright, npm
```

Each process appears once per invocation, in PID order per child. Save the
command's complete standard output, standard error, and exit status in durable
task state, not in the task cache, so the retained result names every process
the guard did not count.

A child that cleanup removed and a later `prepare-cache` re-created gets a
fresh entry: its new device and inode, and the tick sampled before that
`mkdir`. A multi-round task therefore regains a bound on every claim. The fresh
tick replaces the earlier one even when the filesystem reuses the inode. A
child that `prepare-cache` did not create never gets a fresh bound.

A process killed before cleanup leaves scratch for the next claim to inspect
with the same command.

If a confined sandbox denies the sanctioned external path, first request
write permission scoped only to that exact task cache directory. Only when
that permission is unavailable may the session use a last-resort directory at
the task worktree root named `.codex-cache-<task-id>/` (or the corresponding
`.claude-<purpose>/` or `.grok-<purpose>/` harness form). Never put the fallback
in the shared primary checkout. Before generating anything, require both
`git check-ignore -q <fallback-path>` and an empty
`git status --porcelain --untracked-files=normal`; repeat the status check
after creating a probe file. If Git can see the fallback, remove it and stop
rather than dirtying either checkout. Apply the same guarded cleanup rules to
this fallback when the claim ends.

## Run one validation session and preserve its evidence

For this repository, `make validate` runs the existing `build`, `vet`,
`fmt-check`, and complete `test` aggregate in one Make graph. It installs web
dependencies once with `npm ci` and builds the dashboard once. This is the
supported form of the existing `make build vet fmt-check test` sharing;
separate Make invocations prepare dependencies again. Under `make -j validate`,
vet and the installer wait for the dashboard build because they compile Go
packages that embed it. Standalone targets retain their own prerequisites.
The target adds no cache/stamp shortcut and never accepts an existing bundle
without rebuilding and checking its diff. `make test-validation` exercises the
orchestration, evidence, and resource helpers with real local processes and is
also part of `make test`. `make test-validation-docker` runs the real Docker
and PostgreSQL resource-lifecycle fixtures; `make test-integration` depends on
it, and missing Docker fails it as missing evidence rather than skipping.

The complete ordinary gate still includes Compose isolation, installer checks,
Go tests, dashboard TypeScript compilation, Biome, and Playwright. Explicit
capability-parity typechecking remains in `make test-web`. Run configured
`make test-integration` and `make test-integration-singlestore-ci` separately
when the contract requires them. An unset backend, skipped suite, narrowed
command, failed aggregate, or local reuse never satisfies a mandatory fresh
boundary. Each database run uses disposable isolated fixtures.

Each required database run uses the repository fixture lifecycle. Preparation
checks the configured endpoint, repository Go driver, optional external Docker
network, and configured disk threshold, then creates a unique database ending in
`_test` and writes a task-owned ownership record without a DSN or credential.
The external network is inspected and used as configuration only; validation
never creates, prunes, or removes shared networks, volumes, containers, or
foreign databases. Missing configuration, capacity, client, network/readiness,
or database creation is fixture failure and missing evidence, never a skip.

When a database policy contains the optional `fixture` object, the evidence
helper owns the complete order: prepare, authenticated before snapshot,
unchanged full Make gate once, authenticated after snapshot, then teardown of
only the database named by the unchanged ownership record. The same ordering
applies to command failure and catchable interruption. Snapshot and teardown
failures remain separately recorded; teardown never rewrites the preceding gate
outcome. Phase records, ownership state, complete redacted log, manifest and key
remain together in durable task state. Make's PostgreSQL and SingleStore
integration targets use the same lifecycle when invoked directly and accept an
already-prepared owned fixture from the evidence helper without nesting another
fixture. The PostgreSQL target creates and removes only its own invocation's
sealed container and network; long-lived host containers are never cleanup
candidates.

## Own and recover validation resources

Every managed validation launch records what it owns before it starts work,
so cleanup and recovery never guess. `scripts/validation_resources.py` is the
shared helper behind `validation_fixtures.py`, `validation_evidence.py`, the
Makefile, and the Playwright web server.

- **Inventory.** Each invocation exclusively creates an owner-only directory at
  `$XDG_STATE_HOME/conveyor/<task-id>/invocations/<invocation-id>/` holding
  `inventory.json`, an owner lock, and an append-only `recovery.jsonl`. The
  inventory records the task, checkout, host, owning user, owner PID and birth
  identity, sanitized argv, non-secret configuration, temporary-root decision,
  and each resource. It never stores a DSN, password, token, or environment
  dump. The owner lock is the liveness signal and is released even after
  `SIGKILL`.
- **Sealed identities.** A resource is registered as `pending` before creation
  and sealed before any workload uses it. A process group is sealed by PGID,
  leader PID, and process birth identity through a launch handshake: the child
  waits until its record is durable and never executes when sealing fails. The
  managed PostgreSQL container and its network are created without starting,
  sealed by Docker ID and the `sh.conveyor.validation.invocation` label, then
  started. A database on an external server is sealed with its name and, for
  PostgreSQL, its OID. A temporary child is sealed by path, device, and inode.
- **Binding.** Child processes receive `CONVEYOR_VALIDATION_INVOCATION`. Recursive
  Make, the evidence helper's prepared fixture, and Playwright's Vite server
  register into that same inventory, so one owner performs teardown. The
  validation child boundary passes this variable. A binding joins only an
  active owner of the same user and checkout; an inherited binding from
  another checkout starts a separate owned invocation instead.
- **Retained references.** Before writing anything, the evidence helper
  records its output directory in the inventory's `references` list, whether
  it owns the invocation or joined one. Output inside the configured
  `CONVEYOR_VALIDATION_TMP_ROOT`, the task cache, or any disposable path of the
  bound inventory is refused before creation. Owned teardown and recovery
  always honor recorded references (and an older record's
  `configuration.evidence`) without repeated `--reference` arguments.
- **Owned teardown.** Cleanup runs after success, failure, configured timeout,
  `SIGINT`, and `SIGTERM`, in the order process groups, container, network,
  external databases, temporary paths. Process groups receive bounded `TERM`
  then `KILL`, including descendants that outlive the direct child. Each
  signal follows a check of every remaining member's birth identity or
  invocation binding. macOS withholds the environment of platform binaries
  such as `/bin/sh`, so there the supervisor keeps the exited leader unreaped
  until teardown ends. The held leader PID keeps the group ID from reuse, and
  each same-user member of that session's group is verified without a
  readable binding. Containers and networks are removed by ID only after
  their labels and project match. Nothing runs `docker compose down`,
  `--remove-orphans`, prune, or name-pattern deletion. External PostgreSQL and
  SingleStore servers and external networks are configuration and are never
  stopped, capped, or removed.
- **Separate outcomes.** Cleanup outcome is recorded beside the gate outcome.
  A cleanup failure fails the invocation but never rewrites a recorded gate
  success or failure. Evidence manifests name their invocation inventory and
  a `resource_cleanup` result. `check` and `bind` refuse a record whose owned
  cleanup failed.

Inspect without mutation, then recover one named invocation after an owner
was killed:

```sh
python3 scripts/validation_resources.py inspect --task "$task_id"
python3 scripts/validation_resources.py inspect --invocation "$invocation"
python3 scripts/validation_resources.py recover --invocation "$invocation"
```

Recorded references always apply; add `--reference "$path"` only for retained
material the inventory does not already name.

Inspection reports each resource as `pending`, `active`, `abandoned`,
`completed`, `ambiguous`, or `cleanup-failed`, and lists the recorded retained
references. Recovery refuses while the owner
lock is held or the recorded owner still runs, on another host or user, and
for corrupt or legacy inventories. Immediately before each mutation it
rechecks that resource's identity and refuses a changed process birth or
binding, a changed container or network ID or label, a changed database
incarnation, a symlink or substituted path, a path in use, a path that
contains or is named by a recorded or `--reference` retained reference, a
malformed reference list, and an unknown resource kind. A temporary path
uses the same live-use inspector as cache cleanup, including the Linux SSH
session, user-manager, and manager helper rules; the removal detail or refusal message names
each disregarded process with the same line, and the inventory and
`recovery.jsonl` retain it. Pending and ambiguous entries have no sealed identity and are never
removed. Recovery is
idempotent, appends every action and refusal to `recovery.jsonl`, and never
edits an evidence manifest or log: an interrupted attempt stays incomplete and
nonreusable. Resources created before this inventory existed, such as
historical Vite servers or verification SingleStore containers, have no
sealed identity and remain operator work. No daemon or sweep discovers
cleanup candidates by name, prefix, or age.

Database lifecycle commands name an invocation explicitly:

```sh
make test-db-identity                     # port (auto or pinned) and project form
make test-db-up                           # prints INVOCATION, port, container, URL
make test-db-down INVOCATION="$invocation"  # removes only that invocation's resources
```

Each invocation gets its own project `conveyor-test-<invocation-id>` and a free
loopback port, so concurrent runs in one checkout never share a database.
`CONVEYOR_TEST_POSTGRES_PORT` or `TEST_POSTGRES_PORT` pins the port; an
occupied pin fails rather than attaching to another server. The managed
container defaults to `CONVEYOR_TEST_POSTGRES_MEMORY=2g` with a
`CONVEYOR_TEST_POSTGRES_TMPFS_SIZE=1g` data `tmpfs`. Overrides must be a
positive integer with an `m` or `g` suffix, and the `tmpfs` must stay smaller
than the memory limit because its pages count against it.

Large disposable outputs of managed launches use invocation children of
`CONVEYOR_VALIDATION_TMP_ROOT`, defaulting to `CONVEYOR_TASK_CACHE` and then
`${XDG_CACHE_HOME:-$HOME/.cache}/conveyor/<task-id>`. The children receive
`TMPDIR` and `GOTMPDIR`; `HOME` and `XDG_CACHE_HOME` are unchanged. The root
must be disk-backed: a `tmpfs`, `ramfs`, or unknown filesystem refuses with a
diagnostic naming the override. Set `CONVEYOR_VALIDATION_ALLOW_RAM_TMP=1` only
to accept a RAM-backed root deliberately; the inventory and log record it.
`PLAYWRIGHT_OUTPUT_DIR` and `PLAYWRIGHT_REPORT_DIR` move Playwright's test
output and HTML report. Copy any report relied upon for acceptance into
durable state before its disposable directory is removed.

Run a dashboard dev server for review through the same supervisor so a killed
session cannot leave it running unrecorded:

```sh
cd web && python3 ../scripts/validation_resources.py launch --task "$task_id" \
  --role review-vite -- npm run dev -- --host 127.0.0.1 --strictPort
```

The launcher stops its group when it receives `SIGINT`, `SIGTERM`, or
`SIGHUP`, when its parent exits, or after `--timeout`.

`scripts/validation_evidence.py` records a fresh command with `run`, checks an
existing record with `check`, and associates eligible evidence with the actual
pushed branch using `bind`. It never skips a Make prerequisite or changes a
required gate. Run it from the dedicated worktree. By default, `run` exclusively
creates a unique attempt directory under
`$XDG_STATE_HOME/conveyor/<task-id>/` (default
`$HOME/.local/state/conveyor/<task-id>/`), outside both checkouts and all task
caches. It prints absolute retained manifest and complete-log references plus
the success or failure outcome. Keep the manifest, private integrity key, log,
and head-binding file together after disposable caches are removed. The
manifest distinguishes a nonzero execution from a missing, truncated, or
corrupt log. The key detects accidental corruption; it is not a signature
against an author who can rewrite the bundle.

Before `run`, write a JSON policy describing the actual command and every input
boundary. Schema 1 requires these fields; unknown or omitted fields fail closed:

- `schema`: `1`; `task`: the current task ID; `layer`: `local`, `postgres`, or
  `singlestore`; `command`: the complete argv, starting with `make`.
- `environment`: an explicit list of variable names. The child receives only
  listed variables that are set. Include `PATH` and `HOME`, applicable task-cache
  variables, flags, backend URLs, and every relevant configuration variable.
  An absent variable differs from an empty one. Values are stored only as
  keyed fingerprints; never put credentials in argv or policy prose. The log
  replaces inventoried environment values before retention. Inspect logs for
  any application-specific secret encoding before sharing them.
- `tools`: a map from executable name to its version-probe argv, for example
  `"go": ["go", "version"]`. Include all invoked tools and nested runtimes,
  including `make`, `git`, `python3`, and `sh`. A shell probe may use
  `["sh", "-c", "printf POSIX-shell"]`; the resolved executable is also hashed.
  Probes must be read-only, deterministic, and receive the same environment.
- `external_inputs`: absolute file/directory paths covering inputs outside the
  worktree, including SDK/standard-library files, runtime libraries, loaded
  configuration, resolved dependency trees, and browsers where consumed.
  Inventory the configured HOME files or use a clean dedicated HOME. A version
  string alone does not inventory a runtime or its dependencies.
- `exclude`: an object giving a reason for each excluded output directory.
  Only `bin`, `web/playwright-report`, and `web/test-results` can be excluded,
  and never when tracked or read by the command. All other tracked, untracked,
  ignored, generated, dependency, and fixture files in the worktree are hashed,
  including modes and symlink targets. Do not exclude `node_modules`; installed
  dependencies must be inventoried. Files outside the root reached by symlinks
  require an explicit external input.
- `audit`: `inputs_complete` must be a boolean; `input_rationale` explains the
  reviewed inventory and output exclusions. Set it to `false` for unknown
  inputs: `run` still records fresh execution, but `check`/`bind` refuse reuse. `git_metadata` is `dependent` by
  default in author judgment, or `independent` only after inspecting every
  command and transitive input; `git_rationale` records that inspection.
  Unknown inputs or external state require another run, not an optimistic
  audit declaration. Reviewers assess this policy against the actual code.
- `backend`: `null` for local runs. Database records require an object with
  `isolation: "disposable-per-run"` and `probe`: a read-only argv returning JSON
  with nonempty `identity`, `version`, `configuration`, and unique `instance`
  fields from the configured backend. Probe results are fingerprinted before
  and after execution. This helper records backend evidence but refuses its
  reuse: matching configuration cannot prove unchanged mutable database state.
  Preserve fresh backend results and rerun the isolated target when needed.
- `fixture`: optional and valid only for a `postgres` or `singlestore` layer.
  It names the matching backend, the inventoried source and prepared URL
  variable names, the optional external-network variable, a safe database-name
  prefix, a positive minimum-free-bytes threshold, and an operation timeout.
  It contains no DSN or credential. When present, `run` owns preparation,
  before/after snapshots, and teardown; the Make command still names the full
  unchanged backend gate. The optional boolean `managed_postgres` (postgres
  layer only) makes `run` start the invocation's own PostgreSQL container and
  point the source URL variable at it.

`run --timeout <seconds>` stops the supervised gate after that time and
records a distinct, nonreusable `timeout` outcome. The deadline is checked
while output remains readable. `run` never waits for output end-of-file
before cleanup: when the direct command exits, it stops verified surviving
members that still hold the inherited output and records the direct command's
own exit status. Final output drains are bounded.

For example, after authoring and auditing `policy.json` outside the worktree,
let `run` select the collision-safe attempt directory. Copy the printed attempt
directory into `evidence` for later checks and binding:

```sh
python3 scripts/validation_evidence.py run --policy "$policy"
evidence='<printed manifest parent directory>'
python3 scripts/validation_evidence.py check --policy "$policy" --output "$evidence"
# After committing and pushing the assigned task branch:
python3 scripts/validation_evidence.py bind --policy "$policy" --output "$evidence" \
  --remote origin --branch "conveyor/task-<task-id>"
```

`run` always executes the command, preserves its full exit status, timestamps,
redacted log and digest, and captures input fingerprints before and after.
A successful command whose inputs changed is still recorded as fresh execution,
but cannot be reused. Dependency installation that changes resolved inputs can
therefore make a session ineligible. Do not edit an old manifest to claim that
it tested the final state; rerun the affected validation into a new directory.

`check` refuses missing/corrupt manifests, keys or logs; prior failure;
before/after drift; changed commands, flags, tools, dependencies, files,
environment, configuration, host, worktree, or task; and uncertain Git history.
A refusal exits nonzero: run the affected required target again. No automatic
skip toggle or cross-task/cross-environment cache exists.

`bind` additionally requires a clean worktree/index and queries the remote to
prove the assigned branch was pushed at the current head. It writes a separate
`reused-execution` binding, preserving the original execution time and reason.
A commit can preserve file content while changing Git inputs. `make validate`
and `make build` consume `git describe --tags --always --dirty` through
`VERSION`/`LDFLAGS`; treat their evidence as metadata-dependent. Git/history tests
may also consume repository state. Only an audited metadata-independent command
can carry evidence across ordinary linear commits of identical inputs. Even an
identical-content merge, authored conflict resolution, rebase, reset, missing
reflog, or Git change during execution requires fresh validation. An ordinary
`make fmt-check` is a candidate only after auditing the current Makefile and
resolved gofmt runtime; target names alone do not establish independence.

Local manifests and logs are task-context evidence, not artifacts carrying the
`verification_evidence` role, whose permitted image/recording types remain
unchanged. Record fresh exact-head CI independently. Local equivalence never
makes a prior reviewed-head approval current or replaces independent reviewer
judgment, mandatory done criteria, or either operator gate
(`req-review-gates-evidence` REQ-4/AC-4.1 and REQ-7/AC-7.1;
`component-verification-strategy`; DEC-53).
