# Development notes (working log)

Implementation of PLAN.md. This file tracks decisions, deviations and progress.
Newest entries at the bottom of each section.

## Decisions / deviations from PLAN.md

- **Dropped `chromedp/cdproto`**: it forces go ≥1.26 and we only tunnel raw
  JSON CDP through the extension anyway. Untyped `json.RawMessage` params are
  simpler and keep go.mod on 1.25. (PLAN §6 listed cdproto as optional "types
  only".)
- **WebSocket lib**: gorilla/websocket v1.5.3 (nhooyr is now coder/websocket;
  gorilla is fine and ubiquitous).
- **MCP SDK**: modelcontextprotocol/go-sdk v1.6.1. Elicitation API confirmed:
  `session.Elicit(ctx, *mcp.ElicitParams) (*mcp.ElicitResult, error)`;
  capability check via `session.InitializeParams().Capabilities.Elicitation`.
- **`list_tabs` / `list_profiles` are ungated** (orientation metadata only;
  tab titles/URLs but no page content). PLAN puts tabs under `navigate`, but a
  domain-keyed grant doesn't fit a cross-domain listing. Documented in README.
- **Downloads**: not implemented in v1 (PLAN marks them tier-3/optional;
  `Browser.setDownloadBehavior` is restricted under chrome.debugger). The
  `download` action group exists in the matrix for forward-compat.
- **"save to project set" scope** saves into the set named by `--permission-set`
  (default name: `default`).
- **Test seam in extension**: `__TEST_CONFIG__` esbuild define lets UAT builds
  hardcode port/token/profile so headless Chrome connects without the options
  UI. Production build.mjs defines it `null`.
- **Approval fallback** (no elicitation support): zenity on Linux, PowerShell
  message box on Windows, controlled by config `approval = "auto"|"elicit"|"dialog"`.

## Progress checklist

- [x] Environment check (Go 1.25, Node 22, Chrome 149, deps fetchable — see SDK API notes below)
- [x] Extension: manifest, background.ts (relay + kill switch), options page, esbuild
- [x] internal/bridge — WS hub, auth, call/response, events, heartbeat
- [x] internal/config — TOML, token/port generation, setup subcommand support
- [x] internal/audit — JSONL logger
- [x] internal/perms — matrix, eTLD+1, named sets
- [x] internal/approval — elicitation + zenity/PowerShell fallback + Confirm
- [x] internal/browser — manager, events, snapshot/uids, actions, page ops
- [x] internal/server — 32 tools registered + gating (server_test.go is the spec)
- [x] cmd/remote-chrome — main, setup subcommand, signal kill switch
- [x] Unit tests (perms, config, audit, approval, snapshot render)
- [x] Module tests (bridge w/ fake extension over real WS; manager w/ fake CDP; server gating w/ in-memory MCP client + scripted elicitation)
- [x] UAT/regression: real extension in real headless Chrome (test/uat, 15 subtests + binary setup test)
- [x] README, Makefile, .gitignore
- [x] Final: build, vet, gofmt, all tests green (2026-06-10)

- [x] read_page modes (2026-06-10): `article` = readability-lite extraction →
      Markdown via embedded in-page script (internal/browser/extract.js,
      go:embed; scores parents of substantial p/pre/blockquote blocks,
      penalizes link-dense containers, prefers article/main, emits
      headings/links/lists/tables/code; <200 chars ⇒ null ⇒ innerText
      fallback). `with_screenshot` returns text + viewport JPEG in one result.
      Covered in server module tests + 3 UAT subtests (real-Chrome extraction
      incl. noise exclusion).

- [x] OOPIF support (2026-06-11, issue #1): protocol v2 adds sessionId
      routing. The server arms `Target.setAutoAttach` (flatten) in
      EnsurePage — the extension stays a dumb relay, auto-attach is just a
      relayed CDP command. Child sessions tracked per tab
      (attached/detached/targetInfoChanged); snapshot fetches each frame's
      AX tree and grafts it under the owner Iframe node with `f<n>.e<id>`
      uids; DOM ops route to the owning session; iframe JS dialogs are
      answered on their own session (hang prevention). Acting inside a
      frame additionally requires a grant for the frame's own eTLD+1 — an
      embedded widget never inherits the host page's grants. Module tests +
      UAT `oopif_iframe` (real OOPIF: 127.0.0.1 host embeds localhost
      widget; different sites → out of process).

Remaining (post-v1, by PLAN phases): Win11 smoke test (PLAN Phase 0 calls for
it on the real box), record mode (per-session screenshot folder), downloads,
snapshot diffing.

## SDK API cheat sheet (from env-check agent, v1.6.1)

```go
server := mcp.NewServer(&mcp.Implementation{Name, Version}, nil)
mcp.AddTool(server, &mcp.Tool{Name, Description}, handlerFor[In, Out])
// handler: func(ctx, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error)
// session: req.Session; elicitation support:
//   req.Session.InitializeParams().Capabilities.Elicitation != nil
res, err := req.Session.Elicit(ctx, &mcp.ElicitParams{Message, RequestedSchema})
// res.Action: "accept"|"decline"|"cancel"; res.Content map[string]any
server.Run(ctx, &mcp.StdioTransport{})
```
- `go mod tidy` required after adding the SDK (go.sum for transitive deps).
- Handler error => tool error content, not protocol error.

## Gotchas log (hard-won, keep)

- **Google-branded Chrome ignores `--load-extension` since 137.** The UAT
  suite therefore uses Chrome for Testing (`make uat-chrome`, cached under
  `~/.cache/remote-chrome-uat`). Manual sideloading via chrome://extensions in the
  real browser is unaffected.
- **`Page.navigateToHistoryEntry` returns before the navigation commits** —
  and the OLD page still reports `readyState === "complete"`, so a naive
  wait returns instantly with a stale URL. Fix: reset loadFired + 150ms grace
  + WaitLoad (browser/page.go History).
- **`chrome://x` URLs parse as host "x"** — publicsuffix then echoes "x" back
  as the "domain". DomainOf maps every non-http(s) scheme to a `scheme:`
  pseudo-domain instead (caught by perms unit test).
- Two JS dialogs can never be pending at once in real Chrome (the second
  blocks until the first is answered) — tests must deliver dialog events
  sequentially or the reply goroutines race.
- **Never hand Chrome's `--user-data-dir` a `t.TempDir()`**: Chrome helper
  processes keep writing during shutdown and the framework's RemoveAll flakes
  with "directory not empty". The UAT harness owns that dir and removes it
  with retries after the process exits.

- **OOPIF geometry is frame-local.** `DOM.getBoxModel` inside an
  out-of-process iframe is relative to the iframe's own viewport; clicking
  needs the owner `<iframe>`'s box (via `DOM.getFrameOwner` on the parent
  session) added per ancestor (actions.go frameOffset — puppeteer does the
  same). Input events go to the MAIN session regardless: the browser
  hit-tests them into the right frame, including `Input.insertText` after a
  child-session `DOM.focus`.
- Chrome ≥116 keeps MV3 SW alive while WS traffic flows → server pings every 20s.
- Unhandled JS dialogs hang the CDP session → browser manager auto-handles
  `Page.javascriptDialogOpening` (default dismiss) and surfaces it to Claude.
- `chrome.debugger` can't attach to chrome:// pages or the Web Store — tools
  return a clear error for those URLs.
- Stdout is the MCP transport: ALL logging goes to stderr.
