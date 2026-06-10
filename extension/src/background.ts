// remote-chrome bridge — MV3 service worker.
// Holds a WebSocket to the local remote-chrome server and relays:
//   - CDP commands  -> chrome.debugger.sendCommand
//   - tab operations -> chrome.tabs / chrome.windows
//   - CDP events    <- chrome.debugger.onEvent
// All policy (permissions, approvals, audit) lives in the server; this
// worker is a dumb relay with a kill switch on the toolbar icon.

import {
  PROTOCOL_VERSION,
  ServerRequest,
  Response,
  EventMsg,
  DetachedMsg,
  HelloMsg,
} from "./protocol.js";

const EXTENSION_VERSION = chrome.runtime.getManifest().version;
const DEBUGGER_PROTOCOL = "1.3";

let ws: WebSocket | null = null;
let reconnectDelayMs = 1000;
let backoffResetTimer: ReturnType<typeof setTimeout> | undefined;
const MAX_RECONNECT_DELAY_MS = 30_000;
let killed = false; // toolbar kill switch engaged; no auto-reconnect until re-enabled
const attachedTabs = new Set<number>();

interface Settings {
  port: number;
  token: string;
  profile: string;
}

// Injected by esbuild: null in production builds; the UAT harness builds a
// test variant with hardcoded settings so headless Chrome connects without
// the options page. See extension/build.mjs and test/uat.
declare const __TEST_CONFIG__: Settings | null;

async function loadSettings(): Promise<Settings | null> {
  const s = await chrome.storage.local.get(["port", "token", "profile"]);
  if (s.port && s.token) {
    return { port: s.port, token: s.token, profile: s.profile || "default" };
  }
  if (typeof __TEST_CONFIG__ !== "undefined" && __TEST_CONFIG__) {
    return __TEST_CONFIG__;
  }
  return null;
}

function setBadge(text: string, color: string) {
  chrome.action.setBadgeText({ text });
  chrome.action.setBadgeBackgroundColor({ color });
}

// ---- Stale-build alert ----
// When the server refuses the hello with a protocol mismatch, the user must
// reload this extension (or rebuild/restart the server). Make that
// impossible to miss: red "upd" badge, explanatory icon tooltip, one OS
// notification. All of it clears automatically once a matching pair
// connects.

const DEFAULT_TITLE = "remote-chrome: click to disconnect (kill switch)";
const STALE_NOTIFICATION_ID = "remote-chrome-stale-build";
let staleAlertRaised = false;

function raiseStaleAlert(reason: string) {
  setBadge("upd", "#d9534f");
  chrome.action.setTitle({
    title: `remote-chrome: build mismatch — reload this extension in chrome://extensions (${reason})`,
  });
  if (staleAlertRaised) return; // one notification per mismatch episode
  staleAlertRaised = true;
  log("warn", `stale build: ${reason}`);
  chrome.notifications.create(
    STALE_NOTIFICATION_ID,
    {
      type: "basic",
      iconUrl: "icon128.png",
      title: "remote-chrome bridge needs a reload",
      message: `${reason}\nReload the extension in chrome://extensions.`,
      priority: 2,
      requireInteraction: true,
    },
    () => {
      // notifications can fail (disabled OS-side); the badge still shows
      void chrome.runtime.lastError;
    },
  );
}

function clearStaleAlert() {
  if (!staleAlertRaised) return;
  staleAlertRaised = false;
  chrome.action.setTitle({ title: DEFAULT_TITLE });
  chrome.notifications.clear(STALE_NOTIFICATION_ID, () => {
    void chrome.runtime.lastError;
  });
}

// ---- Connection state, observable by the options page ----

export type ConnState = "connected" | "connecting" | "disconnected" | "unconfigured" | "killed";

let connState: ConnState = "disconnected";
let connDetail = "";

function setState(state: ConnState, detail = "") {
  const changed = state !== connState || detail !== connDetail;
  connState = state;
  connDetail = detail;
  if (changed) {
    log(state === "connected" ? "info" : state === "connecting" ? "info" : "warn",
      detail ? `${state} — ${detail}` : state);
  }
  // Notify any open options page; rejects when nobody listens — ignore.
  chrome.runtime
    .sendMessage({ type: "bridge-status", state, detail })
    .catch(() => {});
}

// ---- Ring-buffer log, viewable live from the options page ----
// Kept in chrome.storage.session so it survives service-worker restarts
// (MV3 workers are torn down aggressively) but not browser restarts.

export interface LogEntry {
  t: number;
  level: "info" | "warn" | "error";
  msg: string;
}

const LOG_CAP = 300;
let logBuf: LogEntry[] = [];
let logRestored: Promise<void> | null = null;
let logPersistTimer: ReturnType<typeof setTimeout> | undefined;

function restoreLog(): Promise<void> {
  if (!logRestored) {
    logRestored = chrome.storage.session
      .get("log")
      .then((s) => {
        if (Array.isArray(s.log)) logBuf = [...s.log, ...logBuf].slice(-LOG_CAP);
      })
      .catch(() => {});
  }
  return logRestored;
}

function log(level: LogEntry["level"], msg: string) {
  const entry: LogEntry = { t: Date.now(), level, msg };
  logBuf.push(entry);
  if (logBuf.length > LOG_CAP) logBuf.splice(0, logBuf.length - LOG_CAP);
  // Also visible when inspecting the service worker console.
  const c = level === "error" ? console.error : level === "warn" ? console.warn : console.log;
  c(`[bridge] ${msg}`);
  // Debounced persist; storage failures must never break the relay.
  clearTimeout(logPersistTimer);
  logPersistTimer = setTimeout(() => {
    chrome.storage.session.set({ log: logBuf }).catch(() => {});
  }, 250);
  chrome.runtime.sendMessage({ type: "bridge-log", entry }).catch(() => {});
}

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg?.type === "get-status") {
    sendResponse({ state: connState, detail: connDetail });
  } else if (msg?.type === "get-log") {
    restoreLog().then(() => sendResponse(logBuf));
    return true; // async sendResponse
  }
});

// Surface anything otherwise silent in the worker.
self.addEventListener("error", (ev: any) => {
  log("error", `uncaught: ${ev?.message ?? ev}`);
});
self.addEventListener("unhandledrejection", (ev: any) => {
  log("error", `unhandled rejection: ${ev?.reason?.message ?? ev?.reason ?? ev}`);
});

function send(msg: Response | EventMsg | DetachedMsg | HelloMsg | object) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(msg));
  }
}

async function connect() {
  if (killed) return;
  const settings = await loadSettings();
  if (!settings) {
    setBadge("cfg", "#f0ad4e");
    setState("unconfigured", "enter port and token, then save");
    scheduleReconnect();
    return;
  }
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
    return;
  }

  const url = `ws://127.0.0.1:${settings.port}/ws`;
  setState("connecting", `port ${settings.port}`);
  try {
    ws = new WebSocket(url);
  } catch (e) {
    setState("disconnected", `cannot open ${url}`);
    scheduleReconnect();
    return;
  }

  ws.onopen = () => {
    // Only treat the connection as healthy (and reset backoff) once it has
    // survived a moment — the server accepts the socket and then closes it
    // when the hello is rejected, which must keep backing off.
    backoffResetTimer = setTimeout(() => {
      reconnectDelayMs = 1000;
      setBadge("on", "#5cb85c");
      setState("connected", `port ${settings.port} as "${settings.profile}"`);
      clearStaleAlert();
    }, 3000);
    const hello: HelloMsg = {
      type: "hello",
      token: settings.token,
      profile: settings.profile,
      protocolVersion: PROTOCOL_VERSION,
      extensionVersion: EXTENSION_VERSION,
    };
    ws!.send(JSON.stringify(hello));
    setState("connecting", "handshake sent — waiting for the server to accept");
  };

  ws.onmessage = (ev) => {
    // The server only talks to accepted connections — first message proves
    // the hello went through.
    if (connState !== "connected") {
      setBadge("on", "#5cb85c");
      setState("connected", `port ${settings.port} as "${settings.profile}"`);
      clearStaleAlert();
    }
    let req: ServerRequest;
    try {
      req = JSON.parse(ev.data as string);
    } catch {
      return;
    }
    handleRequest(req);
  };

  ws.onclose = (ev: CloseEvent) => {
    const wasConnected = connState === "connected";
    clearTimeout(backoffResetTimer);
    ws = null;
    setBadge(killed ? "off" : "", killed ? "#d9534f" : "#777");
    if (killed) {
      setState("killed", "kill switch engaged — click the toolbar icon to re-arm");
    } else if (ev.reason) {
      // The server says exactly why it closed (bad token, protocol
      // mismatch, profile label takeover) — show that, never guess.
      setState("disconnected", `server: ${ev.reason}`);
      if (/protocol.*mismatch/i.test(ev.reason)) {
        raiseStaleAlert(ev.reason);
      }
    } else if (wasConnected) {
      setState("disconnected", "connection dropped — retrying");
    } else {
      setState(
        "disconnected",
        `server not reachable on port ${settings.port} — is the remote-chrome server running (e.g. Claude Desktop started)?`,
      );
    }
    detachAll();
    scheduleReconnect();
  };

  ws.onerror = () => {
    // onclose follows with code/reason; just note it happened
    log("warn", "websocket error event");
  };
}

function scheduleReconnect() {
  if (killed) return;
  // chrome.alarms would survive worker shutdown, but the minimum period is
  // too coarse; setTimeout is fine because an open WebSocket keeps the
  // worker alive (Chrome >=116) and after shutdown the next event restarts us.
  setTimeout(connect, reconnectDelayMs);
  reconnectDelayMs = Math.min(reconnectDelayMs * 2, MAX_RECONNECT_DELAY_MS);
}

async function handleRequest(req: ServerRequest) {
  if (req.type === "ping") {
    send({ id: req.id, result: "pong" });
    return;
  }
  try {
    let result: any;
    switch (req.type) {
      case "cdp":
        result = await cdpCommand(req.tabId!, req.sessionId, req.method!, req.params);
        break;
      case "tabs":
        result = await tabsOp(req.method!, req.params || {});
        break;
      case "detach":
        await detachTab(req.tabId!);
        result = true;
        break;
      case "detach_all":
        await detachAll();
        result = true;
        break;
      default:
        throw new Error(`unknown request type: ${(req as any).type}`);
    }
    send({ id: req.id, result });
  } catch (e: any) {
    const err = String(e?.message ?? e);
    log("error", `${req.type}${req.method ? ` ${req.method}` : ""} (tab ${req.tabId ?? "-"}) failed: ${err}`);
    send({ id: req.id, error: err });
  }
}

async function cdpCommand(tabId: number, sessionId: string | undefined, method: string, params: any): Promise<any> {
  await ensureAttached(tabId);
  // sessionId routes to an auto-attached child target (OOPIF) — flat session
  // mode, supported by chrome.debugger since Chrome 125.
  const target: chrome.debugger.Debuggee & { sessionId?: string } = { tabId };
  if (sessionId) target.sessionId = sessionId;
  return chrome.debugger.sendCommand(target, method, params || {});
}

async function ensureAttached(tabId: number) {
  if (attachedTabs.has(tabId)) return;
  await chrome.debugger.attach({ tabId }, DEBUGGER_PROTOCOL);
  attachedTabs.add(tabId);
}

async function detachTab(tabId: number) {
  if (!attachedTabs.has(tabId)) return;
  attachedTabs.delete(tabId);
  try {
    await chrome.debugger.detach({ tabId });
  } catch {
    // already gone (tab closed, user canceled the banner) — fine
  }
}

async function detachAll() {
  const tabs = [...attachedTabs];
  attachedTabs.clear();
  for (const tabId of tabs) {
    try {
      await chrome.debugger.detach({ tabId });
    } catch {
      // ignore
    }
  }
}

async function tabsOp(op: string, params: any): Promise<any> {
  switch (op) {
    case "list": {
      const tabs = await chrome.tabs.query({});
      return tabs.map((t) => ({
        tabId: t.id,
        windowId: t.windowId,
        url: t.url,
        title: t.title,
        active: t.active,
        attached: t.id !== undefined && attachedTabs.has(t.id),
      }));
    }
    case "create": {
      const tab = await chrome.tabs.create({
        url: params.url || "about:blank",
        active: params.active !== false,
      });
      return { tabId: tab.id, windowId: tab.windowId };
    }
    case "close": {
      await chrome.tabs.remove(params.tabId);
      return true;
    }
    case "activate": {
      const tab = await chrome.tabs.update(params.tabId, { active: true });
      if (tab && tab.windowId !== undefined) {
        await chrome.windows.update(tab.windowId, { focused: true });
      }
      return true;
    }
    case "get": {
      const tab = await chrome.tabs.get(params.tabId);
      return { tabId: tab.id, url: tab.url, title: tab.title, active: tab.active };
    }
    case "captureVisible": {
      const tab = await chrome.tabs.get(params.tabId);
      if (!tab.active) {
        await chrome.tabs.update(params.tabId, { active: true });
      }
      const dataUrl = await chrome.tabs.captureVisibleTab(tab.windowId!, {
        format: params.format || "jpeg",
        quality: params.quality ?? 70,
      });
      return { dataUrl };
    }
    default:
      throw new Error(`unknown tabs op: ${op}`);
  }
}

// ---- CDP events -> server ----

chrome.debugger.onEvent.addListener((source, method, params) => {
  if (source.tabId === undefined) return;
  const msg: EventMsg = { type: "event", tabId: source.tabId, method, params };
  const sessionId = (source as chrome.debugger.Debuggee & { sessionId?: string }).sessionId;
  if (sessionId) msg.sessionId = sessionId;
  send(msg);
});

chrome.debugger.onDetach.addListener((source, reason) => {
  if (source.tabId === undefined) return;
  log("warn", `debugger detached from tab ${source.tabId}: ${reason}`);
  attachedTabs.delete(source.tabId);
  const msg: DetachedMsg = { type: "detached", tabId: source.tabId, reason };
  send(msg);
});

chrome.tabs.onRemoved.addListener((tabId) => {
  attachedTabs.delete(tabId);
  send({ type: "detached", tabId, reason: "tab_closed" });
});

// ---- Kill switch: toolbar click toggles the connection ----

chrome.action.onClicked.addListener(async () => {
  if (!killed) {
    killed = true;
    await detachAll();
    if (ws) ws.close();
    ws = null;
    setBadge("off", "#d9534f");
  } else {
    killed = false;
    reconnectDelayMs = 1000;
    connect();
  }
});

// ---- Lifecycle ----

chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
restoreLog();
chrome.storage.onChanged.addListener((_changes, area) => {
  if (area === "local") {
    if (ws) ws.close();
    else connect();
  }
});

connect();
