import datetime
import json
import os
import urllib.request

channel = json.loads(os.environ["CONVEYOR_KIT_OPERATIONS"])


def signal(kind):
    request = urllib.request.Request(
        channel["url"],
        json.dumps({
            "type": kind,
            "step_id": "create",
            "captured_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        }).encode(),
        {"Content-Type": "application/json", "X-Conveyor-Kit-Nonce": channel["nonce"]},
    )
    with urllib.request.urlopen(request, timeout=3) as response:
        return json.load(response)


receipt = signal("operation.dispatching")
if not receipt.get("DispatchAuthorized"):
    raise RuntimeError("dispatch was not durably authorized")
with urllib.request.urlopen(
    urllib.request.Request(os.environ["FIXTURE_PROVIDER_URL"], b"create", method="POST"),
    timeout=3,
) as response:
    response.read()
signal("operation.completed")
