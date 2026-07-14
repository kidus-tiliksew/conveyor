#!/usr/bin/env python3
"""Validate the repository-owned Conveyor Codex plugin without dependencies."""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
PLUGIN = ROOT / "plugins" / "conveyor"
MARKETPLACE = ROOT / ".agents" / "plugins" / "marketplace.json"
SKILL = PLUGIN / "skills" / "conveyor-operator" / "SKILL.md"
SKILL_METADATA = PLUGIN / "skills" / "conveyor-operator" / "agents" / "openai.yaml"


def load_json(path: Path) -> object:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError(f"{path.relative_to(ROOT)}: {exc}") from exc


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def main() -> int:
    manifest = load_json(PLUGIN / ".codex-plugin" / "plugin.json")
    mcp = load_json(PLUGIN / ".mcp.json")
    marketplace = load_json(MARKETPLACE)

    require(isinstance(manifest, dict), "plugin manifest must be an object")
    require(manifest.get("name") == PLUGIN.name, "plugin name must match its directory")
    require(
        bool(re.fullmatch(r"\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?", str(manifest.get("version", "")))),
        "plugin version must be semver",
    )
    require(bool(manifest.get("description")), "plugin description is required")
    require(
        isinstance(manifest.get("author"), dict) and bool(manifest["author"].get("name")),
        "plugin author.name is required",
    )
    require(manifest.get("skills") == "./skills/", "plugin skills path must be ./skills/")
    require(manifest.get("mcpServers") == "./.mcp.json", "plugin must declare .mcp.json")
    interface = manifest.get("interface")
    require(isinstance(interface, dict), "plugin interface metadata is required")
    for field in (
        "displayName",
        "shortDescription",
        "longDescription",
        "developerName",
        "category",
        "defaultPrompt",
    ):
        require(bool(interface.get(field)), f"plugin interface.{field} is required")

    require(isinstance(mcp, dict), "MCP configuration must be an object")
    servers = mcp.get("mcpServers")
    require(isinstance(servers, dict) and len(servers) == 1, "plugin must declare exactly one MCP server")
    server = next(iter(servers.values()))
    require(
        server
        == {
            "type": "http",
            "url": "http://127.0.0.1:8080/mcp",
            "bearer_token_env_var": "CONVEYOR_API_TOKEN",
        },
        "MCP server must use the local endpoint and CONVEYOR_API_TOKEN",
    )

    require(
        isinstance(marketplace, dict) and marketplace.get("name") == "conveyor-local",
        "marketplace name must be conveyor-local",
    )
    entries = marketplace.get("plugins")
    require(isinstance(entries, list) and len(entries) == 1, "marketplace must contain one plugin")
    entry = entries[0]
    require(entry.get("name") == "conveyor", "marketplace plugin name must be conveyor")
    require(
        entry.get("source") == {"source": "local", "path": "./plugins/conveyor"},
        "marketplace source must be repo-relative",
    )
    require(
        entry.get("policy") == {"installation": "AVAILABLE", "authentication": "ON_INSTALL"},
        "marketplace policy is invalid",
    )
    require(bool(entry.get("category")), "marketplace category is required")

    skill_text = SKILL.read_text(encoding="utf-8")
    require(
        skill_text.startswith("---\nname: conveyor-operator\ndescription:"),
        "skill frontmatter is invalid",
    )
    require("[TODO:" not in skill_text, "skill contains an unfinished TODO placeholder")
    for required in (
        "Safe task-worktree setup",
        "conveyor checkout <task-id>",
        "git worktree add -B",
        "git worktree add --track -b",
        "git switch -C",
        "git checkout -B",
        "git push --set-upstream origin",
        "conveyor done <task-id>",
        "never share or mutate the implementation",
        "submit_for_review",
    ):
        require(required in skill_text, f"skill is missing required worktree guidance: {required}")

    metadata_text = SKILL_METADATA.read_text(encoding="utf-8")
    for required in (
        'display_name: "Conveyor Operator"',
        '$conveyor-operator',
        'value: "conveyor-plugin"',
        'url: "http://127.0.0.1:8080/mcp"',
    ):
        require(required in metadata_text, f"skill discovery metadata is missing: {required}")

    machine_path = re.compile(r"(?:/Users/|/home/[A-Za-z0-9._-]+/|[A-Za-z]:\\\\Users\\\\)")
    secret_assignment = re.compile(
        r"(?i)(?:token|secret|password|api[_-]?key)\s*[:=]\s*[\"']?[A-Za-z0-9+/=_-]{20,}"
    )
    for path in sorted(PLUGIN.rglob("*")):
        if not path.is_file():
            continue
        text = path.read_text(encoding="utf-8")
        require(
            not machine_path.search(text),
            f"{path.relative_to(ROOT)} contains a machine-specific path",
        )
        require(
            not secret_assignment.search(text),
            f"{path.relative_to(ROOT)} may contain an embedded secret",
        )

    print("Codex plugin validation passed")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ValueError as exc:
        print(f"plugin validation failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
