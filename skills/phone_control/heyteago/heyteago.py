#!/usr/bin/env python3
"""Scripted HeyTea GO (com.heyteago) control over adb. No vision, no judgment.

Usage:
  heyteago.py menu      # launch app, go to order tab, ensure pickup mode, verify store, print menu JSON
  heyteago.py status    # report current screen state (mode, store, resting banner)

Reliability model: the Flutter semantics tree is a FLAKY sensor — a dump may return
nothing or only the 5 bottom-nav labels even when the screen is fully rendered.
So every decision polls with a predicate over repeated dumps; no single snapshot
is trusted. Exits nonzero with a message if a verification never passes.
"""
import json
import re
import subprocess
import sys
import time

PKG = "com.heyteago"
NAV = {"首页", "点单", "会员", "百货", "我的"}
TAGS = re.compile(r"^(咖啡因(绿|黄|红)灯|含茶|冷热皆宜|可做热饮|含[^，。]{1,12}(成分)?)$")


def adb(*args):
    r = subprocess.run(["adb", *args], capture_output=True)
    if r.returncode != 0:
        sys.exit(f"adb {' '.join(args)} failed: {r.stderr.decode()}")
    return r.stdout.decode(errors="replace")


def dump_once():
    """One dump attempt -> list of (label, (x1,y1,x2,y2)) in document order."""
    adb("shell", "uiautomator", "dump", "/sdcard/ui.xml")
    xml = adb("exec-out", "cat", "/sdcard/ui.xml")
    nodes = []
    for m in re.finditer(r'<node[^>]*text="([^"]*)"[^>]*content-desc="([^"]*)"[^>]*bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"', xml):
        label = (m.group(1) or m.group(2)).strip()
        if label:
            nodes.append((label, tuple(int(m.group(i)) for i in range(3, 7))))
    return nodes


# Dialogs that may pop over any screen; tap their button to clear them.
# 我知道了 = "got it" on the store-resting hours dialog.
INTERRUPT_BUTTONS = ("我知道了",)


def poll(pred, timeout=20, desc="condition", interval=1.5):
    """Dump repeatedly until pred(nodes) is truthy; return those nodes.
    Known interrupt dialogs are dismissed whenever they appear, on every cycle.
    On timeout, exit with the richest dump seen (for the error message)."""
    deadline = time.time() + timeout
    best = []
    while time.time() < deadline:
        nodes = dump_once()
        hit = next((b for l, b in nodes if l in INTERRUPT_BUTTONS and b != (0, 0, 0, 0)), None)
        if hit:
            tap(hit)
            time.sleep(1)
            continue  # re-observe with the dialog gone before judging pred
        if pred(nodes):
            return nodes
        if len(nodes) > len(best):
            best = nodes
        time.sleep(interval)
    labels = " | ".join(l for l, _ in best[:12]) or "(empty dumps)"
    sys.exit(f"timeout waiting for {desc} after {timeout}s; best dump saw: {labels}")


def find(nodes, label, onscreen=True):
    for l, b in nodes:
        if l == label and (not onscreen or b != (0, 0, 0, 0)):
            return b
    return None


def tap(bounds):
    x, y = (bounds[0] + bounds[2]) // 2, (bounds[1] + bounds[3]) // 2
    adb("shell", "input", "tap", str(x), str(y))


def ensure_awake():
    adb("shell", "input", "keyevent", "KEYCODE_WAKEUP")
    time.sleep(1)
    if "mDreamingLockscreen=true" in adb("shell", "dumpsys", "window"):
        adb("shell", "input", "swipe", "636", "2200", "636", "800", "200")
        time.sleep(1.5)
        if "mDreamingLockscreen=true" in adb("shell", "dumpsys", "window"):
            sys.exit("phone is PIN/pattern-locked — unlock it and rerun")


def pickup_state(nodes):
    """Pickup mode invariant: an onscreen 距离您XXXm node (distance to store).
    Store name: node ending just above it, else its document-order predecessor."""
    for i, (l, b) in enumerate(nodes):
        if l.startswith("距离您") and b != (0, 0, 0, 0):
            store = None
            for pl, pb in nodes:
                if pb != (0, 0, 0, 0) and abs(pb[0] - b[0]) < 40 and 0 <= b[1] - pb[3] < 60:
                    store = pl
            if store is None and i > 0:
                prev = nodes[i - 1][0]
                if prev not in NAV and prev not in ("到店取", "喜外送"):
                    store = prev
            return store, l
    return None, None


def in_pickup(nodes):
    return pickup_state(nodes)[1] is not None


def on_order_screen(nodes):
    return pickup_state(nodes)[0] is not None or find(nodes, "喜外送") is not None


def parse_menu(nodes):
    """Group the label stream into items. Prices arrive as '¥' + number (+ optional '¥N' discount)."""
    labels = [l for l, _ in nodes]
    try:
        start = labels.index("搜索") + 1
    except ValueError:
        start = 0
    items, cur = [], None
    i = start
    while i < len(labels):
        l = labels[i]
        if l == "¥" and i + 1 < len(labels) and cur:
            cur["price"] = labels[i + 1]
            i += 2
            if i < len(labels) and re.fullmatch(r"¥[\d.]+", labels[i]):
                cur["discount_price"] = labels[i].lstrip("¥")
                i += 1
            items.append(cur)
            cur = None
            continue
        if re.fullmatch(r"¥[\d.]+", l) or l.startswith("当前门店") or l in ("我知道了", "一起喝") or l in NAV or "滚动" in l:
            i += 1
            continue
        if cur is None:
            cur = {"name": l, "tags": [], "desc": None}
        elif TAGS.match(l):
            cur["tags"].append(l)
        elif l.startswith("#"):
            cur["note"] = l.lstrip("# ")
        elif cur["desc"] is None and (cur["tags"] or len(l) > 14):
            cur["desc"] = l
        else:
            cur = {"name": l, "tags": [], "desc": None}  # previous label was a category header
        i += 1
    return items


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else "menu"
    if not adb("devices").strip().splitlines()[1:]:
        sys.exit("no device attached")
    adb("shell", "settings", "put", "secure", "accessibility_enabled", "1")
    ensure_awake()

    if cmd == "menu":
        adb("shell", "monkey", "-p", PKG, "-c", "android.intent.category.LAUNCHER", "1")
        nodes = poll(lambda n: any(l in NAV for l, _ in n), timeout=30, desc="bottom nav (app loaded)")

        if not on_order_screen(nodes):
            # Cold start lands on the home tab; navigate explicitly. Retry once.
            for attempt in range(2):
                nav = poll(lambda n: any(l == "点单" and b[1] > 2300 for l, b in n),
                           timeout=10, desc="点单 nav button")
                tap(next(b for l, b in nav if l == "点单" and b[1] > 2300))
                try:
                    nodes = poll(on_order_screen, timeout=12, desc="order screen")
                    break
                except SystemExit:
                    if attempt == 1:
                        raise

        store, dist = pickup_state(nodes)
        if not in_pickup(nodes):  # delivery mode is active; switch to pickup
            toggle = find(nodes, "到店取")
            if toggle is None:
                sys.exit("on order screen but no 到店取 toggle found")
            tap(toggle)
            nodes = poll(in_pickup, timeout=12, desc="pickup mode (distance node)")
            store, dist = pickup_state(nodes)

        # Menu labels live in the same tree; poll until we see priced items.
        nodes = poll(lambda n: sum(1 for l, _ in n if l == "¥") >= 3, timeout=12,
                     desc="menu items with prices")
        s2, d2 = pickup_state(nodes)
        store, dist = (s2 or store), (d2 or dist)
    else:
        nodes = poll(lambda n: len(n) > 5, timeout=15, desc="any app content")
        store, dist = pickup_state(nodes)

    out = {
        "mode": "pickup" if dist else "unknown",
        "store": store,
        "distance": dist,
        "store_resting": any(l.startswith("当前门店已休息") for l, _ in nodes),
    }
    if cmd == "menu":
        out["items"] = parse_menu(nodes)
    print(json.dumps(out, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
