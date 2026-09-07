#!/usr/bin/env python3
"""Smoke a fresh SingleStore database through the release binaries and local API.

CONVEYOR_DATABASE_URL must name a fresh database. No real LLM or forge service
is used. Output contains timings and HTTP results, never database credentials.
"""
import argparse
import http.server
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


class TitleFixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        body = json.dumps({"model": "admission-fixture", "output": [
            {"type": "message", "content": [{"type": "output_text", "text": "Verify SingleStore admission storage"}]}],
            "usage": {"input_tokens": 1, "output_tokens": 1}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--bin-dir", type=Path, default=Path("bin"))
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    database_url = os.environ.get("CONVEYOR_DATABASE_URL", "")
    if not database_url:
        parser.error("CONVEYOR_DATABASE_URL must name a fresh SingleStore database")
    output = args.output.resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    binaries = args.bin_dir.resolve()
    token = secrets.token_urlsafe(32)
    environment = {k: v for k, v in os.environ.items() if not k.startswith("CONVEYOR_")}
    environment.update({"CONVEYOR_DATABASE_URL": database_url,
                        "CONVEYOR_API_TOKEN": token,
                        "CONVEYOR_LLM_API_KEY": "local-title-fixture",
                        "CONVEYOR_ENV_FILE": str(output / "empty.env")})
    (output / "empty.env").write_text("")
    provider = http.server.ThreadingHTTPServer(("127.0.0.1", 0), TitleFixture)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    environment["CONVEYOR_LLM_BASE_URL"] = "http://127.0.0.1:" + str(provider.server_port)
    started = time.monotonic()
    timings = {}
    responses = []
    config = output / "conveyor.yaml"
    daemon = None
    try:
        before = time.monotonic()
        init = subprocess.run([str(binaries / "conveyor"), "init", "--config", str(config)],
                              input="Admission fixture\nOperator\nowner@example.test\nbootstrap\nBootstrap\nbootstrap\nhttps://example.test/bootstrap\nmain\n",
                              env=environment, text=True, capture_output=True, timeout=60)
        if init.returncode:
            raise RuntimeError("conveyor init failed: " + init.stderr.replace(database_url, "[DATABASE URL]"))
        timings["init_seconds"] = round(time.monotonic() - before, 3)
        # The sign-in link printed by init is deliberately not recorded.
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        address = "http://127.0.0.1:" + str(port)
        before = time.monotonic()
        with (output / "daemon.log").open("w") as log:
            daemon = subprocess.Popen([str(binaries / "conveyord"), "-config", str(config),
                                       "-addr", "127.0.0.1:" + str(port), "-poll-github", "0s"],
                                      env=environment, stdout=log, stderr=log)
            deadline = time.monotonic() + 60
            while True:
                if daemon.poll() is not None:
                    raise RuntimeError("daemon exited before readiness")
                try:
                    with urllib.request.urlopen(address + "/healthz", timeout=1) as response:
                        if response.status == 200:
                            break
                except (urllib.error.URLError, TimeoutError):
                    pass
                if time.monotonic() >= deadline:
                    raise RuntimeError("daemon readiness timed out")
                time.sleep(0.05)
            timings["startup_seconds"] = round(time.monotonic() - before, 3)

            def api(method, path, body=None, headers=None):
                request = urllib.request.Request(address + path, method=method,
                    data=None if body is None else json.dumps(body).encode(),
                    headers={"Authorization": "Bearer " + token, "Content-Type": "application/json", **(headers or {})})
                before = time.monotonic()
                try:
                    with urllib.request.urlopen(request, timeout=30) as response:
                        raw = response.read()
                        result = json.loads(raw) if raw else None
                        responses.append({"method": method, "path": path, "status": response.status,
                                          "elapsed_seconds": round(time.monotonic() - before, 3)})
                        return result, {key.lower(): value for key, value in response.headers.items()}
                except urllib.error.HTTPError as error:
                    raise RuntimeError(f"{method} {path}: HTTP {error.code}: {error.read().decode()}") from None

            api("POST", "/v1/workspaces", {"id": "admission", "name": "Admission", "document": {"repos": []}})
            record, headers = api("GET", "/v1/workspaces/admission/config")
            document = record["document"]
            document["repos"] = [{"name": "smoke", "url": "https://example.test/smoke", "base": "main"}]
            api("PUT", "/v1/workspaces/admission/config", {"document": document}, {"If-Match": headers["etag"]})
            suffix = "?workspace_id=admission"
            content = "Storage smoke.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: The operator can read persisted task state.\n  user_story:\n    as_a: operator\n    i_want: to read task state\n    so_that: I can inspect stored work\n  acceptance_criteria:\n    - id: AC-1.1\n      statement: Reading a created task returns its identifier.\n```"
            api("POST", "/v1/requirements" + suffix, {"id": "req-admission", "title": "Admission storage", "content": content})
            api("POST", "/v1/requirements/req-admission/versions/1/confirm" + suffix)
            task, _ = api("POST", "/v1/tasks" + suffix, {"repo": "smoke", "body": "Read persisted task state.", "hold": True, "requirement_ids": ["req-admission"]})
            task_id = task["id"]
            api("POST", "/v1/requirements/req-admission/versions" + suffix, {"content": content})
            api("POST", "/v1/requirements/req-admission/versions/2/confirm" + suffix)
            task_read, _ = api("GET", "/v1/tasks/" + task_id + suffix)
            if task_read.get("id") != task_id or not task_read.get("hold"):
                raise RuntimeError("task read did not retain the held task")
            requirement, _ = api("GET", "/v1/requirements/req-admission" + suffix)
            version = requirement.get("current_version", {})
            if version.get("version") != 2 or not version.get("confirmed"):
                raise RuntimeError("requirement read did not retain confirmed revision 2")
            lineage, _ = api("GET", "/v1/lineage/task/" + task_id + suffix)
            node_ids = {node["id"] for node in lineage.get("nodes", [])}
            if task_id not in node_ids or "req-admission" not in node_ids or not lineage.get("links"):
                raise RuntimeError("lineage did not connect the task and requirement")
            activity, _ = api("GET", "/v1/activity" + suffix)
            if not any(item.get("task", {}).get("id") == task_id for item in activity):
                raise RuntimeError("workspace activity omitted the task")
            task_activity, _ = api("GET", "/v1/tasks/" + task_id + "/activity" + suffix)
            if task_activity.get("task", {}).get("id") != task_id or not any(
                    event.get("kind") == "task.created" for event in task_activity.get("events", [])):
                raise RuntimeError("task activity omitted the creation event")
            issued = subprocess.run([str(binaries / "conveyor"), "user", "issue-link", "owner@example.test"],
                                    env=environment, text=True, capture_output=True, timeout=30)
            if issued.returncode or "sign" not in issued.stdout.lower():
                raise RuntimeError("conveyor user issue-link failed")
            timings["total_seconds"] = round(time.monotonic() - started, 3)
            result = {"task_id": task_id, "workspace": "admission", "timings": timings, "requests": responses,
                      "provider": "local deterministic title fixture", "task_held": True}
            (output / "results.json").write_text(json.dumps(result, indent=2) + "\n")
            print(json.dumps(result, indent=2))
    finally:
        if daemon is not None and daemon.poll() is None:
            daemon.send_signal(signal.SIGTERM)
            try:
                daemon.wait(timeout=35)
            except subprocess.TimeoutExpired:
                daemon.kill()
                daemon.wait()
        provider.shutdown()


if __name__ == "__main__":
    main()
