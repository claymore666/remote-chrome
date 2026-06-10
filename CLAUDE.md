# browserd — Browser Control MCP Server

Go MCP server + Chrome MV3 extension that lets Claude act in the user's *real*
Chrome profiles, with every consequential action gated by server-enforced
approvals. Full design: `PLAN.md`. Running decisions/deviations: `NOTES.md`.

## Architecture (one screen)

```
Claude (MCP client) ── stdio JSON-RPC ──> cmd/browserd (Go)
                                            │  internal/server   MCP tools + permission gating
                                            │  internal/perms    (action group × eTLD+1) grant matrix
                                            │  internal/approval elicitation / zenity / PowerShell dialogs
                                            │  internal/audit    JSONL audit log
                                            │  internal/browser  high-level page ops (snapshot/click/wait)
                                            │  internal/bridge   WebSocket hub, 127.0.0.1, token+origin auth
                                            ▼
                              extension/ (MV3, TypeScript)  ── chrome.debugger / chrome.tabs
```

- The extension is a **dumb relay**; all policy lives in the Go server.
- Wire protocol: `internal/bridge/protocol.go` ⇔ `extension/src/protocol.ts`.
  Bump `ProtocolVersion` in BOTH when changing it; server refuses mismatches.
- CDP commands are untyped JSON (no cdproto dependency — keeps go.mod on 1.25).

## Build & test

```sh
make build        # Go binary -> bin/browserd, extension -> extension/dist/
make test         # all Go unit + module tests, no Chrome (go test ./internal/...)
make uat-chrome   # one-time: fetch Chrome for Testing for the UAT suite
make test-uat     # end-to-end against real headless Chrome (go test -tags uat ./test/uat)
make lint         # gofmt + go vet (incl. -tags uat) + tsc --noEmit
```

- Single test: `go test ./internal/<pkg> -run TestName`
  (UAT subtest: `go test -tags uat ./test/uat -run TestUAT/<subtest>`).

- Extension build: `cd extension && npm install && npm run build` (esbuild).
- UAT builds a test variant of the extension with `__TEST_CONFIG__` injected
  (esbuild define) so headless Chrome connects without the options page.
  Production builds define it as `null` — never ship a test build.

## Key invariants (do not break)

1. **Nothing executes without a grant.** Every gated tool goes through
   `server.ensureGrant` BEFORE any command reaches the bridge. New tools must
   declare an action group (`read|navigate|interact|upload|download|eval|profile`).
2. **The model never renders or answers approval dialogs** — approvals are MCP
   elicitation (or native OS dialog fallback), decided by the human.
3. **No cookie/credential tools.** Never expose cookie jars, saved passwords,
   or `chrome.cookies`.
4. WebSocket listener binds `127.0.0.1` only; token compared constant-time;
   `Origin` must be `chrome-extension://`; max message size enforced.
5. Every navigation, interaction, approval decision and profile target is
   appended to the audit log (`~/.browserd/audit.jsonl`).
6. uids returned by `snapshot()` are per-tab, per-generation; interaction with
   a stale uid must error with "take a new snapshot", never guess.

## Conventions

- Errors returned from tool handlers are tool errors (shown to the model) —
  make them actionable ("no grant for interact on x.com — call
  request_permission").
- Logging: `slog` to stderr (stdout is the MCP transport — never print to it).
  `--verbose` dumps every relayed CDP command/response.
- Config/state dir: `~/.browserd` (Linux) / `%LOCALAPPDATA%\browserd` (Win).
- Tests live next to their packages (module tests with fake extension/CDP
  included; `internal/server/server_test.go` is the gating spec);
  UAT/regression in `test/uat` behind the `uat` build tag.
- Before fighting Chrome/CDP weirdness, check the "Gotchas log" in `NOTES.md` —
  it records hard-won fixes (history navigation races, MV3 SW keepalive,
  dialog handling, UAT temp-dir teardown).
