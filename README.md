# Evidence for task 261106-9415aa

Base head for the repro runs: 6ef21cde05a389c5eb13a9c5e8aa294628d88dd5.
Fix head for the fix runs: 43b31372eaac4d1692dffd0f5b838c2ad110b7fc.
Machine: 4 vCPU, 7 GiB, chromium-headless-shell 1228, Playwright 1.61.1.

| File | Head | Load | Result |
| --- | --- | --- | --- |
| repro-1.log | base | stress-ng --cpu 12, 2 workers | 20 of 20 passed |
| repro-2-cpu32-w4.log | base | stress-ng --cpu 32, 4 workers | 60 of 60 passed |
| repro-3-pinned2cpu-16hogs.log | base | 16 hogs pinned to 2 CPUs, 2 workers | 112 of 150 ran, no failure, stopped by the 550 s bound |
| repro-4-nice10-3cpu-24hogs.log | base | 24 hogs on 3 CPUs, nice 10, 4 workers | First render missed its 5 s budget at line 191 (a different failure from load alone); not this flake |
| repro-5-cpu8-vm2-switch2-w3.log | base | stress-ng --cpu 8 --vm 2 --vm-bytes 1G --switch 2, 3 workers | 402 ran with no failure, stopped by the 545 s bound |
| repro-6-amplified-cpu8-vm2-switch2-w2.log | base | Same stress-ng load as repro-5, 2 workers | Failing run: 3 of 8 tests stale after 5000 ms at the unchanged assertion; every stale iteration had the activity read settle first |
| repro-6-amplified.spec.ts.txt | base | - | The temporary spec behind repro-6 (not committed) |
| repro-7-held-read-control.log | base | none | Held detail read: stale for 5000 ms on a paused clock; fresh with no added read after resume or one fake millisecond |
| repro-7-held-read-control.spec.ts.txt | base | - | The temporary spec behind repro-7 (not committed) |
| fix-1-mutation-no-clock-move.log | fix | none | Fix without the clock move: 4 of 4 failed |
| fix-2-repeat-each-50-under-load.log | fix | Same stress-ng load as repro-6, 2 workers | --repeat-each=50: 50 passed |
| fix-3-amplified-under-load.log | fix | Same stress-ng load as repro-6, 2 workers | 8 of 8 passed: 3519 iterations, 11 natural flips, no stale failure |
| fix-3-amplified.spec.ts.txt | fix | - | The temporary spec behind fix-3 (not committed) |
| fix-4-full-spec-file.log | fix | none | Full spec file: 5 passed |

The *.time files hold wall-clock and load-average samples for repro-3 to repro-6.
