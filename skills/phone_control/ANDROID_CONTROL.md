# Android app control over adb — transferable learnings

From building the HeyTea GO skill (2026-08-30/31, OnePlus PLU110, 1272x2772). App-specific maps live in each skill dir.

## Toolkit

- Screenshot: `adb exec-out screencap -p > f.png` (readable directly by Claude).
- UI tree: `adb shell uiautomator dump /sdcard/ui.xml` + `adb exec-out cat /sdcard/ui.xml`.
- Input: `adb shell input tap X Y` / `swipe` / `text` / `keyevent`.
- Launch: `adb shell monkey -p <pkg> -c android.intent.category.LAUNCHER 1`. Find pkg: `pm list packages | grep …`.
- Focused activity: `dumpsys window | grep mCurrentFocus`. Lock state: `dumpsys window | grep mDreamingLockscreen`.

## The Flutter semantics problem (the big one)

Flutter apps render to one TextureView — uiautomator sees nothing. Flutter publishes an accessibility
semantics tree only while it believes an accessibility client is connected.

- `settings put secure accessibility_enabled 1` alone works briefly, then the tree FLAPS: dumps alternate
  full / partial (nav only) / empty while the screen renders fine. Toggling it off/on does NOT revive it.
- Durable fix: enable a persistent, non-touch-intercepting accessibility service, then COLD-RESTART the app:
  ```
  adb shell settings put secure enabled_accessibility_services \
    com.android.systemui.accessibility.accessibilitymenu/com.android.systemui.accessibility.accessibilitymenu.AccessibilityMenuService
  adb shell settings put secure accessibility_enabled 1
  adb shell am force-stop <pkg> && <launch>
  ```
  (Accessibility Menu is AOSP, adds a floating button, does not hijack taps. Never use TalkBack — explore-by-touch breaks `input tap`.)
  Verify with `dumpsys accessibility` → "Bound services" non-empty, then 3+ consecutive dumps with stable node counts.
- Offscreen elements appear in the tree with bounds `[0,0][0,0]` — whole lists readable without scrolling.
- Labels are usually in `content-desc`, not `text`. Attributes like `selected`/`checked` are always false;
  visual state (active tab etc.) must be inferred from CONTENT invariants (e.g. a node only that mode shows).

## Reliability rules for scripts

1. **Dumps are a flaky sensor.** Never decide from one snapshot. Poll with a predicate; keep the richest
   dump seen for the timeout error message. Reuse the nodes that satisfied the predicate — don't re-dump.
2. **Interrupt handlers in the poll loop.** Known dialogs (store-closed, promos) can pop over any screen at
   any time; check-and-dismiss on every observation cycle, not at one fixed step.
3. **Tap by label bounds from the dump**, never hardcoded coordinates (except device-scoped fallbacks noted in the skill).
4. **Never assume the start screen.** Cold start ≠ warm resume (different landing tabs). Wake + unlock first
   (`keyevent KEYCODE_WAKEUP`, swipe up; bail with a clear message if PIN-locked). Navigate explicitly every run.
5. **Verify each transition by a content anchor** unique to the destination screen before the next action.
6. **Trace everything.** The script must write a per-run trace dir: timestamped step log, each dump's node
   count + first labels, every tap taken, and on failure a screenshot + raw XML. A failed scripted run must
   be debuggable from its artifacts alone — otherwise every failure costs an interactive re-investigation.
7. Real-money flows: script drives up to the order-summary screen, then stops for the human.

## Methodology (how to build a new app skill)

Freeform first, script second. Drive the flow interactively using the SAME primitives the script will use,
verifying every step live (screenshot + dump) — then freeze the proven sequence into the script. Do not
write the script early and iterate by run-whole-thing → fail → guess: each interactive step is cheap, each
blind scripted run is slow and opaque. Adversarial-test the final script from worst-case state (phone asleep,
app killed) and treat every failure as a missing rule to encode here.
