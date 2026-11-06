# Evidence for task 261106-0f62f0

Host: 4 CPUs, 8 GB, go1.24.0 linux/amd64. Load: `tools/under_load.py`, N
`sh -c 'while :; do :; done'` busy loops at default niceness.

| File | Tree | Load | Result |
| --- | --- | --- | --- |
| logs/unfixed-count50-race-128loops.log | 6ef21cde (unfixed), the Exit command | 128 loops | 48 of 50 runs fail, each `outcome="failed" err=exercise relay: failed` |
| logs/fixed-count50-race-128loops.log | c2bb1103 (fixed), the Exit command | 128 loops | `ok`, 50 of 50 pass in 421.6 s |
| logs/diag-128.log | 6ef21cde plus a failure message that prints the child's retained output | 128 loops | 12 of 12 fail; the child's stderr ends `TimeoutError: timed out` inside `urlopen` |
| logs/diag-first.log | same diagnostic build | 32 loops | 10 of 10 pass |
| logs/diag-noload.log | 6ef21cde plus relay timing prints | none | relay total 6.3-8.4 ms |
| logs/diag-128-slow-server.log | same timing build | 128 loops | 4 of 4 fail; relay total 272-420 ms |
| logs/diag-128-instant-server.log | same timing build, `responseDelay` and `responseJitter` zero | 128 loops | 8 of 8 fail; 7 as `failed`, 1 as `cancelled` |
| logs/mut1.log | fixed test plus mutations/mut1-ack-after-upload.diff | none | fails, `outcome="timed_out"` |
| logs/mut2.log | fixed test plus mutations/mut2-spool-lock-during-upload.diff | none | fails, `outcome="timed_out"` |

The diagnostic and timing patches were scratch edits and are not committed.
Other unfixed rates seen while choosing the load, printed to the session and not
retained as files: 64 loops 0 of 8 fail, 80 loops 1 of 12, 96 loops 6 of 8.
