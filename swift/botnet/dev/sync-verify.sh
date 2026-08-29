#!/usr/bin/env bash
# Proves whether a second client, following the server's published sync recipe,
# converges with the server after another client sends a message. This is the
# acceptance test for multi-client sync: it must PASS before any client beyond
# the Mac app is trustworthy.
#
# Recipe under test (today): full fetch once, then poll ?after=<last seen id>.
# Known result: DIVERGES — the feed carries inserts only, so client B watches
# the user turn go by as "awaiting", advances past it, and never sees it settle.
# When the change feed lands, update client_b_sync() to the new recipe and this
# script must go green. Do not weaken the assertion to make it pass.
#
#   ./dev/seed-demo.sh &          # once
#   ./dev/sync-verify.sh          # exit 0 = converged, 1 = diverged
set -euo pipefail

API="${BOTNET_API:-http://127.0.0.1:8731}"

if ! curl -sf -m 3 "$API/v1/bots" >/dev/null; then
    echo "sync-verify: no server at $API — run ./dev/seed-demo.sh first" >&2
    exit 2
fi

python3 - "$API" <<'PY'
import json, sys, time, urllib.request

api = sys.argv[1]

def get(path):
    with urllib.request.urlopen(api + path, timeout=10) as r:
        return json.load(r)

def post(path, body=None):
    data = json.dumps(body).encode() if body is not None else b""
    req = urllib.request.Request(api + path, data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.load(r)

bot = get("/v1/bots")[0]["id"]

# Client B: initial full sync, then the published incremental recipe.
b_view = {m["id"]: m for m in get(f"/v1/bots/{bot}/messages")}
cursor = max(b_view) if b_view else ""

def client_b_sync():
    global cursor
    path = f"/v1/bots/{bot}/messages" + (f"?after={cursor}" if cursor else "")
    for m in get(path):
        b_view[m["id"]] = m
        cursor = m["id"]

# Client A sends a turn.
sent = post(f"/v1/bots/{bot}/messages", {"content": "Reply with exactly: ok"})
uid = sent["id"]
print(f"client A sent {uid} [{sent['status']}]")

# Client B keeps syncing until the server says the turn has settled.
deadline = time.time() + 60
settled = None
while time.time() < deadline:
    client_b_sync()
    server_copy = get(f"/v1/messages/{uid}")
    if server_copy["status"] != "awaiting":
        settled = server_copy
        break
    time.sleep(0.5)

if settled is None:
    print("DIVERGED: turn never settled server-side (model down?)")
    sys.exit(2)

# Give B a few more recipe cycles after settlement, then compare.
for _ in range(6):
    client_b_sync()
    time.sleep(0.5)

failures = []
b_copy = b_view.get(uid)
if b_copy is None:
    failures.append(f"B never saw {uid} at all")
elif b_copy["status"] != settled["status"]:
    failures.append(
        f"B has {uid} as '{b_copy['status']}', server says '{settled['status']}'")

reply = [m for m in get(f"/v1/bots/{bot}/messages") if m["id"] > uid]
for m in reply:
    if m["id"] not in b_view:
        failures.append(f"B never saw reply {m['id']}")

if failures:
    print("DIVERGED — a second client's view is wrong:")
    for f in failures:
        print(f"  {f}")
    sys.exit(1)
print(f"CONVERGED: B agrees with server on {uid} and {len(reply)} follow-up(s)")
PY
