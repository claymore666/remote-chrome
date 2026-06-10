// Wire protocol between the remote-chrome Go server and this extension.
// Must stay in sync with internal/bridge/protocol.go (PROTOCOL_VERSION).

export const PROTOCOL_VERSION = 1;

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
