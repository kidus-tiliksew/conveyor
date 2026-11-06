#!/usr/bin/env python3
"""Run a command in the foreground and hold the CPUs with N busy loops.

usage: under_load.py N BINARY_NAME -- command [args...]
The command starts first. The loads start when a process named BINARY_NAME
appears, so `go test`'s link step stays outside the load and the whole test
binary run sits inside it. Each load is `sh -c 'while :; do :; done'` at default
niceness. The wrapper prints the load average every 60 s and stops the loads
when the command ends.
"""
import os, subprocess, sys, time

n, name = int(sys.argv[1]), sys.argv[2]
assert sys.argv[3] == "--"
cmd = sys.argv[4:]
print(f"# load: {n} busy-loop processes on nproc={os.cpu_count()}, started when {name} appears", flush=True)
print(f"# cmd: {' '.join(cmd)}", flush=True)
child = subprocess.Popen(cmd)
while child.poll() is None and subprocess.call(["pgrep", "-x", name[:15]], stdout=subprocess.DEVNULL) != 0:
    time.sleep(0.2)
spin = [subprocess.Popen(["sh", "-c", "while :; do :; done"]) for _ in range(n)]
started = time.time()
last = started
while child.poll() is None:
    time.sleep(1)
    if time.time() - last >= 60:
        last = time.time()
        print(f"# t={last-started:.0f}s loadavg: {open('/proc/loadavg').read().strip()}", flush=True)
print(f"# final loadavg: {open('/proc/loadavg').read().strip()}", flush=True)
for p in spin:
    p.kill()
for p in spin:
    p.wait()
print(f"# exit={child.returncode} elapsed_under_load={time.time()-started:.1f}s", flush=True)
sys.exit(child.returncode)
