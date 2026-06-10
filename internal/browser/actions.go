package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"
	"unicode"
)

// Interaction tools dispatch real browser-level input events
// (Input.dispatchMouseEvent / dispatchKeyEvent), not synthetic DOM events,
// with modest randomized inter-event delays so dynamic pages (hover menus,
// debounced inputs) behave correctly. PLAN §4 "timing layer" — explicitly
// not for evading third-party bot detection.

func pause(minMS, maxMS int) {
	time.Sleep(time.Duration(minMS+rand.IntN(maxMS-minMS+1)) * time.Millisecond)
}

// center locates the visual center of an element in MAIN-viewport
// coordinates, scrolling it into view first. For elements inside an OOPIF
// the box model is relative to the iframe's own viewport, so the owner
// iframe's position in each ancestor frame is added (same correction
// puppeteer applies).
func (m *Manager) center(ctx context.Context, t *Tab, ref uidRef) (x, y float64, err error) {
	// Best-effort: not all nodes support scrollIntoViewIfNeeded.
	_, _ = m.cdp(ctx, t, ref.session, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": ref.backendID})
	q, err := m.boxContent(ctx, t, ref.session, ref.backendID)
	if err != nil {
		return 0, 0, err
	}
	x = (q[0] + q[2] + q[4] + q[6]) / 4
	y = (q[1] + q[3] + q[5] + q[7]) / 4
	offX, offY, err := m.frameOffset(ctx, t, ref.session)
	if err != nil {
		return 0, 0, err
	}
	return x + offX, y + offY, nil
}

// boxContent returns the 4-corner content quad of an element, in its own
// session's viewport coordinates.
func (m *Manager) boxContent(ctx context.Context, t *Tab, sessionID string, backendID int) ([]float64, error) {
	raw, err := m.cdp(ctx, t, sessionID, "DOM.getBoxModel", map[string]any{"backendNodeId": backendID})
	if err != nil {
		return nil, fmt.Errorf("element has no box (hidden or detached?): %w", err)
	}
	var resp struct {
		Model struct {
			Content []float64 `json:"content"` // x1 y1 x2 y2 x3 y3 x4 y4
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Model.Content) < 8 {
		return nil, fmt.Errorf("unexpected box model for node %d", backendID)
	}
	return resp.Model.Content, nil
}

// frameOffset accumulates the main-viewport position of a child session's
// coordinate origin by walking owner <iframe> boxes up the ancestor chain.
// Zero for the main session.
func (m *Manager) frameOffset(ctx context.Context, t *Tab, sessionID string) (x, y float64, err error) {
	for sessionID != "" {
		t.mu.Lock()
		f := t.frames[sessionID]
		t.mu.Unlock()
		if f == nil {
			return 0, 0, fmt.Errorf("iframe session went away — call snapshot again")
		}
		ownerID, err := m.frameOwner(ctx, t, f.Parent, f.Target)
		if err != nil {
			return 0, 0, err
		}
		q, err := m.boxContent(ctx, t, f.Parent, ownerID)
		if err != nil {
			return 0, 0, fmt.Errorf("iframe owner: %w", err)
		}
		x += q[0]
		y += q[1]
		sessionID = f.Parent
	}
	return x, y, nil
}

func (m *Manager) mouse(ctx context.Context, t *Tab, typ string, x, y float64, extra map[string]any) error {
	params := map[string]any{"type": typ, "x": x, "y": y}
	for k, v := range extra {
		params[k] = v
	}
	_, err := m.CDP(ctx, t, "Input.dispatchMouseEvent", params)
	return err
}

// Click moves the mouse to the element and performs a real click. Input
// events always go to the main session: the browser hit-tests them into the
// right frame, OOPIF or not (this is what makes them "real").
func (m *Manager) Click(ctx context.Context, t *Tab, uid string, doubleClick bool) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	ref, err := t.resolveUID(uid)
	if err != nil {
		return err
	}
	x, y, err := m.center(ctx, t, ref)
	if err != nil {
		return err
	}
	if err := m.mouse(ctx, t, "mouseMoved", x, y, nil); err != nil {
		return err
	}
	pause(40, 120)
	clicks := 1
	if doubleClick {
		clicks = 2
	}
	for i := 0; i < clicks; i++ {
		btn := map[string]any{"button": "left", "clickCount": i + 1}
		if err := m.mouse(ctx, t, "mousePressed", x, y, btn); err != nil {
			return err
		}
		pause(20, 60)
		if err := m.mouse(ctx, t, "mouseReleased", x, y, btn); err != nil {
			return err
		}
	}
	return nil
}

// Hover moves the pointer over the element (menus, tooltips).
func (m *Manager) Hover(ctx context.Context, t *Tab, uid string) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	ref, err := t.resolveUID(uid)
	if err != nil {
		return err
	}
	x, y, err := m.center(ctx, t, ref)
	if err != nil {
		return err
	}
	return m.mouse(ctx, t, "mouseMoved", x, y, nil)
}

// Type focuses the element, optionally clears it, and inserts text.
// pressEnter submits afterwards.
func (m *Manager) Type(ctx context.Context, t *Tab, uid, text string, clear, pressEnter bool) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	ref, err := t.resolveUID(uid)
	if err != nil {
		return err
	}
	if _, err := m.cdp(ctx, t, ref.session, "DOM.focus", map[string]any{"backendNodeId": ref.backendID}); err != nil {
		// Fall back to clicking the element to focus it.
		if cerr := m.Click(ctx, t, uid, false); cerr != nil {
			return fmt.Errorf("cannot focus element: %w", err)
		}
	}
	pause(30, 90)
	if clear {
		if err := m.PressKey(ctx, t, "a", []string{"Control"}); err != nil {
			return err
		}
		if err := m.PressKey(ctx, t, "Delete", nil); err != nil {
			return err
		}
	}
	if text != "" {
		if _, err := m.CDP(ctx, t, "Input.insertText", map[string]any{"text": text}); err != nil {
			return err
		}
	}
	if pressEnter {
		pause(40, 120)
		return m.PressKey(ctx, t, "Enter", nil)
	}
	return nil
}

type keyDef struct {
	key, code string
	keyCode   int
	text      string
}

var namedKeys = map[string]keyDef{
	"enter":      {"Enter", "Enter", 13, "\r"},
	"tab":        {"Tab", "Tab", 9, ""},
	"escape":     {"Escape", "Escape", 27, ""},
	"backspace":  {"Backspace", "Backspace", 8, ""},
	"delete":     {"Delete", "Delete", 46, ""},
	"arrowup":    {"ArrowUp", "ArrowUp", 38, ""},
	"arrowdown":  {"ArrowDown", "ArrowDown", 40, ""},
	"arrowleft":  {"ArrowLeft", "ArrowLeft", 37, ""},
	"arrowright": {"ArrowRight", "ArrowRight", 39, ""},
	"home":       {"Home", "Home", 36, ""},
	"end":        {"End", "End", 35, ""},
	"pageup":     {"PageUp", "PageUp", 33, ""},
	"pagedown":   {"PageDown", "PageDown", 34, ""},
	"space":      {" ", "Space", 32, " "},
}

var modifierBits = map[string]int{
	"alt": 1, "control": 2, "ctrl": 2, "meta": 4, "cmd": 4, "shift": 8,
}

// PressKey dispatches keyDown/keyUp for a named key ("Enter", "Tab",
// "ArrowDown", …) or a single character, with optional modifiers
// ("Control", "Shift", "Alt", "Meta").
func (m *Manager) PressKey(ctx context.Context, t *Tab, key string, modifiers []string) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	mod := 0
	for _, name := range modifiers {
		bit, ok := modifierBits[strings.ToLower(name)]
		if !ok {
			return fmt.Errorf("unknown modifier %q (use Control, Shift, Alt, Meta)", name)
		}
		mod |= bit
	}

	def, ok := namedKeys[strings.ToLower(key)]
	if !ok {
		runes := []rune(key)
		if len(runes) != 1 {
			return fmt.Errorf("unknown key %q — use a single character or one of: Enter, Tab, Escape, Backspace, Delete, ArrowUp/Down/Left/Right, Home, End, PageUp, PageDown, Space", key)
		}
		r := runes[0]
		def = keyDef{key: string(r), text: string(r), keyCode: int(unicode.ToUpper(r))}
		if unicode.IsLetter(r) && r < 128 {
			def.code = "Key" + strings.ToUpper(string(r))
		} else if unicode.IsDigit(r) {
			def.code = "Digit" + string(r)
		}
	}

	down := map[string]any{
		"type": "rawKeyDown", "modifiers": mod, "key": def.key, "code": def.code,
		"windowsVirtualKeyCode": def.keyCode, "nativeVirtualKeyCode": def.keyCode,
	}
	if def.text != "" && mod&^8 == 0 { // text only without ctrl/alt/meta
		down["type"] = "keyDown"
		down["text"] = def.text
	}
	if _, err := m.CDP(ctx, t, "Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	pause(15, 50)
	up := map[string]any{
		"type": "keyUp", "modifiers": mod, "key": def.key, "code": def.code,
		"windowsVirtualKeyCode": def.keyCode, "nativeVirtualKeyCode": def.keyCode,
	}
	_, err := m.CDP(ctx, t, "Input.dispatchKeyEvent", up)
	return err
}

// Scroll scrolls the page by a wheel event, or scrolls an element into view
// when uid is given.
func (m *Manager) Scroll(ctx context.Context, t *Tab, uid, direction string, amount int) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	if uid != "" {
		ref, err := t.resolveUID(uid)
		if err != nil {
			return err
		}
		_, err = m.cdp(ctx, t, ref.session, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": ref.backendID})
		return err
	}
	if amount <= 0 {
		amount = 600
	}
	var dx, dy int
	switch strings.ToLower(direction) {
	case "", "down":
		dy = amount
	case "up":
		dy = -amount
	case "right":
		dx = amount
	case "left":
		dx = -amount
	default:
		return fmt.Errorf("unknown scroll direction %q (up/down/left/right)", direction)
	}
	raw, err := m.CDP(ctx, t, "Page.getLayoutMetrics", nil)
	if err != nil {
		return err
	}
	var lm struct {
		CSSVisualViewport struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssVisualViewport"`
	}
	if err := json.Unmarshal(raw, &lm); err != nil {
		return err
	}
	return m.mouse(ctx, t, "mouseWheel",
		lm.CSSVisualViewport.ClientWidth/2, lm.CSSVisualViewport.ClientHeight/2,
		map[string]any{"deltaX": dx, "deltaY": dy})
}

// callOnNode runs a JS function with the element as `this`, returning the
// JSON value. Used for select/check where real input events are impractical.
// Runs on the session owning the node — for OOPIF elements that is the
// iframe's own session.
func (m *Manager) callOnNode(ctx context.Context, t *Tab, ref uidRef, fn string, args ...any) (json.RawMessage, error) {
	raw, err := m.cdp(ctx, t, ref.session, "DOM.resolveNode", map[string]any{"backendNodeId": ref.backendID})
	if err != nil {
		return nil, err
	}
	var resolved struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := json.Unmarshal(raw, &resolved); err != nil || resolved.Object.ObjectID == "" {
		return nil, fmt.Errorf("cannot resolve node %d", ref.backendID)
	}
	callArgs := make([]map[string]any, len(args))
	for i, a := range args {
		callArgs[i] = map[string]any{"value": a}
	}
	raw, err = m.cdp(ctx, t, ref.session, "Runtime.callFunctionOn", map[string]any{
		"objectId":            resolved.Object.ObjectID,
		"functionDeclaration": fn,
		"arguments":           callArgs,
		"returnByValue":       true,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.ExceptionDetails != nil {
		desc := resp.ExceptionDetails.Exception.Description
		if desc == "" {
			desc = resp.ExceptionDetails.Text
		}
		return nil, fmt.Errorf("page exception: %s", truncate(desc, 500))
	}
	return resp.Result.Value, nil
}

// SelectOption selects <option>s in a <select> by value or visible label.
func (m *Manager) SelectOption(ctx context.Context, t *Tab, uid string, values []string) (string, error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return "", err
	}
	ref, err := t.resolveUID(uid)
	if err != nil {
		return "", err
	}
	res, err := m.callOnNode(ctx, t, ref, `function(values) {
		const el = this.tagName === 'SELECT' ? this : this.closest('select');
		if (!el) throw new Error('element is not a <select>');
		const want = new Set(values);
		let hit = 0;
		for (const opt of el.options) {
			const match = want.has(opt.value) || want.has(opt.label) || want.has(opt.text.trim());
			if (el.multiple) { opt.selected = match; if (match) hit++; }
			else if (match) { el.value = opt.value; hit++; break; }
		}
		if (hit === 0) throw new Error('no option matched: ' + values.join(', ') +
			' — available: ' + Array.from(el.options).map(o => o.value || o.text.trim()).slice(0, 30).join(', '));
		el.dispatchEvent(new Event('input', {bubbles: true}));
		el.dispatchEvent(new Event('change', {bubbles: true}));
		return el.multiple ? Array.from(el.selectedOptions).map(o => o.value).join(',') : el.value;
	}`, values)
	if err != nil {
		return "", err
	}
	return string(res), nil
}

// Check sets a checkbox/radio to the desired state via a real click when a
// change is needed.
func (m *Manager) Check(ctx context.Context, t *Tab, uid string, want bool) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	ref, err := t.resolveUID(uid)
	if err != nil {
		return err
	}
	res, err := m.callOnNode(ctx, t, ref, `function() {
		if (typeof this.checked !== 'boolean') throw new Error('element is not checkable');
		return this.checked;
	}`)
	if err != nil {
		return err
	}
	if (string(res) == "true") == want {
		return nil // already in the desired state
	}
	return m.Click(ctx, t, uid, false)
}

// UploadFile attaches local files to a file input. The paths must exist —
// checked here so the error is actionable before any CDP round trip.
func (m *Manager) UploadFile(ctx context.Context, t *Tab, uid string, paths []string) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	ref, err := t.resolveUID(uid)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("file not found: %s", p)
		}
	}
	_, err = m.cdp(ctx, t, ref.session, "DOM.setFileInputFiles", map[string]any{
		"files": paths, "backendNodeId": ref.backendID,
	})
	return err
}

// Eval evaluates a JS expression in the page (escape hatch, eval-gated).
func (m *Manager) Eval(ctx context.Context, t *Tab, expr string) (string, error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return "", err
	}
	raw, err := m.CDP(ctx, t, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
		"userGesture":   true,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Result struct {
			Type        string          `json:"type"`
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		ExceptionDetails *struct {
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if resp.ExceptionDetails != nil {
		desc := resp.ExceptionDetails.Exception.Description
		if desc == "" {
			desc = resp.ExceptionDetails.Text
		}
		return "", fmt.Errorf("page exception: %s", truncate(desc, 1000))
	}
	if resp.Result.Value != nil {
		return truncate(string(resp.Result.Value), 20_000), nil
	}
	return resp.Result.Description, nil
}
