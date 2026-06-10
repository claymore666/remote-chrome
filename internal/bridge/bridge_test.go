package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testToken = "tok-123"

// fakeExtension is a test double for the Chrome extension: a WebSocket
// client that answers relayed requests via a programmable handler.
type fakeExtension struct {
	t       *testing.T
	ws      *websocket.Conn
	handler func(req map[string]any) (any, string) // returns (result, error)
	mu      sync.Mutex
	done    chan struct{}
}

func dialExt(t *testing.T, port int, origin, token, profile string, protoVersion int) (*fakeExtension, *http.Response, error) {
	t.Helper()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	ws, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws", port), hdr)
	if err != nil {
		return nil, resp, err
	}
	hello := map[string]any{
		"type": "hello", "token": token, "profile": profile,
		"protocolVersion": protoVersion, "extensionVersion": "test",
	}
	if err := ws.WriteJSON(hello); err != nil {
		return nil, resp, err
	}
	fe := &fakeExtension{t: t, ws: ws, done: make(chan struct{})}
	return fe, resp, nil
}

// pump runs the fake extension's read loop, answering pings and relayed
// requests with the handler.
func (fe *fakeExtension) pump() {
	go func() {
		defer close(fe.done)
		for {
			var req map[string]any
			if err := fe.ws.ReadJSON(&req); err != nil {
				return
			}
			id := req["id"]
			if req["type"] == "ping" {
				fe.send(map[string]any{"id": id, "result": "pong"})
				continue
			}
			fe.mu.Lock()
			h := fe.handler
			fe.mu.Unlock()
			if h == nil {
				fe.send(map[string]any{"id": id, "error": "no handler"})
				continue
			}
			result, errStr := h(req)
			if errStr != "" {
				fe.send(map[string]any{"id": id, "error": errStr})
			} else {
				fe.send(map[string]any{"id": id, "result": result})
			}
		}
	}()
}

func (fe *fakeExtension) send(v any) {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	fe.ws.WriteJSON(v)
}

func (fe *fakeExtension) close() { fe.ws.Close() }

func startBridge(t *testing.T, pinned []string) (*Bridge, int) {
	t.Helper()
	b := New(testToken, pinned, false, nil)
	port, err := b.Start(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, port
}

func waitProfiles(t *testing.T, b *Bridge, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(b.Profiles()) == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d profiles, have %v", n, b.Profiles())
}

const goodOrigin = "chrome-extension://abcdefghijklmnop"

func TestRejectsNonExtensionOrigin(t *testing.T) {
	_, port := startBridge(t, nil)
	for _, origin := range []string{"", "http://evil.localhost", "https://example.com"} {
		_, resp, err := dialExt(t, port, origin, testToken, "p", ProtocolVersion)
		if err == nil {
			t.Fatalf("origin %q: expected rejection", origin)
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: expected 403, got %v", origin, resp)
		}
	}
}

func TestRejectsUnpinnedOrigin(t *testing.T) {
	b, port := startBridge(t, []string{goodOrigin})
	if _, _, err := dialExt(t, port, "chrome-extension://other", testToken, "p", ProtocolVersion); err == nil {
		t.Fatal("expected unpinned origin to be rejected")
	}
	fe, _, err := dialExt(t, port, goodOrigin, testToken, "p", ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer fe.close()
	fe.pump()
	waitProfiles(t, b, 1)
}

func TestRejectsBadToken(t *testing.T) {
	b, port := startBridge(t, nil)
	fe, _, err := dialExt(t, port, goodOrigin, "wrong-token", "p", ProtocolVersion)
	if err != nil {
		t.Fatal(err) // handshake succeeds; close follows the hello
	}
	defer fe.close()
	fe.pump()
	select {
	case <-fe.done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection with bad token was not closed")
	}
	if len(b.Profiles()) != 0 {
		t.Fatal("bad-token profile must not be registered")
	}
}

func TestRejectsProtocolMismatch(t *testing.T) {
	b, port := startBridge(t, nil)
	fe, _, err := dialExt(t, port, goodOrigin, testToken, "p", ProtocolVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	defer fe.close()
	fe.pump()
	select {
	case <-fe.done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection with wrong protocol version was not closed")
	}
	if len(b.Profiles()) != 0 {
		t.Fatal("mismatched profile must not be registered")
	}
}

func TestCDPCallRoundtrip(t *testing.T) {
	b, port := startBridge(t, nil)
	fe, _, err := dialExt(t, port, goodOrigin, testToken, "personal", ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer fe.close()
	fe.handler = func(req map[string]any) (any, string) {
		if req["type"] != "cdp" || req["method"] != "Page.navigate" {
			return nil, "unexpected request"
		}
		var params map[string]any
		raw, _ := json.Marshal(req["params"])
		json.Unmarshal(raw, &params)
		return map[string]any{"frameId": "f1", "echo": params["url"]}, ""
	}
	fe.pump()
	waitProfiles(t, b, 1)

	raw, err := b.CDP(context.Background(), "personal", 7, "Page.navigate", map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		FrameID string `json:"frameId"`
		Echo    string `json:"echo"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.FrameID != "f1" || resp.Echo != "https://example.com" {
		t.Fatalf("bad response: %+v", resp)
	}
}

func TestExtensionErrorPropagates(t *testing.T) {
	b, port := startBridge(t, nil)
	fe, _, _ := dialExt(t, port, goodOrigin, testToken, "p", ProtocolVersion)
	defer fe.close()
	fe.handler = func(req map[string]any) (any, string) { return nil, "Cannot attach to this target" }
	fe.pump()
	waitProfiles(t, b, 1)

	_, err := b.CDP(context.Background(), "p", 1, "Page.enable", nil)
	if err == nil || !contains(err.Error(), "Cannot attach") {
		t.Fatalf("expected extension error, got %v", err)
	}
}

func TestUnknownProfileAndNoneConnected(t *testing.T) {
	b, port := startBridge(t, nil)
	if _, err := b.CDP(context.Background(), "nope", 1, "Page.enable", nil); err == nil {
		t.Fatal("expected error with no extension connected")
	}
	fe, _, _ := dialExt(t, port, goodOrigin, testToken, "work", ProtocolVersion)
	defer fe.close()
	fe.pump()
	waitProfiles(t, b, 1)
	_, err := b.CDP(context.Background(), "nope", 1, "Page.enable", nil)
	if err == nil || !contains(err.Error(), "work") {
		t.Fatalf("error should name connected profiles, got %v", err)
	}
}

func TestEventsAreDispatched(t *testing.T) {
	b, port := startBridge(t, nil)
	got := make(chan Event, 1)
	b.SetEventHandler(func(ev Event) { got <- ev })
	fe, _, _ := dialExt(t, port, goodOrigin, testToken, "p", ProtocolVersion)
	defer fe.close()
	fe.pump()
	waitProfiles(t, b, 1)

	fe.send(map[string]any{"type": "event", "tabId": 9, "method": "Page.loadEventFired", "params": map[string]any{"timestamp": 1}})
	select {
	case ev := <-got:
		if ev.Profile != "p" || ev.TabID != 9 || ev.Method != "Page.loadEventFired" {
			t.Fatalf("bad event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event not dispatched")
	}

	fe.send(map[string]any{"type": "detached", "tabId": 9, "reason": "user"})
	select {
	case ev := <-got:
		if ev.Method != "__detached" || ev.TabID != 9 {
			t.Fatalf("bad detach event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("detach event not dispatched")
	}
}

func TestPendingCallFailsOnDisconnect(t *testing.T) {
	b, port := startBridge(t, nil)
	fe, _, _ := dialExt(t, port, goodOrigin, testToken, "p", ProtocolVersion)
	fe.handler = func(req map[string]any) (any, string) {
		fe.ws.Close() // die instead of answering
		select {}
	}
	go func() { // manual pump that closes on first request
		var req map[string]any
		for {
			if err := fe.ws.ReadJSON(&req); err != nil {
				return
			}
			if req["type"] == "ping" {
				fe.send(map[string]any{"id": req["id"], "result": "pong"})
				continue
			}
			fe.ws.Close()
			return
		}
	}()
	waitProfiles(t, b, 1)

	_, err := b.CDP(context.Background(), "p", 1, "Page.enable", nil)
	if err == nil {
		t.Fatal("expected pending call to fail when the extension dies")
	}
	waitProfiles(t, b, 0)
}

func TestConnHookAndReconnectReplaces(t *testing.T) {
	b, port := startBridge(t, nil)
	var mu sync.Mutex
	var events []string
	b.SetConnHook(func(profile string, connected bool) {
		mu.Lock()
		events = append(events, fmt.Sprintf("%s=%v", profile, connected))
		mu.Unlock()
	})
	fe1, _, _ := dialExt(t, port, goodOrigin, testToken, "p", ProtocolVersion)
	fe1.pump()
	waitProfiles(t, b, 1)
	// Second connection for the same profile replaces the first.
	fe2, _, _ := dialExt(t, port, goodOrigin, testToken, "p", ProtocolVersion)
	defer fe2.close()
	fe2.pump()
	time.Sleep(100 * time.Millisecond)
	if got := len(b.Profiles()); got != 1 {
		t.Fatalf("expected 1 profile after reconnect, got %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 || events[0] != "p=true" {
		t.Fatalf("conn hook events: %v", events)
	}
}

func TestCallTimeout(t *testing.T) {
	b, port := startBridge(t, nil)
	fe, _, _ := dialExt(t, port, goodOrigin, testToken, "p", ProtocolVersion)
	defer fe.close()
	fe.handler = func(req map[string]any) (any, string) {
		time.Sleep(2 * time.Second)
		return "late", ""
	}
	fe.pump()
	waitProfiles(t, b, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := b.CDP(ctx, "p", 1, "Page.enable", nil)
	if err == nil || !contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
