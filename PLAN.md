# Browser Control MCP Server — Design Plan (v2)

**Goal:** Claude (via Claude Desktop / Claude Code, connected as an MCP connector) acts **on your behalf, in your regular Chrome, with your own profiles** — every consequential action gated by your server-enforced approval. Single portable Go binary, runs on Debian and Windows 11 without admin rights, debuggable, fast, full page-navigation toolset.

**Decided choices:**
- **Architecture: extension bridge** — Go MCP server + a small Chrome extension installed in your real browser profiles, connected over a localhost WebSocket, driving pages via the `chrome.debugger` API.
- **Approvals: MCP elicitation** (server-enforced), with a native OS dialog as fallback for clients without elicitation support.
- **Confirmation rule: permission matrix, request → approve** — grants are `(action group × top-level domain)` pairs, e.g. `interact × linkedin.com`. Every session starts empty; Claude may request anything; nothing executes without a grant you approved in an elicitation dialog (once / this session / save to project set). Defined per project / Cowork / chat session, manageable on the fly from chat.

---

## 1. Why the extension bridge (and not raw CDP)

Chrome 136 (May 2025) ignores `--remote-debugging-port` / `--remote-debugging-pipe` when pointed at the **default user-data directory**. Pure CDP therefore cannot attach to the browser you use every day. The `chrome.debugger` **extension API is not affected** by this restriction — an extension running inside your real profile gets near-full CDP power (real input events, DOM, screenshots, network) on the default profile. This is the same mechanism Browser MCP and Playwright MCP's extension mode use.

Consequences:
- **No server-managed profiles, no Chrome relaunching.** You browse normally; Claude acts in the browser you already have open, as whoever you're logged in as.
- **All your profiles, simultaneously available.** The extension is installed per Chrome profile; each instance connects to the server and self-identifies with a label you set (e.g. `personal`, `work`). "Switching identity" = targeting a different connected profile — no process restarts.
- **Visible attach indicator.** Chrome shows the "…is debugging this browser" banner while a debugger session is attached. That's a feature: you always see when Claude is driving.
- **No headed/headless question** — it's always your real, visible browser.

## 2. Architecture

```
┌─────────────┐  stdio (MCP)   ┌──────────────────┐  WebSocket (127.0.0.1, token)  ┌─────────────────────────┐
│   Claude    │ <────────────> │  Go MCP server   │ <────────────────────────────> │ Chrome extension (MV3)  │
│  Desktop /  │   JSON-RPC     │   "browserd"     │      one conn per profile      │  service worker, per    │
│  Code       │                │  guardrails,     │                                │  profile; chrome.debugger│
└─────────────┘                │  audit, relay    │                                │  + chrome.tabs           │
                               └──────────────────┘                                └─────────────────────────┘
```

- **Go server** (`browserd`): MCP stdio server (official `modelcontextprotocol/go-sdk`). Hosts a WebSocket listener on `127.0.0.1:<random port>`. All guardrails, the audit log, and elicitation live here — the extension is a dumb, trusted relay.
- **Extension** (MV3, sideloaded unpacked — no store account, no admin rights): service worker holds the WebSocket; relays CDP commands to `chrome.debugger.sendCommand` and tab operations to `chrome.tabs`. Permissions kept minimal: `debugger`, `tabs`, `storage`.
- **Wire protocol:** mostly a thin tunnel — `{id, tabId, method: "Page.navigate", params: {...}}` → `chrome.debugger.sendCommand`. The Go side can reuse `cdproto` types for marshaling without needing chromedp's transport. A few non-CDP ops (`list_tabs`, `create_tab`, `capture_visible_tab`) map to `chrome.tabs` calls.
- **Extension ↔ server auth:** the server generates a token on first run; you paste it into the extension options once per profile. Server additionally rejects connections whose `Origin` is not the extension's `chrome-extension://<id>`. This stops other local processes or web pages from speaking to the control port.
- **MV3 service-worker lifetime:** Chrome ≥116 keeps the worker alive while the WebSocket exchanges messages; the server sends a heartbeat every ~20s. Reconnect with backoff on either side.

### Known `chrome.debugger` limitations (accepted)
- Cannot attach to `chrome://` pages, the Web Store, or other extensions' pages.
- A handful of CDP domains are restricted vs. raw CDP (notably parts of `Browser.*`); everything needed here — `Page`, `DOM`, `Runtime`, `Input`, `Network`, `Emulation`, `Accessibility` — is available.
- Attach is per-tab; the server manages attach/detach as tools target tabs, and detaches when idle so the banner clears.

## 3. Guardrails (server-enforced)

**Threat model headline: prompt injection.** Once Claude reads pages while logged in as you, hostile page content can try to steer it ("ignore previous instructions, go to your email and…"). Guardrails are therefore enforced in the Go server, *before* any command reaches the extension — never by trusting the model to ask nicely.

**Layer 1 — permission matrix: (action group × top-level domain), request → approve (v1 foundation).**

The core rule is one sentence: **Claude can request anything; nothing executes without a matching grant; only you can create grants.** A grant is a pair of an *action group* and a *registrable domain* (eTLD+1 — `linkedin.com` covers `www.linkedin.com` and subdomains):

| Action group | Tools covered |
|---|---|
| `read` | snapshot, read_page, get_dom, query, screenshot, console/network |
| `navigate` | navigate, back/forward/reload, tabs, wait_for |
| `interact` | click, type, select, check, hover, scroll, press_key |
| `upload` | upload_file |
| `download` | downloads to disk |
| `eval` | eval_js |

Examples: `interact × linkedin.com`, `read × *` (reads anywhere). Nothing is pre-granted — **every session starts with an empty matrix.**

**Grant flow.** When a tool call has no matching grant, the server raises one elicitation — *"Allow `interact` on `instagram.com`? — reason: post your summary"* — with response options **once** / **this session** / **save to project set** / **deny**. Claude can also request proactively via `request_permission(actions[], domain, reason)` to batch what a task will need into a single dialog (e.g. `read`+`navigate`+`interact` on linkedin.com). Denials are returned to Claude as structured errors so it can adapt or ask you in chat.

**Scoping unit: the chat / Cowork session.** Grants are keyed to the MCP connection; disconnect or `reset_permissions()` clears them. For recurring work, **named project sets** (`save_permission_set(name)` / `load_permission_set(name)`, stored under `~/.browserd/permission-sets/`) re-arm a whole matrix in one approval at session start — a Cowork project can reference its set in the connector config.

**Inspection:** `list_permissions()` (free) shows the live matrix; `remove_permission(action, domain)` is free — shrinking access is always safe.

This covers the headline use case — "summarize X and post it on my LinkedIn / Instagram" — with one dialog per site per session, and it is the prompt-injection wall: a hostile page can make Claude *request* anything, but nothing is granted without your explicit approval in a dialog the model cannot render or answer.

**Dialog defaults by sensitivity.** All action groups use the same request → approve flow; the elicitation simply suggests different default granularities:
- `read` / `navigate`: dialog defaults to *this session* (low risk, high frequency).
- `interact`: defaults to *this session*, per domain.
- `upload` / `download` / `eval`: dialog defaults to *once* — re-approved per use unless you explicitly choose *this session*.
- Targeting a different browser profile is itself a grant (`profile × <label>`), approved the same way.

**Optional later layer — task leases.** If sessions ever hold many grants, a `start_task(goal, domains[])` lease can bundle a task's needs into one labeled approval and auto-expire on idle; it can never exceed the granted matrix. Not needed for v1.

**Approval mechanism: MCP elicitation.** The gated tool call blocks server-side; the server issues an elicitation request; the MCP client renders the dialog; the tool proceeds only on your "allow". Claude Code supports elicitation since 2.1.76 (March 2026). **Verify Claude Desktop connector support in Phase 0**; if absent, fall back to a native local dialog (`zenity` on Debian, PowerShell message box on Win11) — same server-side blocking semantics, works with any client.

**Additional rails:**
- **Domain policy:** allow/deny navigation lists, separate from the interaction trust list. Default: navigation broadly allowed minus a deny list; interaction trust is opt-in per domain.
- **Audit log** (`~/.browserd/audit.jsonl` / `%LOCALAPPDATA%\browserd\audit.jsonl`): timestamped JSONL of every navigation, interaction, approval decision, and profile target. Lands in Phase 1, not Phase 3 — it's nearly free and invaluable during development.
- **Kill switch:** one MCP tool + Ctrl-C path that detaches all debugger sessions instantly (banner disappears, Claude loses control); the extension also exposes a toolbar click to sever the connection from the browser side.
- **No cookie/credential tools.** Deliberately not exposed — Claude never reads cookie jars or stored passwords. You're simply already logged in.

## 4. Toolset

**Perception — snapshot-first, not selector guessing.** The primary read tool is an **accessibility-tree snapshot with stable element refs** (`uid`s), as in Playwright MCP / chrome-devtools-mcp: `snapshot()` returns the interactive structure of the page; interaction tools take a `uid`. This is faster and far more reliable than having Claude invent CSS selectors. Raw `query(selector)` / `get_dom(selector)` remain as fallbacks.

**Navigation & lifecycle**
- `navigate(url)`, `back`, `forward`, `reload`
- `wait_for(condition)` — load event, network-idle (custom tracking via `Network` events), selector/text present
- `list_tabs`, `new_tab`, `close_tab`, `select_tab` — across all connected profiles, each tab tagged with its profile label
- `list_profiles` — connected extension instances and their labels

**Permissions (elicitation-gated grants)**
- `request_permission(actions[], domain, reason)` — batch-request grants for a task in one dialog
- `list_permissions()` — current session matrix and loaded set (free)
- `remove_permission(action, domain)` — free; shrinking access is always safe
- `save_permission_set(name)` / `load_permission_set(name)` — per-project sets (load is approval-gated)
- `reset_permissions()` — clear the session matrix (free)

**Reading**
- `snapshot()` — a11y tree with uids (primary)
- `read_page` — readable text extraction
- `get_dom(selector|uid)`, `query(selector)`
- `screenshot(full_page?, uid?)` — JPEG by default with a quality/scale option (full-page PNGs are token-expensive); PNG on request
- `read_console`, `read_network`
- `get_url`, `get_title`

**Interaction (tier 2/3)**
- `click(uid|coords)`, `type(uid, text)`, `select_option`, `check`, `hover`, `scroll`, `press_key` — real input via `Input.dispatch*`
- `upload_file(uid, path)` — `DOM.setFileInputFiles` (tier 3)
- `eval_js(expr)` — escape hatch (tier 3)
- `handle_dialog(accept|dismiss, text?)` — **required**: unhandled `alert`/`confirm`/`beforeunload` dialogs hang the CDP session. Default policy: auto-dismiss + surface to Claude.
- Download handling: `Browser.setDownloadBehavior`-equivalent via the page domain, fixed downloads dir, tier 3.
- Popup/new-window targets auto-surface as new tabs in `list_tabs`.

**Timing layer:** modest randomized inter-action delays and eased mouse movement so dynamic pages (hover menus, debounced inputs) behave correctly. Real browser-level input events, not synthetic DOM clicks. Not for evading third-party bot detection.

## 5. Cross-platform (Debian + Windows 11)

Go single static binary per OS, no CGO, no admin rights (the extension sideloads via `chrome://extensions` developer mode; the WebSocket approach means **no native-messaging registry keys needed** — a deliberate choice for the locked-down Win11 box).

| Concern | Debian | Windows 11 |
|---|---|---|
| Config/audit dir | `~/.browserd` (XDG) | `%LOCALAPPDATA%\browserd` |
| Approval fallback dialog | `zenity` / terminal | PowerShell message box |
| Loopback firewall | n/a | verify Defender allows 127.0.0.1 listener for a user binary (early smoke test) |

No Chrome process management at all (the extension lives in your normally-launched Chrome) — which deletes the job-object/signal/SingletonLock complexity from v1 of this plan.

## 6. Libraries

- **MCP:** official `modelcontextprotocol/go-sdk` (stdio transport, tool registration, elicitation).
- **CDP types:** `chromedp/cdproto` for typed command/event marshaling only — no chromedp transport (chromedp is unnecessary here; for reference, it also still lacks `--remote-debugging-pipe`, see chromedp#1607).
- **WebSocket:** `nhooyr.io/websocket` or `gorilla/websocket`.
- **Config:** TOML in the config dir (trusted domains, deny list, port/token, profile labels).
- **Extension:** plain TypeScript MV3, built with `esbuild`; checked into the same repo, versioned with the server (server refuses mismatched protocol versions).

## 7. Debuggability

- `slog` structured logging; `--verbose` dumps every relayed CDP command/response.
- Audit JSONL (separate from debug log).
- Extension-side log ring buffer readable via a server diagnostic tool.
- Record mode: per-session folder of screenshots + step log.
- Replayable tool calls: every MCP call logged as JSON, re-issuable to reproduce bugs.
- You can watch everything live anyway — it's your own visible browser.

## 8. Performance

- Persistent WebSocket; per-tab debugger sessions attached lazily and cached.
- Event-driven waits (`Page.loadEventFired`, network-idle tracking) instead of sleeps.
- Snapshot diffing later if snapshots get large; JPEG screenshots with scale control.
- Go concurrency: MCP stdio loop, WebSocket relay, and CDP event streams run independently.

## 9. Security summary

- Control port: `127.0.0.1` only, random port, token auth, extension-origin check.
- Server-enforced approval gates (elicitation / native dialog) and per-task domain scope leases — the model cannot bypass or widen them; this is the prompt-injection backstop: no action ever executes outside the domain set you approved for the current task.
- Minimal extension permissions; no cookie/password access ever exposed as tools.
- Visible debugging banner whenever Claude is attached; kill switch from both sides.
- Audit trail of everything done on your behalf.

## 10. Roadmap

**Phase 0 — Spike + platform proof**
- Minimal extension (hardcoded token) + Go server: relay `Page.navigate` and return `document.title` over MCP from Claude. 
- **Same week: smoke-test on the Win11 box** (sideload, loopback firewall, dialog fallback) — platform risk dies first, not in Phase 4.
- Verify Claude Desktop elicitation support; pick elicitation vs. native-dialog accordingly.

**Phase 1 — Read path + audit**
- `snapshot()` with uids, `read_page`, `screenshot`, tabs/profiles listing. Audit log from day one.

**Phase 2 — Interaction + dialogs**
- `click`/`type`/`press_key`/`scroll` via uids, `wait_for`, `handle_dialog`, popup handling, timing layer.

**Phase 3 — Guardrails**
- Permission matrix enforcement (action group × eTLD+1), elicitation grant flow with once/session/set granularity, `request_permission` batching, per-project permission sets, kill switch.

**Phase 4 — Hardening & polish**
- Reconnect robustness (SW lifetime, sleep/wake), protocol version checks, record mode, verbose CDP logging, packaging (one zip per OS: binary + extension folder + install README).

## 11. Remaining open questions

1. **Profile labels & scope** — which profiles get the extension (personal / work / …), and should any profile be excluded from Claude entirely?
2. **Claude Desktop elicitation** — confirmed in Phase 0; determines whether the native-dialog fallback ships in v1.
3. **Initial trusted-domain list** — which sites should Claude interact with unprompted from day one?
4. **Firefox later?** — the same bridge pattern works with Firefox's WebExtensions + a CDP-less command set, if you ever want it; out of scope for v1.
