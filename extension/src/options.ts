const $ = (id: string) => document.getElementById(id) as HTMLInputElement;

async function load() {
  const s = await chrome.storage.local.get(["port", "token", "profile"]);
  if (s.port) $("port").value = String(s.port);
  if (s.token) $("token").value = s.token;
  $("profile").value = s.profile || "default";
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
  setStatus("saved — reconnecting", "#5cb85c");
}

function setStatus(text: string, color: string) {
  const el = document.getElementById("status")!;
  el.textContent = text;
  (el as HTMLElement).style.color = color;
}

document.getElementById("save")!.addEventListener("click", save);
load();
