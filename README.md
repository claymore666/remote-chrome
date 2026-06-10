# remote-chrome — Browser Control MCP Server

Claude acts **in your regular Chrome, with your own profiles** — and every
consequential action is gated by an approval dialog that only *you* can
answer. One portable Go binary + a small sideloaded extension; no admin
rights, no separate browser, no cookie/password access, full audit trail.

Design rationale: [PLAN.md](PLAN.md). Dev log: [NOTES.md](NOTES.md).

```
Claude ── MCP (stdio) ──> remote-chrome (Go) ── WebSocket 127.0.0.1 ──> Chrome extension ── chrome.debugger ──> your tabs
                          guardrails, approvals, audit                 dumb relay, kill switch
```

## The security model in one sentence

**Claude can request anything; nothing executes without a matching grant
(`action group × domain`); only you create grants** — via an approval dialog
(MCP elicitation, or a native zenity/PowerShell dialog as fallback) that the
model can neither render nor answer. Every session starts with an empty
permission matrix. This is the prompt-injection wall: a hostile page can make
Claude *ask*, it cannot make Claude *do*.

| Action group | Tools | Dialog default |
|---|---|---|
| `read` | snapshot, read_page, get_dom, query, screenshot, read_console, read_network | this session |
| `navigate` | navigate, back, forward, reload, wait_for, new/close/select_tab | this session |
| `interact` | click, type, select_option, check, hover, scroll, press_key, handle_dialog | this session |
| `upload` | upload_file | once |
| `eval` | eval_js | once |
| `profile` | targeting a non-default Chrome profile | once |

Grants are per registrable domain (`linkedin.com` covers all its subdomains).
Cross-origin iframes (embedded logins, payment widgets, consent managers) are
first-class: `snapshot` shows their content inline, but **acting inside one
requires a grant for the frame's own domain** — an embedded third-party
widget never inherits the host page's grants.
`request_permission` lets Claude batch a task's needs into **one** dialog.
Named permission sets (`save_permission_set` / `load_permission_set`) re-arm
a project's matrix in a single approval. `kill_switch` (or Ctrl-C, or
clicking the extension's toolbar icon) severs everything instantly.

## Install

### 1. Build

```sh
make build        # -> bin/remote-chrome, extension/dist/
```

### 2. Server first run

```sh
bin/remote-chrome setup
```

Prints the state dir, the WebSocket port and the auth token, plus these
steps. Config lives in `~/.remote-chrome/config.toml` (Linux) or
`%LOCALAPPDATA%\remote-chrome\config.toml` (Windows), created with a fresh
256-bit token on first run.

### 3. Extension (per Chrome profile you want Claude to reach)

1. `chrome://extensions` → enable **Developer mode** → **Load unpacked** →
   select the `extension/` folder.
2. Extension **Details → Extension options**: enter the port + token from
   `remote-chrome setup`, and a profile label (`personal`, `work`, …).
3. Optional hardening: after the first connection the server log shows the
   extension's origin; pin it in config.toml:
   `pinned_origins = ["chrome-extension://<id>"]`.

The toolbar icon shows connection state (`on`) and is a **kill switch**:
one click detaches everything and stops reconnecting; click again to re-arm.

### 4. Register with your MCP client

```sh
claude mcp add remote-chrome -- /path/to/bin/remote-chrome
```

(or the equivalent connector entry in Claude Desktop. Clients without
elicitation support get native OS dialogs instead — set `approval = "dialog"`
to force that.)

## Tools

Perception is **snapshot-first**: `snapshot()` returns the accessibility
tree with a stable `uid` per interactive element; interaction tools take
those uids — no CSS-selector guessing. `query`/`get_dom` exist as fallbacks.
Screenshots are JPEG by default (PNG on request). Real input events with
small randomized delays, so hover menus and debounced inputs behave.

`read_page` has two modes: `text` (default — full rendered text) and
`article` — readability-style extraction of the page's main content as
**Markdown** (headings, links, lists, tables and inline code survive;
nav/cookie/footer noise is dropped; falls back to text when the page isn't
article-like). Add `with_screenshot: true` to get the visual rendering of
the page in the same call.

Free (ungated) tools: `list_tabs`, `list_profiles`, `get_url`,
`list_permissions`, `remove_permission`, `reset_permissions`,
`list_permission_sets`, `diagnostics`, `kill_switch`.

## Config reference (`config.toml`)

```toml
port = 49531                  # picked free + persisted on first run
token = "<hex>"               # shared with the extensions
pinned_origins = []           # restrict to specific extension ids
default_profile = ""          # profile used when tools omit `profile`
deny_navigation = []          # domains navigation is always refused for
approval = "auto"             # auto | elicit | dialog
permission_set = "default"    # where "save to set" approvals are stored
```

Audit log: `~/.remote-chrome/audit.jsonl` — every navigation, interaction,
approval decision, grant and profile target, one JSON object per line.

## Development

```sh
make test       # unit + module tests (no Chrome)
make uat-chrome # once: fetch Chrome for Testing
make test-uat   # end-to-end: real extension in real headless Chrome
make lint       # gofmt, go vet, tsc --noEmit
bin/remote-chrome --verbose   # dumps every relayed CDP command to stderr
```

After pulling changes, run `make build` and then update **both halves**:
restart the MCP server (new binary) and reload the unpacked extension in
`chrome://extensions`. The server refuses extensions built for a different
wire-protocol version — and you cannot miss it: the extension's toolbar
badge turns into a red **upd**, the icon tooltip explains the fix, and a
desktop notification fires; all of it clears on the next successful
connect. `diagnostics` additionally lists refused connection attempts with
the reason.

The UAT suite builds a self-configuring extension variant
(`__TEST_CONFIG__` esbuild define), launches headless Chrome with it, and
drives the full MCP tool surface against a local fixture page — including
approval flows, dialog auto-handling and the kill switch. Note: Google-branded
Chrome ignores `--load-extension` since 137, hence Chrome for Testing for the
suite (manual sideloading in your real Chrome is unaffected).

## Known limitations (v1)

- `chrome.debugger` cannot attach to `chrome://` pages, the Web Store, or
  other extensions' pages; navigation is restricted to http/https/about.
- Downloads are not implemented yet (the `download` action group is
  reserved); `Browser.setDownloadBehavior` is restricted for extensions.
- Console/network capture starts when first read for a tab (CDP domains are
  enabled lazily).
- Chrome shows its "is debugging this browser" banner while Claude is
  attached — by design; idle tabs are detached after ~2 minutes so it clears.
