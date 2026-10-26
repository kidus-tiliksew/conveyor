import datetime
import json
import os
import sys
import time
import urllib.error
import urllib.request

# Relays state_observation evidence items through the runner's local channel
# with a short child-side timeout, as a kit with a 10 s child timeout does.
# argv: item count, per-request timeout seconds, padding bytes per item, and
# seconds to keep running after the last relay.
count, timeout, padding, linger = int(sys.argv[1]), float(sys.argv[2]), int(sys.argv[3]), float(sys.argv[4])
channel = json.loads(os.environ["CONVEYOR_KIT_OPERATIONS"])
log_path = os.path.join(os.environ["CONVEYOR_KIT_ATTEMPT_DIR"], "relay-log.json")
log = []


def relay(index):
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    body = {
        "type": "evidence",
        "key": "item-%d" % index,
        "evidence_type": "state_observation",
        "captured_at": now,
        "payload": {"target": "fixture", "method": "read", "captured_at": now, "value": {"index": index, "padding": "x" * padding}},
    }
    request = urllib.request.Request(
        channel["url"],
        json.dumps(body).encode(),
        {"Content-Type": "application/json", "X-Conveyor-Kit-Nonce": channel["nonce"]},
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, json.load(response).get("state")
    except urllib.error.HTTPError as error:
        return error.code, None


status = 0
for index in range(1, count + 1):
    code, state = relay(index)
    log.append({"item": index, "status": code, "state": state})
    if code != 200 or state != "recorded":
        status = 3
with open(log_path, "w") as handle:
    json.dump(log, handle)
time.sleep(linger)
sys.exit(status)
