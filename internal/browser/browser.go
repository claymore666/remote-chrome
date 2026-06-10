// Package browser implements high-level page operations (snapshot, click,
// navigate, wait) on top of the bridge's raw CDP relay. It owns per-tab
// state: uid registries, console/network buffers, dialog auto-handling and
// idle detach.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"remote-chrome/internal/bridge"
)

const (
	consoleBufCap = 200
	networkBufCap = 200
	idleDetachAge = 2 * time.Minute
)

type tabKey struct {
	profile string
	tabID   int
}

// Caller is the subset of bridge.Bridge the manager needs; an interface so
// module tests can fake the extension side without a WebSocket.
type Caller interface {
	CDP(ctx context.Context, profile string, tabID int, sessionID, method string, params any) (json.RawMessage, error)
	Tabs(ctx context.Context, profile string, op string, params any) (json.RawMessage, error)
	Detach(ctx context.Context, profile string, tabID int) error
	Profiles() []string
}

type Manager struct {
	b   Caller
	log *slog.Logger

	mu   sync.Mutex
	tabs map[tabKey]*Tab
}

// Tab holds remote-chrome's view of one browser tab.
type Tab struct {
	Profile string
	ID      int

	mu          sync.Mutex
	enabled     map[string]bool   // "<sessionID>\x00<Domain>" we've sent <Domain>.enable for
	uids        map[string]uidRef // snapshot uid -> owning session + backendDOMNodeId
	snapshotGen int
	loadFired   bool
	autoAttach  bool                  // Target.setAutoAttach armed this attach lifetime
	frames      map[string]*frameInfo // child sessionId -> out-of-process iframe target
	frameSeq    int                   // ordinal source for uid prefixes ("f2.e17")

	console  []ConsoleEntry
	network  []*NetworkEntry
	inflight map[string]*NetworkEntry // CDP requestId -> entry (in-flight)

	lastDialog   *DialogInfo
	dialogAccept bool   // policy for the next auto-handled dialog
	dialogText   string // prompt text for the next accepted prompt

	lastUsed time.Time
}

// uidRef locates a snapshot element: the debugger session owning the node
// (empty = the tab's main session) plus its backendDOMNodeId.
type uidRef struct {
	session   string
	backendID int
}

// frameInfo tracks one auto-attached out-of-process iframe target.
// Same-process iframes never appear here — they live in the main session's
// tree like any other DOM.
type frameInfo struct {
	Session string // CDP session id (flat routing)
	Parent  string // parent session id; empty = main session
	Target  string // targetId — equals the frameId for iframe targets
	URL     string // frame document URL (permission gating source)
	Idx     int    // stable ordinal for uid prefixes
}

type ConsoleEntry struct {
	Time time.Time `json:"ts"`
	Type string    `json:"type"`
	Text string    `json:"text"`
}

type NetworkEntry struct {
	Time     time.Time `json:"ts"`
	Method   string    `json:"method"`
	URL      string    `json:"url"`
	Status   int       `json:"status,omitempty"`
	Type     string    `json:"resourceType,omitempty"`
	Failed   string    `json:"failed,omitempty"`
	Finished bool      `json:"finished"`
}

type DialogInfo struct {
	Time     time.Time `json:"ts"`
	Type     string    `json:"type"` // alert | confirm | prompt | beforeunload
	Message  string    `json:"message"`
	Accepted bool      `json:"accepted"`
}

func NewManager(b Caller, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{b: b, log: log, tabs: map[tabKey]*Tab{}}
}

// Tab returns (creating if needed) the state for profile/tabID.
func (m *Manager) Tab(profile string, tabID int) *Tab {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := tabKey{profile, tabID}
	t, ok := m.tabs[k]
	if !ok {
		t = &Tab{
			Profile:  profile,
			ID:       tabID,
			enabled:  map[string]bool{},
			uids:     map[string]uidRef{},
			frames:   map[string]*frameInfo{},
			inflight: map[string]*NetworkEntry{},
			lastUsed: time.Now(),
		}
		m.tabs[k] = t
	}
	return t
}

func (m *Manager) dropTab(profile string, tabID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tabs, tabKey{profile, tabID})
}

// DropProfile forgets all tab state for a disconnected profile.
func (m *Manager) DropProfile(profile string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.tabs {
		if k.profile == profile {
			delete(m.tabs, k)
		}
	}
}

// CDP relays a command on the tab's main session and refreshes its activity
// clock.
func (m *Manager) CDP(ctx context.Context, t *Tab, method string, params any) (json.RawMessage, error) {
	return m.cdp(ctx, t, "", method, params)
}

// cdp relays a command on a specific session (empty = main).
func (m *Manager) cdp(ctx context.Context, t *Tab, sessionID, method string, params any) (json.RawMessage, error) {
	t.mu.Lock()
	t.lastUsed = time.Now()
	t.mu.Unlock()
	return m.b.CDP(ctx, t.Profile, t.ID, sessionID, method, params)
}

// ensureDomain sends <domain>.enable once per attach lifetime and session;
// Page is required for dialog auto-handling and load events, Runtime for
// console, Network for the request log and network-idle waits,
// Accessibility for snapshots.
func (m *Manager) ensureDomain(ctx context.Context, t *Tab, sessionID, domain string) error {
	key := sessionID + "\x00" + domain
	t.mu.Lock()
	already := t.enabled[key]
	t.mu.Unlock()
	if already {
		return nil
	}
	if _, err := m.cdp(ctx, t, sessionID, domain+".enable", nil); err != nil {
		return fmt.Errorf("enable %s: %w", domain, err)
	}
	t.mu.Lock()
	t.enabled[key] = true
	t.mu.Unlock()
	return nil
}

// EnsurePage must precede any interaction so dialogs can never hang the
// session (unhandled alert/confirm/beforeunload block all further CDP).
// It also arms auto-attach so out-of-process iframes become reachable
// child sessions before the first snapshot.
func (m *Manager) EnsurePage(ctx context.Context, t *Tab) error {
	if err := m.ensureDomain(ctx, t, "", "Page"); err != nil {
		return err
	}
	return m.ensureAutoAttach(ctx, t)
}

// ensureAutoAttach arms flat auto-attach on the main session: Chrome then
// reports every out-of-process iframe as a child session
// (Target.attachedToTarget). Once per attach lifetime; reset on detach.
func (m *Manager) ensureAutoAttach(ctx context.Context, t *Tab) error {
	t.mu.Lock()
	already := t.autoAttach
	t.mu.Unlock()
	if already {
		return nil
	}
	if _, err := m.cdp(ctx, t, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true,
	}); err != nil {
		return fmt.Errorf("auto-attach to iframe targets: %w", err)
	}
	t.mu.Lock()
	t.autoAttach = true
	t.mu.Unlock()
	return nil
}

// HandleEvent consumes bridge events. Wired as the bridge's EventHandler;
// must not block (spawns goroutines for replies).
func (m *Manager) HandleEvent(ev bridge.Event) {
	if ev.Method == "__detached" {
		m.handleDetached(ev.Profile, ev.TabID)
		return
	}
	t := m.Tab(ev.Profile, ev.TabID)
	switch ev.Method {
	case "Target.attachedToTarget":
		m.handleAttachedTarget(t, ev.SessionID, ev.Params)
		return
	case "Target.detachedFromTarget":
		m.handleDetachedTarget(t, ev.Params)
		return
	case "Target.targetInfoChanged":
		m.handleTargetInfoChanged(t, ev.Params)
		return
	case "Page.javascriptDialogOpening":
		// Dialogs are tab-modal but the event fires on the opening frame's
		// session — the reply must go back on that same session.
		m.handleDialog(t, ev.SessionID, ev.Params)
		return
	}
	if ev.SessionID != "" {
		// Child-session load/console/network events are not buffered (v1);
		// the buffers describe the top-level page.
		return
	}
	switch ev.Method {
	case "Page.loadEventFired":
		t.mu.Lock()
		t.loadFired = true
		t.mu.Unlock()
	case "Page.frameStartedLoading":
		t.mu.Lock()
		t.loadFired = false
		t.mu.Unlock()
	case "Runtime.consoleAPICalled":
		m.handleConsole(t, ev.Params)
	case "Network.requestWillBeSent":
		m.handleRequestStart(t, ev.Params)
	case "Network.responseReceived":
		m.handleResponse(t, ev.Params)
	case "Network.loadingFinished":
		m.handleRequestEnd(t, ev.Params, "")
	case "Network.loadingFailed":
		m.handleRequestEnd(t, ev.Params, "failed")
	}
}

func (m *Manager) handleDetached(profile string, tabID int) {
	m.mu.Lock()
	t, ok := m.tabs[tabKey{profile, tabID}]
	m.mu.Unlock()
	if !ok {
		return
	}
	// Attach state (enabled domains, snapshot uids, child sessions) died
	// with the debugger session; reset so the next use re-arms everything.
	t.mu.Lock()
	t.enabled = map[string]bool{}
	t.uids = map[string]uidRef{}
	t.frames = map[string]*frameInfo{}
	t.autoAttach = false
	t.inflight = map[string]*NetworkEntry{}
	t.mu.Unlock()
}

// handleAttachedTarget registers an auto-attached OOPIF child session and
// arms it like the main session: Page.enable so an iframe's JS dialog can
// never hang the tab, and nested auto-attach for iframes inside iframes.
func (m *Manager) handleAttachedTarget(t *Tab, parent string, params json.RawMessage) {
	var p struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfo"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return
	}
	if p.TargetInfo.Type != "iframe" {
		return // workers and friends are not page content
	}
	t.mu.Lock()
	t.frameSeq++
	t.frames[p.SessionID] = &frameInfo{
		Session: p.SessionID,
		Parent:  parent,
		Target:  p.TargetInfo.TargetID,
		URL:     p.TargetInfo.URL,
		Idx:     t.frameSeq,
	}
	t.mu.Unlock()
	m.log.Info("iframe target attached", "profile", t.Profile, "tab", t.ID, "url", p.TargetInfo.URL)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.ensureDomain(ctx, t, p.SessionID, "Page"); err != nil {
			m.log.Warn("enable Page on iframe session", "tab", t.ID, "err", err)
			return
		}
		if _, err := m.cdp(ctx, t, p.SessionID, "Target.setAutoAttach", map[string]any{
			"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true,
		}); err != nil {
			m.log.Warn("auto-attach on iframe session", "tab", t.ID, "err", err)
		}
	}()
}

func (m *Manager) handleDetachedTarget(t *Tab, params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return
	}
	t.mu.Lock()
	t.removeFrameLocked(p.SessionID)
	t.mu.Unlock()
}

// removeFrameLocked drops a child session and any frames nested inside it
// (their detach events may arrive on the now-dead parent session, or never).
func (t *Tab) removeFrameLocked(sessionID string) {
	delete(t.frames, sessionID)
	for s, f := range t.frames {
		if f.Parent == sessionID {
			t.removeFrameLocked(s)
		}
	}
}

func (m *Manager) handleTargetInfoChanged(t *Tab, params json.RawMessage) {
	var p struct {
		TargetInfo struct {
			TargetID string `json:"targetId"`
			URL      string `json:"url"`
		} `json:"targetInfo"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.TargetInfo.TargetID == "" {
		return
	}
	t.mu.Lock()
	for _, f := range t.frames {
		if f.Target == p.TargetInfo.TargetID {
			f.URL = p.TargetInfo.URL
		}
	}
	t.mu.Unlock()
}

// handleDialog auto-answers JS dialogs per the tab's policy (default:
// dismiss) and records them so Claude can see what happened. PLAN §4:
// unhandled dialogs hang the CDP session, so this must always respond —
// on the session the dialog fired on (an OOPIF's dialog must be answered
// via the OOPIF's session).
func (m *Manager) handleDialog(t *Tab, sessionID string, params json.RawMessage) {
	var p struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	_ = json.Unmarshal(params, &p)

	t.mu.Lock()
	accept, text := t.dialogAccept, t.dialogText
	t.dialogAccept, t.dialogText = false, "" // policy is one-shot, back to dismiss
	t.lastDialog = &DialogInfo{Time: time.Now(), Type: p.Type, Message: p.Message, Accepted: accept}
	t.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		args := map[string]any{"accept": accept}
		if accept && text != "" {
			args["promptText"] = text
		}
		if _, err := m.cdp(ctx, t, sessionID, "Page.handleJavaScriptDialog", args); err != nil {
			m.log.Warn("handle dialog failed", "profile", t.Profile, "tab", t.ID, "err", err)
		}
	}()
	m.log.Info("auto-handled js dialog", "type", p.Type, "accepted", accept, "message", p.Message)
}

// SetDialogPolicy arms the response for the NEXT dialog on this tab.
func (t *Tab) SetDialogPolicy(accept bool, promptText string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dialogAccept, t.dialogText = accept, promptText
}

// LastDialog returns the most recent auto-handled dialog, if any.
func (t *Tab) LastDialog() *DialogInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastDialog
}

func (m *Manager) handleConsole(t *Tab, params json.RawMessage) {
	var p struct {
		Type string `json:"type"`
		Args []struct {
			Value       any    `json:"value"`
			Description string `json:"description"`
		} `json:"args"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	text := ""
	for i, a := range p.Args {
		if i > 0 {
			text += " "
		}
		if a.Value != nil {
			text += fmt.Sprintf("%v", a.Value)
		} else {
			text += a.Description
		}
	}
	t.mu.Lock()
	t.console = append(t.console, ConsoleEntry{Time: time.Now(), Type: p.Type, Text: truncate(text, 2000)})
	if len(t.console) > consoleBufCap {
		t.console = t.console[len(t.console)-consoleBufCap:]
	}
	t.mu.Unlock()
}

func (m *Manager) handleRequestStart(t *Tab, params json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Type      string `json:"type"`
		Request   struct {
			URL    string `json:"url"`
			Method string `json:"method"`
		} `json:"request"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	e := &NetworkEntry{Time: time.Now(), Method: p.Request.Method, URL: truncate(p.Request.URL, 500), Type: p.Type}
	t.mu.Lock()
	t.inflight[p.RequestID] = e
	t.network = append(t.network, e)
	if len(t.network) > networkBufCap {
		t.network = t.network[len(t.network)-networkBufCap:]
	}
	t.mu.Unlock()
}

func (m *Manager) handleResponse(t *Tab, params json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Response  struct {
			Status int `json:"status"`
		} `json:"response"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	t.mu.Lock()
	if e, ok := t.inflight[p.RequestID]; ok {
		e.Status = p.Response.Status
	}
	t.mu.Unlock()
}

func (m *Manager) handleRequestEnd(t *Tab, params json.RawMessage, failed string) {
	var p struct {
		RequestID string `json:"requestId"`
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	t.mu.Lock()
	if e, ok := t.inflight[p.RequestID]; ok {
		e.Finished = true
		if failed != "" {
			e.Failed = p.ErrorText
			if e.Failed == "" {
				e.Failed = "failed"
			}
		}
		delete(t.inflight, p.RequestID)
	}
	t.mu.Unlock()
}

func (t *Tab) inflightCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inflight)
}

// Console returns a copy of the buffered console entries.
func (t *Tab) Console() []ConsoleEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ConsoleEntry(nil), t.console...)
}

// Network returns a copy of the buffered request log.
func (t *Tab) Network() []NetworkEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]NetworkEntry, len(t.network))
	for i, e := range t.network {
		out[i] = *e
	}
	return out
}

// StartIdleDetacher detaches debugger sessions from tabs untouched for
// idleDetachAge so Chrome's "is debugging" banner clears when Claude is done.
func (m *Manager) StartIdleDetacher(ctx context.Context) {
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				m.detachIdle(ctx)
			}
		}
	}()
}

func (m *Manager) detachIdle(ctx context.Context) {
	m.mu.Lock()
	var idle []*Tab
	for _, t := range m.tabs {
		t.mu.Lock()
		// Only tabs we actually attached to (some domain enabled) and idle.
		if len(t.enabled) > 0 && time.Since(t.lastUsed) > idleDetachAge {
			idle = append(idle, t)
		}
		t.mu.Unlock()
	}
	m.mu.Unlock()
	for _, t := range idle {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := m.b.Detach(dctx, t.Profile, t.ID); err == nil {
			m.log.Info("detached idle tab", "profile", t.Profile, "tab", t.ID)
		}
		cancel()
		// __detached event resets the tab state.
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}
