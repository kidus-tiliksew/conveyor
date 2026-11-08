import datetime
import json
import os
import socket
import sys
import urllib.error
import urllib.parse
import urllib.request

# Sends relay signals through the runner's local channel one at a time and
# logs each answer. argv: the exit status, then one step per argument written
# as mode:type:key, where mode is "send" or "hangup", type is "evidence" or an
# operation signal type, and key is the evidence key or operation step ID.
#
# A "hangup" step creates the FIFO hangup-gate in the attempt directory, sends
# the request, and waits until the test writes to the gate from inside the
# relay check under test. It then closes its sending side. The runner's HTTP
# server reads that end of stream as a client hangup and cancels the request
# context. The step then reads until the server closes the connection. The
# server closes it only after the relay handler has returned, so the next step
# never overtakes a hung-up request, and no timeout or sleep orders the steps.
status = int(sys.argv[1])
steps = sys.argv[2:]
channel = json.loads(os.environ["CONVEYOR_KIT_OPERATIONS"])
attempt_dir = os.environ["CONVEYOR_KIT_ATTEMPT_DIR"]
log_path = os.path.join(attempt_dir, "relay-log.json")
gate_path = os.path.join(attempt_dir, "hangup-gate")
log = []


def body(kind, key):
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    if kind == "evidence":
        return {
            "type": "evidence",
            "key": key,
            "evidence_type": "state_observation",
            "captured_at": now,
            "payload": {"target": "fixture", "method": "read", "captured_at": now, "value": {"key": key}},
        }
    return {"type": kind, "step_id": key, "captured_at": now}


def send(data):
    request = urllib.request.Request(
        channel["url"],
        data,
        {"Content-Type": "application/json", "X-Conveyor-Kit-Nonce": channel["nonce"]},
    )
    try:
        with urllib.request.urlopen(request) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, None


def hangup(data):
    address = urllib.parse.urlsplit(channel["url"]).netloc
    host, port = address.rsplit(":", 1)
    head = (
        "POST /operations HTTP/1.1\r\n"
        "Host: %s\r\n"
        "Content-Type: application/json\r\n"
        "X-Conveyor-Kit-Nonce: %s\r\n"
        "Content-Length: %d\r\n"
        "\r\n" % (address, channel["nonce"], len(data))
    ).encode()
    if os.path.lexists(gate_path):
        os.remove(gate_path)
    os.mkfifo(gate_path, 0o600)
    with socket.create_connection((host, int(port))) as connection:
        connection.sendall(head + data)
        with open(gate_path, "rb") as gate:
            gate.read(1)
        connection.shutdown(socket.SHUT_WR)
        answer = b""
        while True:
            chunk = connection.recv(65536)
            if not chunk:
                break
            answer += chunk
    line = answer.split(b"\r\n", 1)[0].split()
    return (int(line[1]) if len(line) > 1 else 0), None


for index, step in enumerate(steps, 1):
    mode, kind, key = step.split(":", 2)
    data = json.dumps(body(kind, key)).encode()
    code, answer = (hangup if mode == "hangup" else send)(data)
    entry = {"step": index, "mode": mode, "type": kind, "status": code}
    if isinstance(answer, dict):
        entry["state"] = answer.get("state") or answer.get("State")
        entry["dispatch_authorized"] = answer.get("DispatchAuthorized")
    log.append(entry)
    # Rewrite the whole log after each step, so a child the runner stops
    # still leaves every answer it received.
    with open(log_path + ".tmp", "w") as handle:
        json.dump(log, handle)
    os.replace(log_path + ".tmp", log_path)
sys.exit(status)
