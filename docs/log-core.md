# The log core

Conveyor's durable queue is an append-only event log. The relational schema
still holds every entity; the log holds jobs. This page describes the log
contract, the PostgreSQL driver, and the queue that runs on them.

## The log

`internal/eventlog` is the contract. A log is partitioned by workspace, and
inside a partition every entity is one stream, `<type>/<id>`. An append
names the stream version the writer observed, so two writers racing on one
stream have exactly one lose. A partition's positions are total and become
visible in order, so a reader that has seen position N has seen every lower
position.

Two drivers implement the contract. `memlog` is in-process and is the test
double; `pglog` is PostgreSQL. `logtest` is the conformance suite, and
passing it is what "implements the contract" means.

The PostgreSQL driver owns four tables (`event_log`, `event_log_streams`,
`event_log_positions`, and the job index `event_log_job_index`). The startup
migration runner creates them when they are missing. They carry no foreign keys, no CHECK
constraints, and no triggers. Serialization is two row locks in a fixed
order: the partition's position row, then the stream's head row. Holding
the position row until commit is what makes positions visible in order. A
driver for another engine needs only a unique key and a transaction.

## The queue

`internal/queue/logqueue` is the durable queue. Every job is one stream,
`job/<kind>:<key>`, so uniqueness per key is the stream itself: enqueue
appends `job.enqueued` unless the fold says a job is still active. A claim
is an append of `job.claimed` naming the version the worker observed, so
replicas racing for one job have exactly one win, with no lock and no
SKIP LOCKED. Completion, failure with a retry time, snooze (which hands the
attempt back), rescue of a claim older than the threshold, and discard after
the last attempt are events on the same stream. The runtime tails each
workspace's log into an in-memory job index and drains it one job at a time
per kind. A failed catch-up, whether its index load or a tail, is retried on
its own backoff of 5 seconds doubling to one minute, and the next successful
catch-up stops that timer.

Each driver also keeps one row per job stream in `event_log_job_index`,
written in the same transaction as every job append and folded with the
same transition table as the runtime (`internal/eventlog/jobindex`). At
start the runtime reads only the unfinished rows and the events of those
jobs' current generation, then tails from the head it read first, so a
restart does not replay finished jobs. Finished job events stay in
`event_log` and remain readable.

Enqueues from the store run inside the store's own transaction by binding it
to the log driver, so a lifecycle command's rows and its job commit together.
One deadline alarm per workspace runs the order clock at the earliest
recorded deadline, scheduled job, or deadline notice, and never on a fixed
timer. A fire writes nothing to the log: a tick is not a fact, and its
handler is idempotent under concurrent replicas.

The dispatcher sees only `queue.Runtime` and the handler contract: return
nil to complete a job, `queue.Snooze` to reschedule it, any other error to
fail it. A registration may also name a `Discarded` handler. Rescue runs it
once, on the replica whose discard append wins, when it discards a job whose
handler never recorded an outcome: `dispatch_task` then parks the task, and
the review and GitHub issue publications mark their row failed.
`ReconcileQueuedTasks` repairs a queued task without a live job and fails a
running task whose job was discarded after its last attempt.

## Operating it

Nothing beyond the ordinary upgrade. The first start of a binary with the
log core runs migration 121, which moves every job River still held onto its
log stream and drops River's tables; an upgrade with work queued or in
flight loses nothing. Migration 155 (SingleStore 0030) fills the job index by
folding every existing job stream once. At each start the runtime also
rebuilds any index row whose version differs from its stream's head, which
covers jobs that a daemon older than 155 appended after the upgrade. The log tables are part of the database from then on
and are dumped and restored with it.
