// Wire protocol between the remote-chrome Go server and this extension.
// Must stay in sync with internal/bridge/protocol.go (PROTOCOL_VERSION).

// v2: sessionId on requests and events (flat routing to auto-attached
// out-of-process iframe targets).
export const PROTOCOL_VERSION = 2;

// extension -> server, first message on the socket
export interface HelloMsg {
  type: "hello";
  token: string;
  profile: string;
  protocolVersion: number;
  extensionVersion: string;
}

// server -> extension
export interface ServerRequest {
  id: number;
  type: "cdp" | "tabs" | "detach" | "detach_all" | "ping";
  tabId?: number;
  sessionId?: string; // CDP child session (OOPIF); absent = the tab's main session
  method?: string; // CDP method for "cdp", op name for "tabs"
  params?: any;
}

// extension -> server
export interface Response {
  id: number;
  result?: any;
  error?: string;
}

export interface EventMsg {
  type: "event";
  tabId: number;
  sessionId?: string; // child session the event came from; absent = main session
  method: string;
  params: any;
}

export interface DetachedMsg {
  type: "detached";
  tabId: number;
  reason: string;
}

export interface LogMsg {
  type: "log";
  level: "info" | "warn" | "error";
  message: string;
}
