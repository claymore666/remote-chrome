// Package bridge hosts the localhost WebSocket endpoint that remote-chrome's
// Chrome extension instances connect to, and relays commands/events.
//
// Wire protocol — must stay in sync with extension/src/protocol.ts.
package bridge

import "encoding/json"

// ProtocolVersion is checked against the extension's hello message; the
// server refuses mismatched extensions.
//
// v2: sessionId on requests and events (flat routing to auto-attached
// out-of-process iframe targets).
const ProtocolVersion = 2

// Hello is the first message an extension sends after connecting.
type Hello struct {
	Type             string `json:"type"` // "hello"
	Token            string `json:"token"`
	Profile          string `json:"profile"`
	ProtocolVersion  int    `json:"protocolVersion"`
	ExtensionVersion string `json:"extensionVersion"`
}

// Request is a server -> extension command.
type Request struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"` // "cdp" | "tabs" | "detach" | "detach_all" | "ping"
	TabID     int             `json:"tabId,omitempty"`
	SessionID string          `json:"sessionId,omitempty"` // CDP child session (OOPIF); empty = the tab's main session
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// inbound is any extension -> server message; fields are a union over
// responses, events and logs, discriminated by Type ("" for responses).
type inbound struct {
	// response
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	// event / detached / log / hello
	Type      string          `json:"type"`
	TabID     int             `json:"tabId"`
	SessionID string          `json:"sessionId"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	Reason    string          `json:"reason"`
	Level     string          `json:"level"`
	Message   string          `json:"message"`
}

// Event is a CDP event (or synthetic detach notice) forwarded by an extension.
type Event struct {
	Profile   string
	TabID     int
	SessionID string // child session the event came from; empty = main session
	Method    string // CDP method, or "__detached" with Params nil
	Params    json.RawMessage
}
