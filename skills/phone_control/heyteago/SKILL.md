# HeyTea GO (喜茶GO) phone control

Drive the HeyTea GO Android app over adb. Verified 2026-08-30/31 on OnePlus PLU110, screen 1272x2772.
General Android-control lessons live in ../ANDROID_CONTROL.md — read that first.

## Status

- Freeform flow proven end-to-end: launch → 点单 tab → pickup mode → store verified → menu parsed (149-node dump).
- `heyteago.py` exists and encodes the flow but predates two fixes it still needs:
  1. It relies on the flaky `accessibility_enabled=1` hack; it must instead require/enable the
     persistent Accessibility Menu service (see ANDROID_CONTROL.md) and cold-restart the app after.
  2. No debug trace. Add a per-run trace dir (step log, dump summaries, screenshot+XML on failure)
     so failed runs are debuggable from artifacts instead of rerunning interactively.
- Untested: store switching, add-to-cart, checkout. Always stop before payment and show the user.

## Key facts

- Package: `com.heyteago`. Launch: `adb shell monkey -p com.heyteago -c android.intent.category.LAUNCHER 1`.
- Flutter app inside a Tencent mini-app container (`WxaContainerActivity`). Plain uiautomator sees one TextureView.
- Semantics tree only stays alive with a persistent accessibility service enabled BEFORE the app starts
  (cold restart after enabling). With it: 5+ consecutive identical dumps, stable. Without: dumps flap
  between full / nav-only (5 nodes) / empty, even while the screen renders fine.
- Labels are in `content-desc` (text attr usually empty). Offscreen items appear with bounds `[0,0][0,0]`
  — the full category menu is readable without scrolling.
- Cold start lands on the 首页 home tab; warm resume restores the last tab. Always navigate explicitly:
  tap the 点单 nav label (bounds y>2300; nav bar y≈2495 on this device).

## Order-screen map (点单 tab)

- Mode toggle top-left: 到店取 (pickup ~[49,182][231,252]) | 喜外送 (delivery ~[315,185][476,248]).
  Selection is visual only — `selected` attr is always false in the dump. Scriptable invariant instead:
  pickup mode ⇔ an onscreen `距离您XXXm` node; store name is the node just above it.
- Left rail categories: 时令上新 / 推荐榜 / 茶特调/茗茶 / 植物茶/鲜果茶 / 苦巧/抹茶/波波茶 / 灵感茶点 / 经典/小料.
- Item cards: name, tag chips (咖啡因绿灯/黄灯/红灯, 含茶, 冷热皆宜…), description, then `¥` + number nodes;
  a following bare `¥17`-style node is a discount price. `#`-prefixed labels are personal-recommendation notes.
- Bottom nav: 首页 | 点单 | 会员 | 百货 | 我的.
- Store-resting dialog (`当前门店已休息`, hours 周一至周日 10:00-22:00) pops over any screen when closed;
  dismiss button `我知道了`. Handle as an interrupt on every observation cycle, not at one fixed step.
