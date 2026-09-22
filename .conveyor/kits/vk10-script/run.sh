#!/bin/sh
set -eu
# req-verification-kits AC-3.1; feature-verification-kit-execution VK-10.2.
exec python3 - <<'PY'
import datetime, json, os, pathlib, time, urllib.request
api = os.environ["CONVEYOR_KIT_BINDING_FIXTURE"]
channel = json.loads(os.environ["CONVEYOR_KIT_OPERATIONS"])
run = pathlib.Path(os.environ["CONVEYOR_KIT_ATTEMPT_DIR"]).name
def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
def evidence(key, kind, payload):
    req = urllib.request.Request(channel["url"], json.dumps(dict(type="evidence", key=key, evidence_type=kind, captured_at=now(), payload=payload)).encode(),
        {"Content-Type": "application/json", "X-Conveyor-Kit-Nonce": channel["nonce"]})
    with urllib.request.urlopen(req, timeout=10) as response:
        assert json.load(response)["state"] == "recorded"
def observe(path):
    start = now()
    with urllib.request.urlopen(api + path, timeout=10) as response:
        status = response.status
        value = json.load(response)
    evidence("api", "api_exchange", dict(method="GET", url=api+path, request_at=start, response_at=now(), response_status=status,
        request_headers={}, response_headers={}, request_summary="Read disposable VK-10 fixture", response_summary=json.dumps(value)))
    evidence("state", "state_observation", dict(target=api+path, method="HTTP GET", captured_at=now(), value=value))
    passed = value.get("observed") is True
    evidence("assertion", "assertion_result", dict(assertion_id="fixture-observed", text="Fixture API observation", expected="observed=true",
        actual=json.dumps(value), outcome="pass" if passed else "fail", supporting=[dict(evidence_id=run+"-api"), dict(evidence_id=run+"-state")]))
    if not passed:
        raise SystemExit(1)
observe("/observe")
PY
