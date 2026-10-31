#!/usr/bin/env python3
"""Render the reviewed allocator SQL for the operator-selected database."""
import os
from pathlib import Path
import sys


def main():
    database = os.environ.get("S2_DATABASE", "")
    if not database:
        print("S2_DATABASE must name the approved database", file=sys.stderr)
        return 2
    source = Path(__file__).resolve().parents[1] / "docs/sql/events-columnstore/04-allocator.sql"
    quoted = "`" + database.replace("`", "``") + "`"
    sys.stdout.write(source.read_text().replace("__DATABASE__", quoted))
    return 0


if __name__ == "__main__":
    sys.exit(main())
