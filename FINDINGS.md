# Findings from live install & test session (Cowork, 2026-06-11)

Context: parallel session installed the server + extension on the dev machine,
registered it with Claude Desktop and project `.mcp.json`, and ran a live test
against a real Chrome profile. Code fixes below are ALREADY APPLIED and built
(`bin/remote-chrome`, `extension/dist/`); open items are not.

## Fixed (review + keep)

1. **Elicitation accept was treated as deny** — `internal/server/server.go`,
   `elicit()`. Claude Desktop returns `action: "accept"` with an *empty*
   `decision` field; `ParseDecision("")` failed and the code silently denied.
   Reproduced live: user clicked accept, tool got "permission denied by user".
   Fix: empty decision on accept → `DefaultDecision()`; every approval outcome
   is now slog-logged. Follow-up (2026-06-11, reviewed + completed): the
   original patch was unreachable — the schema's `required` made the SDK
   reject content-less accepts before the server saw them, and the added
   `default` made the SDK's ApplyDefaults panic on nil content (jsonschema-go
   nil-map write). Both dropped from the schema; regression test
   `TestAcceptedApprovalWithEmptyDecisionUsesDefault` covers session + once
   defaults via content-less accepts (garbage decisions are rejected by the
   SDK's enum validation client-side; declines were already covered).

2. **Extension discarded the server's WS close reason** — the server sends
   precise close frames ("bad token", "protocol mismatch", takeover) but the
   extension guessed at causes, blaming the token for a protocol mismatch.
   `background.ts` now surfaces `CloseEvent.reason` verbatim; "connected" badge
   /status only after the server provably accepted the hello (first message or
   3s stable); failed handshakes keep exponential backoff (previously reset to
   1s on every open → 1/s hammering, see 00:28 log spam).

3. **Server close frames enriched** — `internal/bridge/bridge.go`: protocol
   mismatch close reason now includes both versions + fix hint; profile-label
   takeover sends a close reason instead of silently closing.

4. **Extension-side logging** — 300-entry ring log in the SW (persisted in
   `chrome.storage.session`), logs state changes, close code/reason, failed
   CDP/tabs requests, debugger detaches, uncaught errors/rejections. Options
   page renders it live with a copy button.

5. **Options page UX** — live connection status line; profile label prefills
   with the Chrome account email (`chrome.identity.getProfileUserInfo`;
   manifest gained `identity` + `identity.email` permissions).

## Open items (not fixed)

6. **`request_permission` unusable through Cowork/Claude Desktop** — the
   server-side schema is a correct `[]string`, but the schema as seen by the
   client lost `type: array` on `actions`, so the client serializes an array
   as string and server validation rejects it; `actions` is also `required`,
   so the tool cannot be called at all from such clients. Cause appears to be
   client-side schema mangling, NOT our code — but consider a server-side
   workaround (e.g. also accept a comma-separated string).

7. **Protocol-drift failure mode** — extension and binary built at different
   times produced live v1-server/v2-extension mismatch. Consider embedding the
   protocol version in `make build` output and/or a `diagnostics` warning.

8. **Dev-machine gotcha**: shell exports `NODE_ENV=production`, which makes
   `npm install` (run by `make build-ext`) prune devDependencies → esbuild
   vanishes mid-build. Consider `npm install --include=dev` in the Makefile.

## Live test result (real Chrome, real profile)

- extension connected as profile `christian.kamien@gmail.com` ✔
- `diagnostics`, `list_tabs` returned real tabs ✔
- approval dialog raised via elicitation ✔ (accept bug found + fixed, see 1)
- UAT suite passes; `go vet` + unit tests green after all changes ✔
