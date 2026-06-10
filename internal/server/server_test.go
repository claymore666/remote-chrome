package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"remote-chrome/internal/audit"
	"remote-chrome/internal/bridge"
	"remote-chrome/internal/browser"
	"remote-chrome/internal/config"
	"remote-chrome/internal/perms"
)

// fakeBrowser implements browser.Caller + Bridger: a minimal simulated
// Chrome with one tab on https://www.example.com.
type fakeBrowser struct {
	mu          sync.Mutex
	profiles    []string
	tabURL      string
	cdpCalls    []string
	detachAll   int
	articleMode bool // extraction script "finds" an article
	iframeMode  bool // main AX tree contains an OOPIF; child session answers
}

func newFakeBrowser() *fakeBrowser {
	return &fakeBrowser{profiles: []string{"personal"}, tabURL: "https://www.example.com/page"}
}

func (f *fakeBrowser) Profiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.profiles...)
}

func (f *fakeBrowser) DetachAll(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachAll++
}

func (f *fakeBrowser) Detach(ctx context.Context, profile string, tabID int) error { return nil }

func (f *fakeBrowser) cdpCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cdpCalls)
}

const axTreeJSON = `{"nodes":[
  {"nodeId":"1","ignored":false,"role":{"value":"RootWebArea"},"name":{"value":"Example"},"childIds":["2"],"backendDOMNodeId":1},
  {"nodeId":"2","parentId":"1","ignored":false,"role":{"value":"button"},"name":{"value":"Go"},"childIds":[],"backendDOMNodeId":42}
]}`

const axTreeWithIframeJSON = `{"nodes":[
  {"nodeId":"1","ignored":false,"role":{"value":"RootWebArea"},"name":{"value":"Example"},"childIds":["2","3"],"backendDOMNodeId":1},
  {"nodeId":"2","parentId":"1","ignored":false,"role":{"value":"button"},"name":{"value":"Go"},"childIds":[],"backendDOMNodeId":42},
  {"nodeId":"3","parentId":"1","ignored":false,"role":{"value":"Iframe"},"name":{"value":""},"childIds":[],"backendDOMNodeId":200}
]}`

const iframeAXJSON = `{"nodes":[
  {"nodeId":"1","ignored":false,"role":{"value":"RootWebArea"},"name":{"value":"Pay"},"childIds":["2"],"backendDOMNodeId":1},
  {"nodeId":"2","parentId":"1","ignored":false,"role":{"value":"button"},"name":{"value":"Pay now"},"childIds":[],"backendDOMNodeId":42}
]}`

func (f *fakeBrowser) CDP(ctx context.Context, profile string, tabID int, sessionID, method string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	f.cdpCalls = append(f.cdpCalls, method)
	iframeMode := f.iframeMode
	f.mu.Unlock()
	switch method {
	case "Accessibility.getFullAXTree":
		if sessionID != "" {
			return json.RawMessage(iframeAXJSON), nil
		}
		if iframeMode {
			return json.RawMessage(axTreeWithIframeJSON), nil
		}
		return json.RawMessage(axTreeJSON), nil
	case "DOM.getFrameOwner":
		return json.RawMessage(`{"backendNodeId":200}`), nil
	case "Runtime.evaluate":
		var p struct {
			Expression string `json:"expression"`
		}
		raw, _ := json.Marshal(params)
		json.Unmarshal(raw, &p)
		if strings.Contains(p.Expression, "__remote-chrome_extract__") {
			f.mu.Lock()
			isArticle := f.articleMode
			f.mu.Unlock()
			if !isArticle {
				return json.RawMessage(`{"result":{"value":null}}`), nil
			}
			return json.RawMessage(`{"result":{"value":{"title":"Fake Article","byline":"A. Author","markdown":"# Fake Article\n\nSome **markdown** body."}}}`), nil
		}
		return json.RawMessage(`{"result":{"value":"complete"}}`), nil
	case "Page.navigate":
		var p struct {
			URL string `json:"url"`
		}
		raw, _ := json.Marshal(params)
		json.Unmarshal(raw, &p)
		f.mu.Lock()
		f.tabURL = p.URL
		f.mu.Unlock()
		return json.RawMessage(`{"frameId":"f1"}`), nil
	case "Page.captureScreenshot":
		img := base64.StdEncoding.EncodeToString([]byte("fake-jpeg-bytes"))
		return json.RawMessage(fmt.Sprintf(`{"data":%q}`, img)), nil
	case "DOM.getBoxModel":
		return json.RawMessage(`{"model":{"content":[0,0,10,0,10,10,0,10],"width":10,"height":10}}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (f *fakeBrowser) Tabs(ctx context.Context, profile string, op string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	url := f.tabURL
	f.mu.Unlock()
	switch op {
	case "list":
		return json.RawMessage(fmt.Sprintf(`[{"tabId":1,"url":%q,"title":"Example","active":true,"attached":false}]`, url)), nil
	case "get":
		return json.RawMessage(fmt.Sprintf(`{"tabId":1,"url":%q,"title":"Example","active":true}`, url)), nil
	case "create":
		return json.RawMessage(`{"tabId":2}`), nil
	case "activate", "close":
		return json.RawMessage(`true`), nil
	}
	return nil, fmt.Errorf("no fake for tabs.%s", op)
}

// elicitScript scripts the "human": a queue of decisions returned to
// successive elicitations.
type elicitScript struct {
	mu        sync.Mutex
	decisions []string // for grant dialogs
	approvals []bool   // for boolean confirm dialogs
	count     int
	messages  []string
}

func (e *elicitScript) handler(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.count++
	e.messages = append(e.messages, req.Params.Message)
	// Boolean confirm (load_permission_set)?
	schema, _ := json.Marshal(req.Params.RequestedSchema)
	if strings.Contains(string(schema), `"approve"`) {
		if len(e.approvals) == 0 {
			return &mcp.ElicitResult{Action: "decline"}, nil
		}
		ok := e.approvals[0]
		e.approvals = e.approvals[1:]
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": ok}}, nil
	}
	if len(e.decisions) == 0 {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	d := e.decisions[0]
	e.decisions = e.decisions[1:]
	if d == "cancel" {
		return &mcp.ElicitResult{Action: "cancel"}, nil
	}
	if d == "" {
		// Some hosts (Claude Desktop) accept without any content.
		return &mcp.ElicitResult{Action: "accept"}, nil
	}
	return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": d}}, nil
}

type harness struct {
	srv     *Server
	fb      *fakeBrowser
	mgr     *browser.Manager
	script  *elicitScript
	session *mcp.ClientSession
	dir     string
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Approval: "auto", PermissionSet: "default", Token: "t"}
	if mutate != nil {
		mutate(cfg)
	}
	fb := newFakeBrowser()
	aud, err := audit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aud.Close() })
	mgr := browser.NewManager(fb, nil)
	srv := New(cfg, dir, fb, mgr, aud, nil)

	script := &elicitScript{}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"},
		&mcp.ClientOptions{ElicitationHandler: script.handler})

	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return &harness{srv: srv, fb: fb, mgr: mgr, script: script, session: session, dir: dir}
}

func (h *harness) call(t *testing.T, tool string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) transport error: %v", tool, err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return res, text
}

// ---- the prompt-injection wall ----

func TestGatedToolDeniedExecutesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"deny"}
	res, text := h.call(t, "snapshot", nil)
	if !res.IsError {
		t.Fatalf("denied snapshot must fail, got: %s", text)
	}
	if !strings.Contains(text, "permission denied") {
		t.Fatalf("denial must be a structured, actionable error: %s", text)
	}
	if h.fb.cdpCount() != 0 {
		t.Fatalf("NO CDP command may reach the browser on denial; got %v", h.fb.cdpCalls)
	}
	if h.script.count != 1 {
		t.Fatalf("expected exactly 1 elicitation, got %d", h.script.count)
	}
}

func TestSessionGrantPersistsOnceDoesNot(t *testing.T) {
	h := newHarness(t, nil)
	// "this session": second call needs no dialog.
	h.script.decisions = []string{"this session"}
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("snapshot after session grant failed: %s", txt)
	}
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("second snapshot failed: %s", txt)
	}
	if h.script.count != 1 {
		t.Fatalf("session grant must not re-prompt: %d elicitations", h.script.count)
	}

	// eval is a different action group -> new approval; "once" doesn't stick.
	h.script.decisions = []string{"once", "deny"}
	if res, txt := h.call(t, "eval_js", map[string]any{"expression": "1+1"}); res.IsError {
		t.Fatalf("eval after once grant failed: %s", txt)
	}
	if res, _ := h.call(t, "eval_js", map[string]any{"expression": "2+2"}); !res.IsError {
		t.Fatal("once grant must not persist to a second call")
	}
	if h.script.count != 3 {
		t.Fatalf("expected 3 elicitations total, got %d", h.script.count)
	}
}

func TestGrantIsDomainScoped(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session", "this session", "deny"}
	// navigate to example.com: grant #1 (navigate × example.com)
	if res, txt := h.call(t, "navigate", map[string]any{"url": "https://www.example.com/other"}); res.IsError {
		t.Fatalf("navigate failed: %s", txt)
	}
	// snapshot on example.com: grant #2 (read × example.com)
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("snapshot failed: %s", txt)
	}
	// navigate to a DIFFERENT domain: needs a fresh grant -> denied
	if res, _ := h.call(t, "navigate", map[string]any{"url": "https://evil.test/"}); !res.IsError {
		t.Fatal("grant for example.com must not cover evil.test")
	}
	for _, m := range h.script.messages {
		if strings.Contains(m, "navigate") && strings.Contains(m, "example.com") {
			return
		}
	}
	t.Fatalf("approval message should name action and domain: %v", h.script.messages)
}

func TestActionGroupsAreIndependent(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session", "deny"}
	if res, txt := h.call(t, "snapshot", nil); res.IsError { // read granted
		t.Fatalf("snapshot failed: %s", txt)
	}
	// read grant must not allow interact on the same domain
	if res, _ := h.call(t, "click", map[string]any{"uid": "e42"}); !res.IsError {
		t.Fatal("read grant must not imply interact")
	}
}

func TestRequestPermissionBatchesOneDialog(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session"}
	res, txt := h.call(t, "request_permission", map[string]any{
		"actions": []string{"read", "navigate", "interact"},
		"domain":  "https://www.example.com/x",
		"reason":  "post your summary",
	})
	if res.IsError {
		t.Fatalf("request_permission failed: %s", txt)
	}
	if h.script.count != 1 {
		t.Fatalf("batch request must raise ONE dialog, got %d", h.script.count)
	}
	if !strings.Contains(h.script.messages[0], "post your summary") {
		t.Fatalf("reason missing from dialog: %q", h.script.messages[0])
	}
	// All three groups usable without further dialogs.
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("snapshot: %s", txt)
	}
	if res, txt := h.call(t, "click", map[string]any{"uid": "e42"}); res.IsError {
		t.Fatalf("click: %s", txt)
	}
	if h.script.count != 1 {
		t.Fatalf("no further dialogs expected, got %d", h.script.count)
	}
}

func TestSaveToSetPersistsGrant(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"save to set"}
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("snapshot: %s", txt)
	}
	set, err := perms.ReadSet(h.dir, "default")
	if err != nil {
		t.Fatalf("grant was not persisted to the default set: %v", err)
	}
	if len(set.Grants) != 1 || set.Grants[0].Action != perms.Read || set.Grants[0].Domain != "example.com" {
		t.Fatalf("bad persisted set: %+v", set.Grants)
	}
}

func TestLoadPermissionSetNeedsApproval(t *testing.T) {
	h := newHarness(t, nil)
	// Prepare a set on disk.
	m := perms.NewMatrix()
	m.Grant(perms.Read, "example.com")
	m.Grant(perms.Interact, "example.com")
	if _, err := m.SaveSet(h.dir, "proj"); err != nil {
		t.Fatal(err)
	}
	// Declined load: nothing applied.
	h.script.approvals = []bool{false}
	if res, _ := h.call(t, "load_permission_set", map[string]any{"name": "proj"}); !res.IsError {
		t.Fatal("declined set load must fail")
	}
	if len(h.srv.Matrix().List()) != 0 {
		t.Fatal("declined load must not apply grants")
	}
	// Approved load: one dialog arms everything.
	h.script.approvals = []bool{true}
	if res, txt := h.call(t, "load_permission_set", map[string]any{"name": "proj"}); res.IsError {
		t.Fatalf("approved load failed: %s", txt)
	}
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("snapshot after set load should need no dialog: %s", txt)
	}
	if res, txt := h.call(t, "click", map[string]any{"uid": "e42"}); res.IsError {
		t.Fatalf("click after set load should need no dialog: %s", txt)
	}
}

func TestFreeToolsNeedNoApproval(t *testing.T) {
	h := newHarness(t, nil) // empty decision script: any elicitation would decline
	for _, tool := range []string{"list_tabs", "list_profiles", "list_permissions", "list_permission_sets", "diagnostics", "get_url"} {
		res, txt := h.call(t, tool, nil)
		if res.IsError {
			t.Fatalf("%s must be free, got error: %s", tool, txt)
		}
	}
	if h.script.count != 0 {
		t.Fatalf("free tools must not elicit, got %d dialogs", h.script.count)
	}
}

func TestRemoveAndResetPermissions(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session", "deny"}
	h.call(t, "snapshot", nil) // grants read × example.com
	if _, txt := h.call(t, "list_permissions", nil); !strings.Contains(txt, "example.com") {
		t.Fatalf("grant missing from list: %s", txt)
	}
	h.call(t, "remove_permission", map[string]any{"action": "read", "domain": "example.com"})
	if res, _ := h.call(t, "snapshot", nil); !res.IsError {
		t.Fatal("after remove, snapshot must need a new grant")
	}
}

func TestNavigateDenyList(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.DenyNavigation = []string{"evil.com"} })
	h.script.decisions = []string{"this session"} // would approve if asked — must not be asked
	res, txt := h.call(t, "navigate", map[string]any{"url": "https://www.evil.com/x"})
	if !res.IsError || !strings.Contains(txt, "deny_navigation") {
		t.Fatalf("deny-listed navigation must fail regardless of grants: %s", txt)
	}
	if h.script.count != 0 {
		t.Fatal("deny list must short-circuit before any dialog")
	}
}

func TestNavigateRefusesNonWebSchemes(t *testing.T) {
	h := newHarness(t, nil)
	for _, u := range []string{"chrome://settings", "file:///etc/passwd", "javascript:alert(1)"} {
		res, _ := h.call(t, "navigate", map[string]any{"url": u})
		if !res.IsError {
			t.Fatalf("navigate to %q must be refused", u)
		}
	}
	if h.fb.cdpCount() != 0 {
		t.Fatal("refused navigation must not touch the browser")
	}
}

func TestProfileTargetingIsGated(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.DefaultProfile = "personal" })
	h.fb.mu.Lock()
	h.fb.profiles = []string{"personal", "work"}
	h.fb.mu.Unlock()

	// Default profile: only the read grant dialog.
	h.script.decisions = []string{"this session"}
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("snapshot on default profile: %s", txt)
	}
	// Other profile: profile grant dialog + (already granted) read.
	h.script.decisions = []string{"deny"}
	if res, txt := h.call(t, "snapshot", map[string]any{"profile": "work"}); !res.IsError {
		t.Fatalf("targeting another profile must require a profile grant: %s", txt)
	}
	found := false
	for _, msg := range h.script.messages {
		if strings.Contains(msg, "profile") && strings.Contains(msg, "work") {
			found = true
		}
	}
	if !found {
		t.Fatalf("profile grant dialog missing: %v", h.script.messages)
	}
}

func TestKillSwitch(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session"}
	h.call(t, "snapshot", nil)
	if len(h.srv.Matrix().List()) == 0 {
		t.Fatal("precondition: a grant exists")
	}
	res, txt := h.call(t, "kill_switch", nil)
	if res.IsError {
		t.Fatalf("kill_switch: %s", txt)
	}
	if h.fb.detachAll != 1 {
		t.Fatal("kill_switch must detach all debugger sessions")
	}
	if len(h.srv.Matrix().List()) != 0 {
		t.Fatal("kill_switch must clear the matrix")
	}
}

func TestScreenshotReturnsImage(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session"}
	res, txt := h.call(t, "screenshot", nil)
	if res.IsError {
		t.Fatalf("screenshot: %s", txt)
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("expected ImageContent, got %T", res.Content[0])
	}
	if img.MIMEType != "image/jpeg" || string(img.Data) != "fake-jpeg-bytes" {
		t.Fatalf("bad image: %s %d bytes", img.MIMEType, len(img.Data))
	}
}

func TestSnapshotReturnsUIDs(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session"}
	res, txt := h.call(t, "snapshot", nil)
	if res.IsError {
		t.Fatalf("snapshot: %s", txt)
	}
	if !strings.Contains(txt, `button "Go" [uid=e42]`) {
		t.Fatalf("snapshot missing interactive uid: %s", txt)
	}
}

func TestReadPageArticleMode(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session"}
	h.fb.mu.Lock()
	h.fb.articleMode = true
	h.fb.mu.Unlock()

	res, txt := h.call(t, "read_page", map[string]any{"mode": "article"})
	if res.IsError {
		t.Fatalf("read_page article: %s", txt)
	}
	for _, want := range []string{"Fake Article", "A. Author", "**markdown**", "(article extraction)"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("article output missing %q: %s", want, txt)
		}
	}

	// Non-article page: extraction returns null -> innerText fallback + note.
	h.fb.mu.Lock()
	h.fb.articleMode = false
	h.fb.mu.Unlock()
	res, txt = h.call(t, "read_page", map[string]any{"mode": "article"})
	if res.IsError || !strings.Contains(txt, "no article-like main content detected") {
		t.Fatalf("article fallback missing: %s", txt)
	}

	// Unknown mode is an error.
	if res, _ := h.call(t, "read_page", map[string]any{"mode": "yaml"}); !res.IsError {
		t.Fatal("unknown mode must error")
	}
}

func TestReadPageWithScreenshot(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session"}
	res, txt := h.call(t, "read_page", map[string]any{"with_screenshot": true})
	if res.IsError {
		t.Fatalf("read_page with screenshot: %s", txt)
	}
	if len(res.Content) != 2 {
		t.Fatalf("want text + image, got %d contents", len(res.Content))
	}
	img, ok := res.Content[1].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/jpeg" {
		t.Fatalf("second content should be a jpeg, got %T", res.Content[1])
	}
}

func TestCancelledElicitationDenies(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"cancel"}
	res, _ := h.call(t, "snapshot", nil)
	if !res.IsError {
		t.Fatal("cancelled dialog must deny")
	}
	if h.fb.cdpCount() != 0 {
		t.Fatal("no CDP on cancel")
	}
}

func TestAuditTrailWritten(t *testing.T) {
	h := newHarness(t, nil)
	h.script.decisions = []string{"this session"}
	h.call(t, "navigate", map[string]any{"url": "https://www.example.com/x"})
	// The audit log is flushed per write; read it back.
	data, err := readFile(h.dir + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind":"approval"`, `"kind":"grant"`, `"kind":"tool"`, `"tool":"navigate"`, `"decision":"this session"`} {
		if !strings.Contains(data, want) {
			t.Errorf("audit log missing %s:\n%s", want, data)
		}
	}
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

// ---- per-frame gating (OOPIF; issue #1) ----

// TestIframeInteractionNeedsFrameGrant: a grant for the host page must NOT
// cover an embedded third-party frame — acting inside it raises its own
// approval, and a denial stops the action before any input reaches Chrome.
func TestIframeInteractionNeedsFrameGrant(t *testing.T) {
	h := newHarness(t, nil)
	h.fb.mu.Lock()
	h.fb.iframeMode = true
	h.fb.mu.Unlock()
	// Chrome auto-attached the OOPIF (https://pay.example inside example.com).
	h.mgr.HandleEvent(bridge.Event{Profile: "personal", TabID: 1, Method: "Target.attachedToTarget",
		Params: json.RawMessage(`{"sessionId":"sessA","targetInfo":{"targetId":"frame1","type":"iframe","url":"https://pay.example/checkout"}}`)})

	// read × example.com shows the iframe's content — perception covers
	// what the user already sees on the page.
	h.script.decisions = []string{"this session"}
	res, snap := h.call(t, "snapshot", nil)
	if res.IsError {
		t.Fatalf("snapshot: %s", snap)
	}
	if !strings.Contains(snap, `button "Pay now" [uid=f1.e42]`) {
		t.Fatalf("iframe content missing from snapshot:\n%s", snap)
	}

	// interact × example.com is granted, interact × pay.example is DENIED:
	// the click must fail and nothing may reach the browser.
	h.script.decisions = []string{"this session", "deny"}
	before := h.fb.cdpCount()
	res, txt := h.call(t, "click", map[string]any{"uid": "f1.e42"})
	if !res.IsError {
		t.Fatal("click inside an ungranted frame must fail")
	}
	if !strings.Contains(txt, "pay.example") {
		t.Fatalf("denial must name the frame domain: %s", txt)
	}
	h.fb.mu.Lock()
	calls := append([]string(nil), h.fb.cdpCalls[before:]...)
	h.fb.mu.Unlock()
	for _, c := range calls {
		if strings.HasPrefix(c, "Input.") || strings.HasPrefix(c, "DOM.") {
			t.Fatalf("denied frame click leaked %s to the browser", c)
		}
	}
	if len(h.script.messages) < 3 || !strings.Contains(h.script.messages[2], "pay.example") {
		t.Fatalf("frame approval dialog must name the frame domain: %v", h.script.messages)
	}

	// With the frame grant, the click goes through.
	h.script.decisions = []string{"this session"}
	res, txt = h.call(t, "click", map[string]any{"uid": "f1.e42"})
	if res.IsError {
		t.Fatalf("click after frame grant failed: %s", txt)
	}
}

// TestAcceptedApprovalWithEmptyDecisionUsesDefault: some MCP hosts (Claude
// Desktop) submit empty content for single-field elicitation schemas.
// Accept means accept — the suggested default granularity applies: session
// for read-ish groups, once for the risky ones. Never more than suggested.
func TestAcceptedApprovalWithEmptyDecisionUsesDefault(t *testing.T) {
	h := newHarness(t, nil)

	// read × example.com, empty decision -> session default: no re-prompt.
	h.script.decisions = []string{""}
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("snapshot after empty-decision accept failed: %s", txt)
	}
	if res, txt := h.call(t, "snapshot", nil); res.IsError {
		t.Fatalf("second snapshot failed: %s", txt)
	}
	if h.script.count != 1 {
		t.Fatalf("session default must not re-prompt: %d elicitations", h.script.count)
	}

	// eval × example.com, empty decision -> once default: second call
	// re-prompts (and a decline then denies).
	h.script.decisions = []string{"", "deny"}
	if res, txt := h.call(t, "eval_js", map[string]any{"expression": "1"}); res.IsError {
		t.Fatalf("eval after empty-decision accept failed: %s", txt)
	}
	if res, _ := h.call(t, "eval_js", map[string]any{"expression": "2"}); !res.IsError {
		t.Fatal("once default must not persist to a second eval")
	}
}
