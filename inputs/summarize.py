"""Summarize a Playwright JSON report: per-test result counts, each failure's
first error line, and the board-map elapsed-time distribution."""
import collections, json, sys

report = json.load(open(sys.argv[1]))
counts = collections.Counter()
times = []
failures = []

def walk(suite):
    for spec in suite.get("specs", []):
        for test in spec["tests"]:
            for result in test["results"]:
                counts[(f"{spec['file']}:{spec['line']}", spec["title"], result["status"])] += 1
                for note in result.get("annotations", []):
                    if note.get("type") == "groups, tasks, and edges after read (ms)":
                        times.append(int(note["description"]))
                if result["status"] != "passed":
                    message = (result.get("error") or {}).get("message", "")
                    failures.append((spec["title"], message.strip().splitlines()[:4]))
    for child in suite.get("suites", []):
        walk(child)

for suite in report["suites"]:
    walk(suite)
print("== summary ==")
for (where, title, status), count in sorted(counts.items()):
    print(f"{count:3d} {status:8s} {where} {title}")
for title, lines in failures:
    print(f"failure: {title}")
    for line in lines:
        print(f"    {line}")
if times:
    ordered = sorted(times)
    median = ordered[len(ordered) // 2]
    print(f"board-map elapsed ms (n={len(ordered)}): {ordered}")
    print(f"board-map min={ordered[0]} median={median} max={ordered[-1]} "
          f"max_share_of_2000ms_budget={ordered[-1] / 2000:.1%} at_or_below_60pct={ordered[-1] <= 1200}")
stats = report["stats"]
print(f"stats: expected={stats['expected']} unexpected={stats['unexpected']} flaky={stats['flaky']} skipped={stats['skipped']}")
