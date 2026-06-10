//go:build uat

// Package uat is the end-to-end / regression suite: it drives the REAL
// stack — Go server ↔ real extension ↔ real headless Chrome ↔ a local test
// page — through the MCP tool surface, with a scripted "human" answering
// the elicitation dialogs.
//
// Run: make test-uat   (or: go test -tags uat ./test/uat -v)
// Requires google-chrome/chromium and the extension's node_modules
// (cd extension && npm install).
package uat

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"browserd/internal/audit"
	"browserd/internal/bridge"
	"browserd/internal/browser"
	"browserd/internal/config"
	"browserd/internal/server"
)

const testPage = `<!DOCTYPE html>
<html><head><title>browserd UAT page</title></head><body>
<h1>UAT fixture</h1>
<p id="status">clicks: 0</p>
<button id="btn" onclick="document.getElementById('status').textContent='clicks: '+(++window.__clicks||(window.__clicks=1))">Click me</button>
<form onsubmit="event.preventDefault();document.getElementById('greet').textContent='hello '+document.getElementById('name').value">
  <label>Name <input id="name" type="text"></label>
  <button type="submit">Greet</button>
</form>
<p id="greet"></p>
<select id="pet"><option value="">choose</option><option value="cat">Cat</option><option value="dog">Dog</option></select>
<input type="checkbox" id="agree"><label for="agree">I agree</label>
<a href="/page2">go to page two</a>
<button id="logbtn" onclick="console.log('uat-console-message', 7)">Log</button>
<button id="alertbtn" onclick="window.__alertDone=confirm('proceed?')">Alert</button>
<button id="latebtn" onclick="setTimeout(()=>{const d=document.createElement('div');d.id='late';d.textContent='late content';document.body.appendChild(d)},300)">Load late</button>
</body></html>`

const page2 = `<!DOCTYPE html><html><head><title>page two</title></head><body><h1>Second page</h1></body></html>`

const articlePage = `<!DOCTYPE html>
<html><head><title>The Bridge Pattern — browserd blog</title><meta name="author" content="C. Kamien"></head><body>
<nav><a href="/">Home</a> <a href="/about">About</a> <a href="/archive">Archive</a> <a href="/contact">Contact</a></nav>
<div id="cookiebanner">We use cookies to improve your experience. <button>Accept cookies</button></div>
<article>
  <h1>The Bridge Pattern</h1>
  <p>Chrome stopped allowing remote debugging on default profiles in version 136, which means every
     traditional automation stack silently lost access to the browser people actually use every day.
     The extension bridge restores that access without weakening the browser's security posture.</p>
  <h2>Why an extension</h2>
  <p>The <code>chrome.debugger</code> API is exempt from the restriction and grants nearly full protocol
     access from inside the user's real profile. See <a href="/docs">the documentation</a> for details
     on which protocol domains remain restricted in that mode.</p>
  <ul><li>real input events</li><li>visible attach banner</li><li>no process management</li></ul>
  <h2>Numbers</h2>
  <table><tr><th>approach</th><th>works on default profile</th></tr>
  <tr><td>raw CDP</td><td>no</td></tr><tr><td>extension bridge</td><td>yes</td></tr></table>
  <p>The result is an automation path that the user can always see, always interrupt, and always audit
     after the fact, which is precisely the property a permission-gated agent needs.</p>
</article>
<footer>© 2026 browserd — <a href="/imprint">Imprint</a> <a href="/privacy">Privacy</a></footer>
</body></html>`

// findChrome prefers Chrome for Testing / Chromium: Google-branded Chrome
// ignores --load-extension since 137, so the branded binary cannot host the
// test extension (real users sideload via chrome://extensions instead).
// Install: npx -y @puppeteer/browsers install chrome@stable --path ~/.cache/browserd-uat
func findChrome() string {
	if c := os.Getenv("CHROME_BIN"); c != "" {
		return c
	}
	if home, err := os.UserHomeDir(); err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, ".cache", "browserd-uat", "chrome", "linux-*", "chrome-linux64", "chrome"))
		if len(matches) > 0 {
			return matches[len(matches)-1]
		}
	}
	for _, name := range []string{"chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// repoRoot walks up from this file to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// buildTestExtension builds the self-configuring extension variant into a
// temp dir (manifest + dist with __TEST_CONFIG__ baked in).
func buildTestExtension(t *testing.T, port int, token string) string {
	t.Helper()
	root := repoRoot(t)
	extSrc := filepath.Join(root, "extension")
	if _, err := os.Stat(filepath.Join(extSrc, "node_modules")); err != nil {
		t.Skip("extension/node_modules missing — run: cd extension && npm install")
	}
	dir := t.TempDir()
	for _, f := range []string{"manifest.json", "options.html"} {
		data, err := os.ReadFile(filepath.Join(extSrc, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := json.Marshal(map[string]any{"port": port, "token": token, "profile": "uat"})
	cmd := exec.Command("node", "build.mjs")
	cmd.Dir = extSrc
	cmd.Env = append(os.Environ(),
		"BROWSERD_TEST_CONFIG="+string(cfg),
		"BROWSERD_EXT_OUTDIR="+filepath.Join(dir, "dist"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("extension build failed: %v\n%s", err, out)
	}
	return dir
}

// autoApprover answers every elicitation with "this session" / approve and
// counts the dialogs.
type autoApprover struct{ count atomic.Int32 }

func (a *autoApprover) handler(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	a.count.Add(1)
	schema, _ := json.Marshal(req.Params.RequestedSchema)
	if strings.Contains(string(schema), `"approve"`) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	}
	return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": "this session"}}, nil
}

type uatHarness struct {
	session  *mcp.ClientSession
	approver *autoApprover
	pageURL  string
	br       *bridge.Bridge
}

func startUAT(t *testing.T) *uatHarness {
	t.Helper()
	chrome := findChrome()
	if chrome == "" {
		t.Skip("no Chrome/Chromium found (set CHROME_BIN)")
	}

	// Local fixture site.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, testPage) })
	mux.HandleFunc("/page2", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, page2) })
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, articlePage) })
	site := httptest.NewServer(mux)
	t.Cleanup(site.Close)

	// Bridge + manager + MCP server, in process.
	token, err := config.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	cfg := &config.Config{Token: token, Approval: "elicit", PermissionSet: "default"}
	aud, err := audit.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aud.Close() })

	br := bridge.New(token, nil, testing.Verbose(), nil)
	mgr := browser.NewManager(br, nil)
	br.SetEventHandler(mgr.HandleEvent)
	port, err := br.Start(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { br.Close() })

	srv := server.New(cfg, stateDir, br, mgr, aud, nil)
	approver := &autoApprover{}
	client := mcp.NewClient(&mcp.Implementation{Name: "uat", Version: "0"},
		&mcp.ClientOptions{ElicitationHandler: approver.handler})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.MCP.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	// Real Chrome with the self-configuring test extension. The profile dir
	// is managed manually (not t.TempDir): Chrome's helper processes keep
	// writing during shutdown, which makes the framework's RemoveAll race
	// and flake with "directory not empty".
	extDir := buildTestExtension(t, port, token)
	profileDir, err := os.MkdirTemp("", "browserd-uat-profile-*")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--headless=new",
		"--user-data-dir=" + profileDir,
		"--load-extension=" + extDir,
		"--no-first-run", "--no-default-browser-check",
		"--disable-background-networking",
		"--remote-allow-origins=*",
		"about:blank",
	}
	if os.Getenv("UAT_NO_SANDBOX") != "" {
		args = append([]string{"--no-sandbox"}, args...)
	}
	cmd := exec.Command(chrome, args...)
	if testing.Verbose() {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		// Chrome's children may outlive the main process briefly; retry.
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(profileDir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		os.RemoveAll(profileDir) // last try; a leftover tmp dir is not a test failure
	})

	// Wait for the extension to connect.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range br.Profiles() {
			if p == "uat" {
				return &uatHarness{session: session, approver: approver, pageURL: site.URL, br: br}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("extension never connected to the bridge (headless Chrome + MV3 service worker)")
	return nil
}

func (h *uatHarness) call(t *testing.T, tool string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return res, text
}

func (h *uatHarness) must(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	res, text := h.call(t, tool, args)
	if res.IsError {
		t.Fatalf("%s failed: %s", tool, text)
	}
	return text
}

// uidFor extracts the uid of the first snapshot line containing marker.
func uidFor(t *testing.T, snapshot, marker string) string {
	t.Helper()
	for _, line := range strings.Split(snapshot, "\n") {
		if strings.Contains(line, marker) {
			if i := strings.Index(line, "[uid="); i >= 0 {
				return strings.TrimSuffix(line[i+5:strings.Index(line, "]")], "")
			}
		}
	}
	t.Fatalf("no uid for %q in snapshot:\n%s", marker, snapshot)
	return ""
}

func TestUAT(t *testing.T) {
	h := startUAT(t)

	t.Run("navigate", func(t *testing.T) {
		out := h.must(t, "navigate", map[string]any{"url": h.pageURL})
		if !strings.Contains(out, "browserd UAT page") {
			t.Fatalf("navigate result missing title: %s", out)
		}
	})

	var snap string
	t.Run("snapshot", func(t *testing.T) {
		snap = h.must(t, "snapshot", nil)
		for _, want := range []string{`heading "UAT fixture"`, `button "Click me" [uid=`, `link "go to page two" [uid=`} {
			if !strings.Contains(snap, want) {
				t.Fatalf("snapshot missing %q:\n%s", want, snap)
			}
		}
	})

	t.Run("click", func(t *testing.T) {
		h.must(t, "click", map[string]any{"uid": uidFor(t, snap, `button "Click me"`)})
		h.must(t, "wait_for", map[string]any{"condition": "text", "value": "clicks: 1"})
		out := h.must(t, "read_page", nil)
		if !strings.Contains(out, "clicks: 1") {
			t.Fatalf("click did not register: %s", out)
		}
	})

	t.Run("type_and_submit", func(t *testing.T) {
		h.must(t, "type", map[string]any{"uid": uidFor(t, snap, `textbox "Name"`), "text": "world"})
		h.must(t, "click", map[string]any{"uid": uidFor(t, snap, `button "Greet"`)})
		h.must(t, "wait_for", map[string]any{"condition": "text", "value": "hello world"})
	})

	t.Run("select_and_check", func(t *testing.T) {
		out := h.must(t, "select_option", map[string]any{"uid": uidFor(t, snap, "combobox"), "values": []string{"Dog"}})
		if !strings.Contains(out, "dog") {
			t.Fatalf("select failed: %s", out)
		}
		h.must(t, "check", map[string]any{"uid": uidFor(t, snap, `checkbox "I agree"`)})
		fresh := h.must(t, "snapshot", nil)
		if !strings.Contains(fresh, "checked") {
			t.Fatalf("checkbox not checked:\n%s", fresh)
		}
	})

	t.Run("wait_for_late_content", func(t *testing.T) {
		fresh := h.must(t, "snapshot", nil)
		h.must(t, "click", map[string]any{"uid": uidFor(t, fresh, `button "Load late"`)})
		h.must(t, "wait_for", map[string]any{"condition": "selector", "value": "#late", "timeout_s": 5})
	})

	t.Run("screenshot", func(t *testing.T) {
		res, _ := h.call(t, "screenshot", nil)
		if res.IsError {
			t.Fatal("screenshot failed")
		}
		img, ok := res.Content[0].(*mcp.ImageContent)
		if !ok || img.MIMEType != "image/jpeg" || len(img.Data) < 1000 {
			t.Fatalf("expected a real jpeg, got %T %d bytes", res.Content[0], len(img.Data))
		}
	})

	t.Run("console", func(t *testing.T) {
		h.must(t, "read_console", nil) // start capture
		fresh := h.must(t, "snapshot", nil)
		h.must(t, "click", map[string]any{"uid": uidFor(t, fresh, `button "Log"`)})
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if out := h.must(t, "read_console", nil); strings.Contains(out, "uat-console-message") {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatal("console message never captured")
	})

	t.Run("dialog_auto_dismissed", func(t *testing.T) {
		fresh := h.must(t, "snapshot", nil)
		h.must(t, "click", map[string]any{"uid": uidFor(t, fresh, `button "Alert"`)})
		time.Sleep(500 * time.Millisecond) // dialog fires + auto-handles
		out := h.must(t, "handle_dialog", map[string]any{"accept": true})
		if !strings.Contains(out, "confirm") || !strings.Contains(out, "proceed?") {
			t.Fatalf("auto-dismissed dialog not reported: %s", out)
		}
		// page still responsive (the hang-prevention property)
		h.must(t, "read_page", nil)
	})

	t.Run("eval_js", func(t *testing.T) {
		out := h.must(t, "eval_js", map[string]any{"expression": "window.__alertDone"})
		if !strings.Contains(out, "false") {
			t.Fatalf("confirm() should have been dismissed -> false, got %s", out)
		}
	})

	t.Run("history", func(t *testing.T) {
		h.must(t, "navigate", map[string]any{"url": h.pageURL + "/page2"})
		out := h.must(t, "back", nil)
		if !strings.Contains(out, "browserd UAT page") {
			t.Fatalf("back failed: %s", out)
		}
		out = h.must(t, "forward", nil)
		if !strings.Contains(out, "page two") {
			t.Fatalf("forward failed: %s", out)
		}
	})

	t.Run("tabs", func(t *testing.T) {
		out := h.must(t, "new_tab", map[string]any{"url": h.pageURL})
		var newTab int
		fmt.Sscanf(out, "opened tab %d", &newTab)
		if newTab == 0 {
			t.Fatalf("no tab id: %s", out)
		}
		list := h.must(t, "list_tabs", nil)
		if !strings.Contains(list, `"uat"`) || strings.Count(list, "tabId") < 2 {
			t.Fatalf("list_tabs: %s", list)
		}
		h.must(t, "close_tab", map[string]any{"tab_id": newTab})
	})

	t.Run("read_page_article", func(t *testing.T) {
		h.must(t, "navigate", map[string]any{"url": h.pageURL + "/article"})
		out := h.must(t, "read_page", map[string]any{"mode": "article"})
		for _, want := range []string{
			"(article extraction)",
			"by C. Kamien",
			"## Why an extension",       // headings survive as markdown
			"](" + h.pageURL + "/docs)", // links survive, absolutized
			"- real input events",       // lists survive
			"| raw CDP | no |",          // tables survive
			"`chrome.debugger`",         // inline code survives
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("article markdown missing %q:\n%s", want, out)
			}
		}
		for _, junk := range []string{"Accept cookies", "Archive", "Imprint"} {
			if strings.Contains(out, junk) {
				t.Fatalf("article markdown contains noise %q:\n%s", junk, out)
			}
		}
	})

	t.Run("read_page_article_fallback", func(t *testing.T) {
		h.must(t, "navigate", map[string]any{"url": h.pageURL + "/page2"}) // no article content
		out := h.must(t, "read_page", map[string]any{"mode": "article"})
		if !strings.Contains(out, "no article-like main content detected") || !strings.Contains(out, "Second page") {
			t.Fatalf("fallback to text missing: %s", out)
		}
	})

	t.Run("read_page_with_screenshot", func(t *testing.T) {
		res, txt := h.call(t, "read_page", map[string]any{"mode": "article", "with_screenshot": true})
		if res.IsError {
			t.Fatalf("read_page with screenshot: %s", txt)
		}
		var img *mcp.ImageContent
		for _, c := range res.Content {
			if ic, ok := c.(*mcp.ImageContent); ok {
				img = ic
			}
		}
		if img == nil || img.MIMEType != "image/jpeg" || len(img.Data) < 1000 {
			t.Fatalf("expected text + real jpeg in one result, contents: %d", len(res.Content))
		}
	})

	t.Run("permissions_were_elicited", func(t *testing.T) {
		if h.approver.count.Load() == 0 {
			t.Fatal("expected elicitation dialogs during UAT")
		}
		out := h.must(t, "list_permissions", nil)
		if !strings.Contains(out, "127.0.0.1") {
			t.Fatalf("matrix should hold grants for the fixture host: %s", out)
		}
	})

	t.Run("kill_switch_then_recover", func(t *testing.T) {
		h.must(t, "kill_switch", nil)
		out := h.must(t, "list_permissions", nil)
		if strings.Contains(out, "127.0.0.1") {
			t.Fatalf("kill_switch must clear grants: %s", out)
		}
		before := h.approver.count.Load()
		h.must(t, "navigate", map[string]any{"url": h.pageURL}) // re-elicits + re-attaches
		h.must(t, "snapshot", nil)
		if h.approver.count.Load() <= before {
			t.Fatal("post-kill actions must require fresh approvals")
		}
	})
}

// TestBinarySetup covers cmd/browserd's setup path end to end.
func TestBinarySetup(t *testing.T) {
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "browserd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/browserd")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	stateDir := t.TempDir()
	cmd := exec.Command(bin, "setup")
	cmd.Env = append(os.Environ(), "BROWSERD_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browserd setup: %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"Port", "Token", "chrome://extensions", stateDir} {
		if !strings.Contains(s, want) {
			t.Fatalf("setup output missing %q:\n%s", want, s)
		}
	}
	// Config was created with a persisted port + 0600 perms.
	cfgPath := filepath.Join(stateDir, "config.toml")
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms: %v", info.Mode().Perm())
	}
}
