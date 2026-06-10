const $ = (id: string) => document.getElementById(id) as HTMLInputElement;

async function signedInEmail(): Promise<string | null> {
  try {
    const info = await chrome.identity.getProfileUserInfo({
      accountStatus: chrome.identity.AccountStatus.ANY,
    });
    return info.email || null;
  } catch {
    return null;
  }
}

async function load() {
  const s = await chrome.storage.local.get(["port", "token", "profile"]);
  if (s.port) $("port").value = String(s.port);
  if (s.token) $("token").value = s.token;
  // Prefill the profile label with the Chrome account email when the user
  // hasn't chosen one yet.
  $("profile").value = s.profile || (await signedInEmail()) || "default";
  renderStatus(await queryStatus());
}

async function save() {
  const port = parseInt($("port").value, 10);
  const token = $("token").value.trim();
  const profile = $("profile").value.trim() || "default";
  if (!port || !token) {
    setStatus("port and token are required", "#d9534f");
    return;
  }
  await chrome.storage.local.set({ port, token, profile });
  setStatus("saved", "#5cb85c");
}

function setStatus(text: string, color: string) {
  const el = document.getElementById("status")!;
  el.textContent = text;
  (el as HTMLElement).style.color = color;
}

// ---- Live connection state (reported by the background worker) ----

interface BridgeStatus {
  state: "connected" | "connecting" | "disconnected" | "unconfigured" | "killed" | "unknown";
  detail: string;
}

const STATE_STYLE: Record<BridgeStatus["state"], { color: string; label: string }> = {
  unknown: { color: "#d9534f", label: "✖ no status from background worker" },
  connected: { color: "#5cb85c", label: "✔ connected" },
  connecting: { color: "#f0ad4e", label: "… connecting" },
  disconnected: { color: "#d9534f", label: "✖ not connected" },
  unconfigured: { color: "#f0ad4e", label: "not configured" },
  killed: { color: "#d9534f", label: "✖ kill switch engaged" },
};

async function queryStatus(): Promise<BridgeStatus> {
  try {
    const r = await chrome.runtime.sendMessage({ type: "get-status" });
    if (r && r.state) return r as BridgeStatus;
  } catch {
    // old worker build or worker unreachable
  }
  return { state: "unknown", detail: "reload the extension on chrome://extensions" };
}

function renderStatus(s: BridgeStatus) {
  const el = document.getElementById("conn")!;
  const { color, label } = STATE_STYLE[s.state] ?? STATE_STYLE.disconnected;
  el.textContent = s.detail ? `${label} — ${s.detail}` : label;
  el.style.color = color;
}

// ---- Log panel ----

interface LogEntry {
  t: number;
  level: "info" | "warn" | "error";
  msg: string;
}

const logEl = () => document.getElementById("log")!;

function fmtEntry(e: LogEntry): HTMLElement {
  const line = document.createElement("div");
  line.className = e.level;
  const ts = new Date(e.t).toLocaleTimeString(undefined, { hour12: false });
  line.textContent = `${ts} [${e.level}] ${e.msg}`;
  return line;
}

function appendLog(e: LogEntry) {
  const el = logEl();
  if (el.childElementCount === 0) el.textContent = ""; // clear placeholder text
  const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 4;
  el.appendChild(fmtEntry(e));
  while (el.childElementCount > 300) el.firstElementChild!.remove();
  if (atBottom) el.scrollTop = el.scrollHeight;
}

async function loadLog() {
  let entries: LogEntry[] = [];
  try {
    const r = await chrome.runtime.sendMessage({ type: "get-log" });
    if (Array.isArray(r)) entries = r;
  } catch {
    // old worker build; panel stays empty
  }
  const el = logEl();
  el.textContent = "";
  if (entries.length === 0) {
    el.textContent = "(no log entries yet)";
    return;
  }
  for (const e of entries) el.appendChild(fmtEntry(e));
  el.scrollTop = el.scrollHeight;
}

document.getElementById("copylog")!.addEventListener("click", async () => {
  await navigator.clipboard.writeText(logEl().innerText);
});

chrome.runtime.onMessage.addListener((msg) => {
  if (msg?.type === "bridge-status") {
    renderStatus({ state: msg.state, detail: msg.detail });
  } else if (msg?.type === "bridge-log" && msg.entry) {
    appendLog(msg.entry as LogEntry);
  }
});

document.getElementById("save")!.addEventListener("click", save);
load();
loadLog();
