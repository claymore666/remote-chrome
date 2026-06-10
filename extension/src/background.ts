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
    scheduleReconnect();
    return;
  }
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
    return;
  }

  const url = `ws://127.0.0.1:${settings.port}/ws`;
  try {
    ws = new WebSocket(url);
  } catch (e) {
    scheduleReconnect();
    return;
  }

  ws.onopen = () => {
    reconnectDelayMs = 1000;
    const hello: HelloMsg = {
      type: "hello",
      token: settings.token,
      profile: settings.profile,
      protocolVersion: PROTOCOL_VERSION,
      extensionVersion: EXTENSION_VERSION,
    };
    ws!.send(JSON.stringify(hello));
    setBadge("on", "#5cb85c");
  };

  ws.onmessage = (ev) => {
    let req: ServerRequest;
    try {
      req = JSON.parse(ev.data as string);
    } catch {
      return;
    }
    handleRequest(req);
  };

  ws.onclose = () => {
    ws = null;
    setBadge(killed ? "off" : "", killed ? "#d9534f" : "#777");
    detachAll();
    scheduleReconnect();
  };

  ws.onerror = () => {
    // onclose follows; nothing to do here
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
        result = await cdpCommand(req.tabId!, req.method!, req.params);
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
    send({ id: req.id, error: String(e?.message ?? e) });
  }
}

async function cdpCommand(tabId: number, method: string, params: any): Promise<any> {
  await ensureAttached(tabId);
  return chrome.debugger.sendCommand({ tabId }, method, params || {});
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
  send(msg);
});

chrome.debugger.onDetach.addListener((source, reason) => {
  if (source.tabId === undefined) return;
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
chrome.storage.onChanged.addListener((_changes, area) => {
  if (area === "local") {
    if (ws) ws.close();
    else connect();
  }
});

connect();
