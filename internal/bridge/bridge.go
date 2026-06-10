package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxMessageSize   = 32 << 20 // CDP screenshots come back base64 in one frame
	helloTimeout     = 5 * time.Second
	heartbeatPeriod  = 20 * time.Second // keeps the MV3 service worker alive
	writeTimeout     = 10 * time.Second
	defaultCallLimit = 60 * time.Second
)

var (
	ErrNoProfile    = errors.New("no such profile connected")
	ErrNotConnected = errors.New("no browser extension connected")
	ErrCallTimeout  = errors.New("extension call timed out")
	ErrDisconnected = errors.New("extension disconnected while call was pending")
)

// EventHandler receives CDP events forwarded by extensions. Called from the
// connection's read loop — must not block.
type EventHandler func(ev Event)

// ConnHook is notified when a profile connects/disconnects.
type ConnHook func(profile string, connected bool)

// Bridge owns the localhost WebSocket listener and one connection per
// connected Chrome profile.
type Bridge struct {
	token         string
	pinnedOrigins []string // exact chrome-extension://<id> origins; empty = any extension origin
	log           *slog.Logger
	verbose       bool

	mu       sync.RWMutex
	profiles map[string]*profileConn

	onEvent atomic.Value // EventHandler
	onConn  atomic.Value // ConnHook

	server *http.Server
	ln     net.Listener
}

type profileConn struct {
	label   string
	ws      *websocket.Conn
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan inbound
	nextID    atomic.Int64

	closed chan struct{}
}

func New(token string, pinnedOrigins []string, verbose bool, log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	return &Bridge{
		token:         token,
		pinnedOrigins: pinnedOrigins,
		verbose:       verbose,
		log:           log,
		profiles:      map[string]*profileConn{},
	}
}

func (b *Bridge) SetEventHandler(h EventHandler) { b.onEvent.Store(h) }
func (b *Bridge) SetConnHook(h ConnHook)         { b.onConn.Store(h) }

// Start binds 127.0.0.1:port (port 0 = ephemeral) and serves the /ws
// endpoint in the background. Returns the bound port.
func (b *Bridge) Start(port int) (int, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 0, fmt.Errorf("bind control port: %w", err)
	}
	b.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", b.handleWS)
	b.server = &http.Server{Handler: mux}
	go func() {
		if err := b.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.log.Error("bridge http server stopped", "err", err)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (b *Bridge) Close() error {
	b.mu.Lock()
	for _, pc := range b.profiles {
		pc.ws.Close()
	}
	b.mu.Unlock()
	if b.server != nil {
		return b.server.Close()
	}
	return nil
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 << 10,
	WriteBufferSize: 64 << 10,
	// Origin is checked explicitly in handleWS so we can log rejections.
	CheckOrigin: func(*http.Request) bool { return true },
}

func (b *Bridge) originAllowed(origin string) bool {
	if !strings.HasPrefix(origin, "chrome-extension://") {
		return false
	}
	if len(b.pinnedOrigins) == 0 {
		return true
	}
	for _, o := range b.pinnedOrigins {
		if origin == o {
			return true
		}
	}
	return false
}

func (b *Bridge) handleWS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !b.originAllowed(origin) {
		b.log.Warn("rejected connection: bad origin", "origin", origin, "remote", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxMessageSize)

	// First message must be a valid hello within the timeout.
	ws.SetReadDeadline(time.Now().Add(helloTimeout))
	var hello Hello
	if err := ws.ReadJSON(&hello); err != nil || hello.Type != "hello" {
		b.log.Warn("rejected connection: no hello", "err", err)
		ws.Close()
		return
	}
	ws.SetReadDeadline(time.Time{})

	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(b.token)) != 1 {
		b.log.Warn("rejected connection: bad token", "profile", hello.Profile)
		ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "bad token"), time.Now().Add(time.Second))
		ws.Close()
		return
	}
	if hello.ProtocolVersion != ProtocolVersion {
		b.log.Warn("rejected connection: protocol mismatch",
			"profile", hello.Profile, "theirs", hello.ProtocolVersion, "ours", ProtocolVersion)
		ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "protocol version mismatch"), time.Now().Add(time.Second))
		ws.Close()
		return
	}
	label := hello.Profile
	if label == "" {
		label = "default"
	}

	pc := &profileConn{
		label:   label,
		ws:      ws,
		pending: map[int64]chan inbound{},
		closed:  make(chan struct{}),
	}

	b.mu.Lock()
	if old, ok := b.profiles[label]; ok {
		// A reconnect (e.g. service worker restarted) replaces the old conn.
		old.ws.Close()
	}
	b.profiles[label] = pc
	b.mu.Unlock()

	b.log.Info("extension connected", "profile", label, "origin", origin, "extVersion", hello.ExtensionVersion)
	if h, ok := b.onConn.Load().(ConnHook); ok && h != nil {
		h(label, true)
	}

	go b.heartbeat(pc)
	b.readLoop(pc) // blocks until the connection dies
}

func (b *Bridge) readLoop(pc *profileConn) {
	defer func() {
		close(pc.closed)
		pc.ws.Close()
		b.mu.Lock()
		if b.profiles[pc.label] == pc {
			delete(b.profiles, pc.label)
		}
		b.mu.Unlock()
		// Fail all pending calls.
		pc.pendingMu.Lock()
		for id, ch := range pc.pending {
			delete(pc.pending, id)
			close(ch)
		}
		pc.pendingMu.Unlock()
		b.log.Info("extension disconnected", "profile", pc.label)
		if h, ok := b.onConn.Load().(ConnHook); ok && h != nil {
			h(pc.label, false)
		}
	}()

	for {
		_, data, err := pc.ws.ReadMessage()
		if err != nil {
			return
		}
		var msg inbound
		if err := json.Unmarshal(data, &msg); err != nil {
			b.log.Warn("bad message from extension", "profile", pc.label, "err", err)
			continue
		}
		switch msg.Type {
		case "": // response to a call
			pc.pendingMu.Lock()
			ch, ok := pc.pending[msg.ID]
			if ok {
				delete(pc.pending, msg.ID)
			}
			pc.pendingMu.Unlock()
			if ok {
				ch <- msg
			}
		case "event":
			if b.verbose {
				b.log.Debug("cdp event", "profile", pc.label, "tab", msg.TabID, "method", msg.Method)
			}
			b.dispatchEvent(Event{Profile: pc.label, TabID: msg.TabID, Method: msg.Method, Params: msg.Params})
		case "detached":
			b.log.Info("debugger detached", "profile", pc.label, "tab", msg.TabID, "reason", msg.Reason)
			b.dispatchEvent(Event{Profile: pc.label, TabID: msg.TabID, Method: "__detached"})
		case "log":
			b.log.Info("extension log", "profile", pc.label, "level", msg.Level, "msg", msg.Message)
		}
	}
}

func (b *Bridge) dispatchEvent(ev Event) {
	if h, ok := b.onEvent.Load().(EventHandler); ok && h != nil {
		h(ev)
	}
}

func (b *Bridge) heartbeat(pc *profileConn) {
	t := time.NewTicker(heartbeatPeriod)
	defer t.Stop()
	for {
		select {
		case <-pc.closed:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			_, err := b.callConn(ctx, pc, Request{Type: "ping"})
			cancel()
			if err != nil {
				pc.ws.Close()
				return
			}
		}
	}
}

// Profiles returns the labels of currently connected profiles, sorted by
// connection map iteration (callers sort if they need stable order).
func (b *Bridge) Profiles() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.profiles))
	for label := range b.profiles {
		out = append(out, label)
	}
	return out
}

func (b *Bridge) conn(profile string) (*profileConn, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.profiles) == 0 {
		return nil, ErrNotConnected
	}
	pc, ok := b.profiles[profile]
	if !ok {
		return nil, fmt.Errorf("%w: %q (connected: %s)", ErrNoProfile, profile, strings.Join(b.Profiles(), ", "))
	}
	return pc, nil
}

// Call sends a request to the given profile's extension and waits for the
// response. A zero ctx deadline gets a 60s default.
func (b *Bridge) Call(ctx context.Context, profile string, req Request) (json.RawMessage, error) {
	pc, err := b.conn(profile)
	if err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCallLimit)
		defer cancel()
	}
	return b.callConn(ctx, pc, req)
}

func (b *Bridge) callConn(ctx context.Context, pc *profileConn, req Request) (json.RawMessage, error) {
	req.ID = pc.nextID.Add(1)
	ch := make(chan inbound, 1)
	pc.pendingMu.Lock()
	pc.pending[req.ID] = ch
	pc.pendingMu.Unlock()
	cleanup := func() {
		pc.pendingMu.Lock()
		delete(pc.pending, req.ID)
		pc.pendingMu.Unlock()
	}

	if b.verbose && req.Type != "ping" {
		b.log.Debug("-> extension", "profile", pc.label, "type", req.Type, "tab", req.TabID,
			"method", req.Method, "params", string(req.Params))
	}

	pc.writeMu.Lock()
	pc.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	err := pc.ws.WriteJSON(req)
	pc.writeMu.Unlock()
	if err != nil {
		cleanup()
		pc.ws.Close()
		return nil, fmt.Errorf("write to extension: %w", err)
	}

	select {
	case <-ctx.Done():
		cleanup()
		return nil, fmt.Errorf("%w (%s %s)", ErrCallTimeout, req.Type, req.Method)
	case <-pc.closed:
		return nil, ErrDisconnected
	case msg, ok := <-ch:
		if !ok {
			return nil, ErrDisconnected
		}
		if msg.Error != "" {
			return nil, fmt.Errorf("extension: %s", msg.Error)
		}
		if b.verbose && req.Type != "ping" {
			b.log.Debug("<- extension", "profile", pc.label, "id", req.ID, "resultBytes", len(msg.Result))
		}
		return msg.Result, nil
	}
}

// CDP relays a CDP command to a tab. params may be nil.
func (b *Bridge) CDP(ctx context.Context, profile string, tabID int, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = data
	}
	return b.Call(ctx, profile, Request{Type: "cdp", TabID: tabID, Method: method, Params: raw})
}

// Tabs relays a chrome.tabs operation.
func (b *Bridge) Tabs(ctx context.Context, profile string, op string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = data
	}
	return b.Call(ctx, profile, Request{Type: "tabs", Method: op, Params: raw})
}

// Detach asks the extension to detach the debugger from one tab.
func (b *Bridge) Detach(ctx context.Context, profile string, tabID int) error {
	_, err := b.Call(ctx, profile, Request{Type: "detach", TabID: tabID})
	return err
}

// DetachAll severs every debugger session on every connected profile.
// Used by the kill switch and on shutdown; best-effort.
func (b *Bridge) DetachAll(ctx context.Context) {
	for _, label := range b.Profiles() {
		if _, err := b.Call(ctx, label, Request{Type: "detach_all"}); err != nil {
			b.log.Warn("detach_all failed", "profile", label, "err", err)
		}
	}
}
