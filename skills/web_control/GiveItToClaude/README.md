# GiveItToClaude

One-click "save this page for Claude". Human browses normally (no automation fingerprint);
clicking the toolbar button snapshots the page's live DOM (post-JS, i.e. rendered grids included)
to `~/Downloads/GiveItToClaude/<timestamp>_<title>.html` with the source URL in a leading comment.
Claude ingests from that inbox.

## Install (once)

1. Chrome → `chrome://extensions`
2. Toggle **Developer mode** (top right)
3. **Load unpacked** → select this directory (`web_control_skills/GiveItToClaude`)
4. Pin the extension (puzzle icon → pin) so the button is visible.

## Use

Get the page into the state you want (run the query, pass the captcha), then click the
GiveItToClaude button. Badge shows OK on success, ERR on failure. Each click saves a new
uniquified file; nothing is overwritten.

## Notes

- Chrome extensions can only write under `~/Downloads/`, hence the inbox location.
- Saves the main document only (iframes excluded). Fine for data grids; not for iframe-embedded content.
- `chrome://` and Web Store pages can't be captured (Chrome blocks script injection there).
