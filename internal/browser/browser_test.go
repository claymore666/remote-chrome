package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-chrome/internal/bridge"
)

// fakeCaller simulates the extension side: programmable per-CDP-method
// responses plus a call log for asserting what was relayed.
type fakeCaller struct {
	mu       sync.Mutex
	cdp      map[string]func(params json.RawMessage) (any, error)
	tabs     map[string]func(params json.RawMessage) (any, error)
	calls    []string
	detached []int
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{
		cdp:  map[string]func(json.RawMessage) (any, error){},
		tabs: map[string]func(json.RawMessage) (any, error){},
	}
}

func (f *fakeCaller) on(method string, fn func(json.RawMessage) (any, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cdp[method] = fn
}

func (f *fakeCaller) onValue(method string, v any) {
	f.on(method, func(json.RawMessage) (any, error) { return v, nil })
}

// onSess programs a response for a method on one child session; such calls
// are logged as "<sessionID>|<method>". Methods without a session-specific
// handler fall back to the session-agnostic one.
func (f *fakeCaller) onSess(sessionID, method string, v any) {
	f.on(sessionID+"|"+method, func(json.RawMessage) (any, error) { return v, nil })
}

func (f *fakeCaller) CDP(ctx context.Context, profile string, tabID int, sessionID, method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	name := method
	if sessionID != "" {
		name = sessionID + "|" + method
	}
	f.mu.Lock()
	f.calls = append(f.calls, name)
	fn := f.cdp[name]
	if fn == nil {
		fn = f.cdp[method]
	}
	f.mu.Unlock()
	if fn == nil {
		return json.RawMessage(`{}`), nil
	}
	v, err := fn(raw)
	if err != nil {
		return nil, err
	}
	out, _ := json.Marshal(v)
	return out, nil
}

func (f *fakeCaller) Tabs(ctx context.Context, profile string, op string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, "tabs."+op)
	fn := f.tabs[op]
	f.mu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("no fake for tabs.%s", op)
	}
	v, err := fn(raw)
	if err != nil {
		return nil, err
	}
	out, _ := json.Marshal(v)
	return out, nil
}

func (f *fakeCaller) Detach(ctx context.Context, profile string, tabID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached = append(f.detached, tabID)
	return nil
}

func (f *fakeCaller) Profiles() []string { return []string{"test"} }

func (f *fakeCaller) calledWith(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == method {
			return true
		}
	}
	return false
}

func (f *fakeCaller) waitCalled(t *testing.T, method string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.calledWith(method) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s was never called; calls: %v", method, f.calls)
}

func setup(t *testing.T) (*Manager, *fakeCaller, *Tab) {
	f := newFakeCaller()
	m := NewManager(f, nil)
	return m, f, m.Tab("test", 1)
}

func TestDialogAutoDismiss(t *testing.T) {
	m, f, tab := setup(t)
	gotParams := make(chan map[string]any, 1)
	f.on("Page.handleJavaScriptDialog", func(p json.RawMessage) (any, error) {
		var v map[string]any
		json.Unmarshal(p, &v)
		gotParams <- v
		return map[string]any{}, nil
	})
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Page.javascriptDialogOpening",
		Params: json.RawMessage(`{"message":"Are you sure?","type":"confirm"}`)})
	select {
	case p := <-gotParams:
		if p["accept"] != false {
			t.Fatalf("default policy must dismiss, got %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dialog was not auto-handled")
	}
	last := tab.LastDialog()
	if last == nil || last.Type != "confirm" || last.Message != "Are you sure?" || last.Accepted {
		t.Fatalf("last dialog not recorded: %+v", last)
	}
}

func TestDialogPolicyIsOneShot(t *testing.T) {
	m, f, tab := setup(t)
	var accepts []any
	var mu sync.Mutex
	f.on("Page.handleJavaScriptDialog", func(p json.RawMessage) (any, error) {
		var v map[string]any
		json.Unmarshal(p, &v)
		mu.Lock()
		accepts = append(accepts, v["accept"])
		mu.Unlock()
		return map[string]any{}, nil
	})
	tab.SetDialogPolicy(true, "hello")
	ev := bridge.Event{Profile: "test", TabID: 1, Method: "Page.javascriptDialogOpening",
		Params: json.RawMessage(`{"message":"q","type":"prompt"}`)}
	// Real Chrome never opens a second dialog before the first is answered,
	// so deliver them sequentially: fire, wait for the response, fire again.
	for want := 1; want <= 2; want++ {
		m.HandleEvent(ev)
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			n := len(accepts)
			mu.Unlock()
			if n >= want || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(accepts) != 2 || accepts[0] != true || accepts[1] != false {
		t.Fatalf("policy must apply once then revert to dismiss: %v", accepts)
	}
}

func TestConsoleBuffering(t *testing.T) {
	m, _, tab := setup(t)
	for i := 0; i < consoleBufCap+10; i++ {
		m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Runtime.consoleAPICalled",
			Params: json.RawMessage(fmt.Sprintf(`{"type":"log","args":[{"value":"msg %d"},{"value":42}]}`, i))})
	}
	got := tab.Console()
	if len(got) != consoleBufCap {
		t.Fatalf("ring buffer should cap at %d, got %d", consoleBufCap, len(got))
	}
	last := got[len(got)-1]
	if last.Type != "log" || !strings.Contains(last.Text, "42") {
		t.Fatalf("bad entry: %+v", last)
	}
}

func TestNetworkBufferingAndIdle(t *testing.T) {
	m, _, tab := setup(t)
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Network.requestWillBeSent",
		Params: json.RawMessage(`{"requestId":"r1","type":"XHR","request":{"url":"https://api.example.com/x","method":"POST"}}`)})
	if tab.inflightCount() != 1 {
		t.Fatal("request should be in flight")
	}
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Network.responseReceived",
		Params: json.RawMessage(`{"requestId":"r1","response":{"status":201}}`)})
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Network.loadingFinished",
		Params: json.RawMessage(`{"requestId":"r1"}`)})
	if tab.inflightCount() != 0 {
		t.Fatal("request should have completed")
	}
	entries := tab.Network()
	if len(entries) != 1 || entries[0].Status != 201 || entries[0].Method != "POST" || !entries[0].Finished {
		t.Fatalf("bad network entry: %+v", entries)
	}

	// failed request
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Network.requestWillBeSent",
		Params: json.RawMessage(`{"requestId":"r2","request":{"url":"https://x.com","method":"GET"}}`)})
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Network.loadingFailed",
		Params: json.RawMessage(`{"requestId":"r2","errorText":"net::ERR_FAILED"}`)})
	entries = tab.Network()
	if entries[1].Failed != "net::ERR_FAILED" {
		t.Fatalf("failure not recorded: %+v", entries[1])
	}
}

func TestWaitNetworkIdle(t *testing.T) {
	m, _, tab := setup(t)
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Network.requestWillBeSent",
		Params: json.RawMessage(`{"requestId":"r1","request":{"url":"https://x.com","method":"GET"}}`)})
	go func() {
		time.Sleep(150 * time.Millisecond)
		m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Network.loadingFinished",
			Params: json.RawMessage(`{"requestId":"r1"}`)})
	}()
	start := time.Now()
	if err := m.waitNetworkIdle(context.Background(), tab, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("idle must require 500ms of quiet after completion, returned after %v", elapsed)
	}
}

func TestWaitForSelector(t *testing.T) {
	m, f, tab := setup(t)
	var n int
	var mu sync.Mutex
	f.on("Runtime.evaluate", func(json.RawMessage) (any, error) {
		mu.Lock()
		n++
		found := n >= 3
		mu.Unlock()
		return map[string]any{"result": map[string]any{"value": found}}, nil
	})
	if err := m.WaitFor(context.Background(), tab, "selector", "#done", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitFor(context.Background(), tab, "bogus", "", time.Second); err == nil {
		t.Fatal("unknown condition must error")
	}
}

func TestClickDispatchesRealMouseEvents(t *testing.T) {
	m, f, tab := setup(t)
	tab.uids["e7"] = uidRef{backendID: 7}
	f.onValue("DOM.getBoxModel", map[string]any{"model": map[string]any{
		"content": []float64{10, 20, 110, 20, 110, 60, 10, 60}, "width": 100, "height": 40}})
	var events []map[string]any
	var mu sync.Mutex
	f.on("Input.dispatchMouseEvent", func(p json.RawMessage) (any, error) {
		var v map[string]any
		json.Unmarshal(p, &v)
		mu.Lock()
		events = append(events, v)
		mu.Unlock()
		return map[string]any{}, nil
	})
	if err := m.Click(context.Background(), tab, "e7", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("want move+press+release, got %v", events)
	}
	if events[0]["type"] != "mouseMoved" || events[1]["type"] != "mousePressed" || events[2]["type"] != "mouseReleased" {
		t.Fatalf("bad event order: %v", events)
	}
	// center of the 100x40 box at (10,20)
	if events[1]["x"] != 60.0 || events[1]["y"] != 40.0 {
		t.Fatalf("click not at element center: %v", events[1])
	}
}

func TestClickUnknownUID(t *testing.T) {
	m, _, tab := setup(t)
	err := m.Click(context.Background(), tab, "e404", false)
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("expected actionable snapshot error, got %v", err)
	}
}

func TestNavigateClearsUIDsAndChecksError(t *testing.T) {
	m, f, tab := setup(t)
	tab.uids["e1"] = uidRef{backendID: 1}
	f.onValue("Page.navigate", map[string]any{"frameId": "f1"})
	f.onValue("Runtime.evaluate", map[string]any{"result": map[string]any{"value": "complete"}})
	if err := m.Navigate(context.Background(), tab, "https://example.com", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := tab.resolveUID("e1"); err == nil {
		t.Fatal("uids must be invalidated by navigation")
	}
	f.onValue("Page.navigate", map[string]any{"errorText": "net::ERR_NAME_NOT_RESOLVED"})
	err := m.Navigate(context.Background(), tab, "https://nope.invalid", time.Second)
	if err == nil || !strings.Contains(err.Error(), "ERR_NAME_NOT_RESOLVED") {
		t.Fatalf("navigation error not surfaced: %v", err)
	}
}

func TestDetachedResetsTabState(t *testing.T) {
	m, _, tab := setup(t)
	tab.mu.Lock()
	tab.enabled["Page"] = true
	tab.uids["e1"] = uidRef{backendID: 1}
	tab.mu.Unlock()
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "__detached"})
	tab.mu.Lock()
	defer tab.mu.Unlock()
	if len(tab.enabled) != 0 || len(tab.uids) != 0 {
		t.Fatal("detach must reset enabled domains and uids")
	}
}

func TestEnsureDomainOnlyOnce(t *testing.T) {
	m, f, tab := setup(t)
	if err := m.EnsurePage(context.Background(), tab); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsurePage(context.Background(), tab); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == "Page.enable" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Page.enable sent %d times, want 1", n)
	}
}

func TestSelectOptionErrorsListOptions(t *testing.T) {
	m, f, tab := setup(t)
	tab.uids["e5"] = uidRef{backendID: 5}
	f.onValue("DOM.resolveNode", map[string]any{"object": map[string]any{"objectId": "obj1"}})
	f.on("Runtime.callFunctionOn", func(json.RawMessage) (any, error) {
		return map[string]any{"exceptionDetails": map[string]any{
			"exception": map[string]any{"description": "Error: no option matched: x — available: a, b"}}}, nil
	})
	_, err := m.SelectOption(context.Background(), tab, "e5", []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "available: a, b") {
		t.Fatalf("expected available-options error, got %v", err)
	}
}

func TestUploadRequiresExistingFile(t *testing.T) {
	m, _, tab := setup(t)
	tab.uids["e9"] = uidRef{backendID: 9}
	err := m.UploadFile(context.Background(), tab, "e9", []string{"/does/not/exist.png"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected file-not-found, got %v", err)
	}
}

// ---- OOPIF (cross-origin iframe) support ----

func attachIframe(m *Manager, sess, target, url string) {
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Target.attachedToTarget",
		Params: json.RawMessage(fmt.Sprintf(
			`{"sessionId":%q,"targetInfo":{"targetId":%q,"type":"iframe","url":%q},"waitingForDebugger":false}`,
			sess, target, url))})
}

func TestIframeSessionLifecycle(t *testing.T) {
	m, f, tab := setup(t)
	attachIframe(m, "sessA", "frame1", "https://pay.example/checkout")

	tab.mu.Lock()
	fr := tab.frames["sessA"]
	tab.mu.Unlock()
	if fr == nil || fr.Target != "frame1" || fr.URL != "https://pay.example/checkout" || fr.Parent != "" {
		t.Fatalf("frame not tracked: %+v", fr)
	}
	// The child session is armed asynchronously: Page for dialog safety,
	// nested auto-attach for iframes inside iframes.
	f.waitCalled(t, "sessA|Page.enable")
	f.waitCalled(t, "sessA|Target.setAutoAttach")

	// Non-iframe targets (workers) are ignored.
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Target.attachedToTarget",
		Params: json.RawMessage(`{"sessionId":"sessW","targetInfo":{"targetId":"w1","type":"worker","url":"x"}}`)})
	tab.mu.Lock()
	_, worker := tab.frames["sessW"]
	tab.mu.Unlock()
	if worker {
		t.Fatal("worker target must not be tracked as a frame")
	}

	// URL updates propagate (frame navigated within the iframe).
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Target.targetInfoChanged",
		Params: json.RawMessage(`{"targetInfo":{"targetId":"frame1","type":"iframe","url":"https://pay.example/3ds"}}`)})
	if got := tab.FrameURLForUID("nope"); got != "" {
		t.Fatalf("unknown uid must report no frame, got %q", got)
	}
	tab.mu.Lock()
	url := tab.frames["sessA"].URL
	tab.mu.Unlock()
	if url != "https://pay.example/3ds" {
		t.Fatalf("frame URL not updated: %s", url)
	}

	// Detach drops the frame; uids pointing at it become stale errors.
	tab.mu.Lock()
	tab.uids["f1.e42"] = uidRef{session: "sessA", backendID: 42}
	tab.mu.Unlock()
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, Method: "Target.detachedFromTarget",
		Params: json.RawMessage(`{"sessionId":"sessA"}`)})
	tab.mu.Lock()
	_, still := tab.frames["sessA"]
	tab.mu.Unlock()
	if still {
		t.Fatal("detached frame still tracked")
	}
	if err := m.Click(context.Background(), tab, "f1.e42", false); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("click on dead-frame uid must demand a fresh snapshot, got %v", err)
	}
}

const mainAXWithIframe = `{"nodes":[
  {"nodeId":"1","ignored":false,"role":{"value":"RootWebArea"},"name":{"value":"Host"},"childIds":["2","3"],"backendDOMNodeId":100},
  {"nodeId":"2","parentId":"1","ignored":false,"role":{"value":"Iframe"},"name":{"value":""},"childIds":[],"backendDOMNodeId":200},
  {"nodeId":"3","parentId":"1","ignored":false,"role":{"value":"button"},"name":{"value":"Host button"},"childIds":[],"backendDOMNodeId":101}
]}`

const childAX = `{"nodes":[
  {"nodeId":"1","ignored":false,"role":{"value":"RootWebArea"},"name":{"value":"Widget"},"childIds":["2"],"backendDOMNodeId":1},
  {"nodeId":"2","parentId":"1","ignored":false,"role":{"value":"button"},"name":{"value":"Pay now"},"childIds":[],"backendDOMNodeId":42}
]}`

func setupIframePage(t *testing.T) (*Manager, *fakeCaller, *Tab) {
	m, f, tab := setup(t)
	attachIframe(m, "sessA", "frame1", "https://pay.example/checkout")
	f.on("Accessibility.getFullAXTree", func(json.RawMessage) (any, error) {
		var v map[string]any
		json.Unmarshal(json.RawMessage(mainAXWithIframe), &v)
		return v, nil
	})
	f.on("sessA|Accessibility.getFullAXTree", func(json.RawMessage) (any, error) {
		var v map[string]any
		json.Unmarshal(json.RawMessage(childAX), &v)
		return v, nil
	})
	f.onValue("DOM.getFrameOwner", map[string]any{"backendNodeId": 200})
	return m, f, tab
}

func TestSnapshotGraftsIframeAndRoutesClicks(t *testing.T) {
	m, f, tab := setupIframePage(t)
	// Owner iframe box on the main page: top-left (100, 200).
	f.onValue("DOM.getBoxModel", map[string]any{"model": map[string]any{
		"content": []float64{100, 200, 400, 200, 400, 500, 100, 500}, "width": 300, "height": 300}})
	// Element box inside the iframe (frame-local coordinates).
	f.onSess("sessA", "DOM.getBoxModel", map[string]any{"model": map[string]any{
		"content": []float64{10, 10, 30, 10, 30, 20, 10, 20}, "width": 20, "height": 10}})

	snap, err := m.Snapshot(context.Background(), tab)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Iframe [https://pay.example/checkout]`,
		`button "Pay now" [uid=f1.e42]`,
		`button "Host button" [uid=e101]`,
	} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, snap)
		}
	}
	if got := tab.FrameURLForUID("f1.e42"); got != "https://pay.example/checkout" {
		t.Fatalf("FrameURLForUID = %q", got)
	}
	if got := tab.FrameURLForUID("e101"); got != "" {
		t.Fatalf("main-frame uid must have no frame URL, got %q", got)
	}

	// Clicking the iframe element: geometry from the child session plus the
	// owner offset, input dispatched on the MAIN session.
	var events []map[string]any
	var mu sync.Mutex
	f.on("Input.dispatchMouseEvent", func(p json.RawMessage) (any, error) {
		var v map[string]any
		json.Unmarshal(p, &v)
		mu.Lock()
		events = append(events, v)
		mu.Unlock()
		return map[string]any{}, nil
	})
	if err := m.Click(context.Background(), tab, "f1.e42", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("want move+press+release, got %d", len(events))
	}
	// local center (20, 15) + iframe offset (100, 200)
	if events[1]["x"] != 120.0 || events[1]["y"] != 215.0 {
		t.Fatalf("click missed OOPIF element: %v", events[1])
	}
	if !f.calledWith("sessA|DOM.getBoxModel") {
		t.Fatal("element geometry must come from the child session")
	}
	if f.calledWith("sessA|Input.dispatchMouseEvent") {
		t.Fatal("input must be dispatched on the main session (browser hit-testing)")
	}
}

func TestSnapshotSurvivesBrokenIframe(t *testing.T) {
	m, f, tab := setupIframePage(t)
	f.on("sessA|Accessibility.getFullAXTree", func(json.RawMessage) (any, error) {
		return nil, fmt.Errorf("target crashed")
	})
	snap, err := m.Snapshot(context.Background(), tab)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap, "content unavailable") || !strings.Contains(snap, "pay.example") {
		t.Fatalf("broken iframe must be visible in the snapshot:\n%s", snap)
	}
	if !strings.Contains(snap, `button "Host button" [uid=e101]`) {
		t.Fatalf("main content must survive a broken iframe:\n%s", snap)
	}
}

func TestIframeDialogAnsweredOnItsSession(t *testing.T) {
	m, f, tab := setup(t)
	attachIframe(m, "sessA", "frame1", "https://pay.example/checkout")
	got := make(chan string, 1)
	f.on("sessA|Page.handleJavaScriptDialog", func(json.RawMessage) (any, error) {
		got <- "child"
		return map[string]any{}, nil
	})
	f.on("Page.handleJavaScriptDialog", func(json.RawMessage) (any, error) {
		got <- "main"
		return map[string]any{}, nil
	})
	m.HandleEvent(bridge.Event{Profile: "test", TabID: 1, SessionID: "sessA",
		Method: "Page.javascriptDialogOpening",
		Params: json.RawMessage(`{"message":"sure?","type":"confirm"}`)})
	select {
	case who := <-got:
		if who != "child" {
			t.Fatalf("dialog answered on %s session, must be the child's", who)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("iframe dialog never answered")
	}
	if d := tab.LastDialog(); d == nil || d.Message != "sure?" {
		t.Fatalf("iframe dialog not recorded: %+v", d)
	}
}
