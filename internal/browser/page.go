package browser

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// extractJS is the readability-lite extraction script (see extract.js): it
// locates the page's main content and returns it as Markdown, or null when
// the page does not look like an article.
//
//go:embed extract.js
var extractJS string

// TabInfo is the cross-profile tab listing entry.
type TabInfo struct {
	Profile  string `json:"profile"`
	TabID    int    `json:"tabId"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Active   bool   `json:"active"`
	Attached bool   `json:"attached"`
}

// ListTabs lists tabs across the given profiles, each tagged with its label.
func (m *Manager) ListTabs(ctx context.Context, profiles []string) ([]TabInfo, error) {
	var out []TabInfo
	for _, p := range profiles {
		raw, err := m.b.Tabs(ctx, p, "list", nil)
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", p, err)
		}
		var tabs []TabInfo
		if err := json.Unmarshal(raw, &tabs); err != nil {
			return nil, err
		}
		for i := range tabs {
			tabs[i].Profile = p
		}
		out = append(out, tabs...)
	}
	return out, nil
}

// TabURL fetches a tab's current URL (the domain source for permission gating).
func (m *Manager) TabURL(ctx context.Context, profile string, tabID int) (url, title string, err error) {
	raw, err := m.b.Tabs(ctx, profile, "get", map[string]any{"tabId": tabID})
	if err != nil {
		return "", "", err
	}
	var info struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", "", err
	}
	return info.URL, info.Title, nil
}

// NewTab opens a tab (about:blank when url is empty).
func (m *Manager) NewTab(ctx context.Context, profile, url string) (int, error) {
	params := map[string]any{}
	if url != "" {
		params["url"] = url
	}
	raw, err := m.b.Tabs(ctx, profile, "create", params)
	if err != nil {
		return 0, err
	}
	var resp struct {
		TabID int `json:"tabId"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, err
	}
	return resp.TabID, nil
}

func (m *Manager) CloseTab(ctx context.Context, profile string, tabID int) error {
	_, err := m.b.Tabs(ctx, profile, "close", map[string]any{"tabId": tabID})
	m.dropTab(profile, tabID)
	return err
}

func (m *Manager) SelectTab(ctx context.Context, profile string, tabID int) error {
	_, err := m.b.Tabs(ctx, profile, "activate", map[string]any{"tabId": tabID})
	return err
}

// Navigate drives the tab to url and waits (best effort, up to timeout) for
// the load event.
func (m *Manager) Navigate(ctx context.Context, t *Tab, url string, timeout time.Duration) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	t.mu.Lock()
	t.loadFired = false
	t.uids = map[string]uidRef{} // old uids are meaningless after navigation
	t.mu.Unlock()
	raw, err := m.CDP(ctx, t, "Page.navigate", map[string]any{"url": url})
	if err != nil {
		return err
	}
	var resp struct {
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(raw, &resp); err == nil && resp.ErrorText != "" {
		return fmt.Errorf("navigation failed: %s", resp.ErrorText)
	}
	return m.WaitLoad(ctx, t, timeout)
}

// WaitLoad waits for the page load event (or readyState complete, whichever
// is observed first). Best effort: a timeout is reported, not fatal.
func (m *Manager) WaitLoad(ctx context.Context, t *Tab, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t.mu.Lock()
		fired := t.loadFired
		t.mu.Unlock()
		if fired {
			return nil
		}
		// The event can predate Page.enable; poll readyState as backstop.
		raw, err := m.CDP(ctx, t, "Runtime.evaluate", map[string]any{
			"expression": "document.readyState", "returnByValue": true,
		})
		if err == nil {
			var resp struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal(raw, &resp) == nil && resp.Result.Value == "complete" {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("page did not finish loading within %s (it may still be usable — try snapshot)", timeout)
}

// History moves back (delta=-1) or forward (delta=+1).
func (m *Manager) History(ctx context.Context, t *Tab, delta int) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	raw, err := m.CDP(ctx, t, "Page.getNavigationHistory", nil)
	if err != nil {
		return err
	}
	var hist struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &hist); err != nil {
		return err
	}
	idx := hist.CurrentIndex + delta
	if idx < 0 || idx >= len(hist.Entries) {
		return fmt.Errorf("no history entry in that direction (at %d of %d)", hist.CurrentIndex+1, len(hist.Entries))
	}
	t.mu.Lock()
	t.loadFired = false
	t.uids = map[string]uidRef{} // history navigation invalidates uids too
	t.mu.Unlock()
	if _, err := m.CDP(ctx, t, "Page.navigateToHistoryEntry", map[string]any{"entryId": hist.Entries[idx].ID}); err != nil {
		return err
	}
	// navigateToHistoryEntry returns before the navigation lands; without
	// this wait callers read the OLD tab URL. The grace sleep keeps the
	// readyState backstop from sampling the old page, which still reports
	// "complete" until the navigation commits.
	time.Sleep(150 * time.Millisecond)
	return m.WaitLoad(ctx, t, 15*time.Second)
}

func (m *Manager) Reload(ctx context.Context, t *Tab, timeout time.Duration) error {
	if err := m.EnsurePage(ctx, t); err != nil {
		return err
	}
	t.mu.Lock()
	t.loadFired = false
	t.uids = map[string]uidRef{}
	t.mu.Unlock()
	if _, err := m.CDP(ctx, t, "Page.reload", nil); err != nil {
		return err
	}
	return m.WaitLoad(ctx, t, timeout)
}

// WaitFor supports conditions: "load", "network_idle", "selector", "text".
func (m *Manager) WaitFor(ctx context.Context, t *Tab, condition, value string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	switch condition {
	case "load":
		return m.WaitLoad(ctx, t, timeout)
	case "network_idle":
		return m.waitNetworkIdle(ctx, t, timeout)
	case "selector", "text":
		if value == "" {
			return fmt.Errorf("condition %q needs a value", condition)
		}
		var expr string
		if condition == "selector" {
			expr = fmt.Sprintf("!!document.querySelector(%q)", value)
		} else {
			expr = fmt.Sprintf("document.body && document.body.innerText.includes(%q)", value)
		}
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			raw, err := m.CDP(ctx, t, "Runtime.evaluate", map[string]any{
				"expression": expr, "returnByValue": true,
			})
			if err == nil {
				var resp struct {
					Result struct {
						Value bool `json:"value"`
					} `json:"result"`
				}
				if json.Unmarshal(raw, &resp) == nil && resp.Result.Value {
					return nil
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("%s %q did not appear within %s", condition, value, timeout)
	default:
		return fmt.Errorf("unknown condition %q (load, network_idle, selector, text)", condition)
	}
}

// waitNetworkIdle waits until no request has been in flight for 500ms.
func (m *Manager) waitNetworkIdle(ctx context.Context, t *Tab, timeout time.Duration) error {
	if err := m.ensureDomain(ctx, t, "", "Network"); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	quietSince := time.Time{}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if t.inflightCount() == 0 {
			if quietSince.IsZero() {
				quietSince = time.Now()
			} else if time.Since(quietSince) >= 500*time.Millisecond {
				return nil
			}
		} else {
			quietSince = time.Time{}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("network did not go idle within %s (%d requests in flight)", timeout, t.inflightCount())
}

// ReadPage extracts the page's readable text.
func (m *Manager) ReadPage(ctx context.Context, t *Tab) (string, error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return "", err
	}
	out, err := m.Eval(ctx, t, "document.body ? document.body.innerText : ''")
	if err != nil {
		return "", err
	}
	var text string
	if json.Unmarshal([]byte(out), &text) != nil {
		text = out
	}
	return truncate(strings.TrimSpace(text), 30_000), nil
}

// Article is the result of readability extraction.
type Article struct {
	Title    string `json:"title"`
	Byline   string `json:"byline"`
	Markdown string `json:"markdown"`
}

// ReadArticle extracts the page's main content as Markdown. Returns
// (nil, nil) when no article-like content is detected — callers fall back
// to ReadPage.
func (m *Manager) ReadArticle(ctx context.Context, t *Tab) (*Article, error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return nil, err
	}
	raw, err := m.CDP(ctx, t, "Runtime.evaluate", map[string]any{
		"expression":    extractJS,
		"returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Value *Article `json:"value"`
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
		return nil, fmt.Errorf("extraction script failed: %s", truncate(desc, 500))
	}
	return resp.Result.Value, nil
}

// GetDOM returns the outerHTML of an element by uid or CSS selector.
func (m *Manager) GetDOM(ctx context.Context, t *Tab, uid, selector string) (string, error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return "", err
	}
	params := map[string]any{}
	sess := ""
	switch {
	case uid != "":
		ref, err := t.resolveUID(uid)
		if err != nil {
			return "", err
		}
		params["backendNodeId"] = ref.backendID
		sess = ref.session
	case selector != "":
		root, err := m.CDP(ctx, t, "DOM.getDocument", map[string]any{"depth": 0})
		if err != nil {
			return "", err
		}
		var doc struct {
			Root struct {
				NodeID int `json:"nodeId"`
			} `json:"root"`
		}
		if err := json.Unmarshal(root, &doc); err != nil {
			return "", err
		}
		raw, err := m.CDP(ctx, t, "DOM.querySelector", map[string]any{
			"nodeId": doc.Root.NodeID, "selector": selector,
		})
		if err != nil {
			return "", err
		}
		var found struct {
			NodeID int `json:"nodeId"`
		}
		if err := json.Unmarshal(raw, &found); err != nil {
			return "", err
		}
		if found.NodeID == 0 {
			return "", fmt.Errorf("no element matches selector %q", selector)
		}
		params["nodeId"] = found.NodeID
	default:
		return "", fmt.Errorf("pass uid or selector")
	}
	raw, err := m.cdp(ctx, t, sess, "DOM.getOuterHTML", params)
	if err != nil {
		return "", err
	}
	var resp struct {
		OuterHTML string `json:"outerHTML"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	return truncate(resp.OuterHTML, 30_000), nil
}

// Query returns the outerHTML of up to 20 elements matching a CSS selector.
func (m *Manager) Query(ctx context.Context, t *Tab, selector string) (string, error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return "", err
	}
	expr := fmt.Sprintf(`JSON.stringify(Array.from(document.querySelectorAll(%q)).slice(0, 20).map(e => e.outerHTML.slice(0, 1500)))`, selector)
	return m.Eval(ctx, t, expr)
}

// Screenshot captures the viewport (default), the full page, or one element.
// JPEG by default — full-page PNGs are token-expensive (PLAN §4).
func (m *Manager) Screenshot(ctx context.Context, t *Tab, fullPage bool, uid, format string, quality int) (data []byte, mime string, err error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return nil, "", err
	}
	if format == "" {
		format = "jpeg"
	}
	if format != "jpeg" && format != "png" {
		return nil, "", fmt.Errorf("format must be jpeg or png")
	}
	if quality <= 0 || quality > 100 {
		quality = 70
	}
	params := map[string]any{"format": format}
	if format == "jpeg" {
		params["quality"] = quality
	}
	if fullPage {
		params["captureBeyondViewport"] = true
	}
	if uid != "" {
		ref, err := t.resolveUID(uid)
		if err != nil {
			return nil, "", err
		}
		// center is in main-viewport coordinates (frame offsets applied),
		// which is the space Page.captureScreenshot clips in.
		x, y, err := m.center(ctx, t, ref)
		if err != nil {
			return nil, "", err
		}
		raw, err := m.cdp(ctx, t, ref.session, "DOM.getBoxModel", map[string]any{"backendNodeId": ref.backendID})
		if err != nil {
			return nil, "", err
		}
		var box struct {
			Model struct {
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"model"`
		}
		if err := json.Unmarshal(raw, &box); err != nil {
			return nil, "", err
		}
		params["clip"] = map[string]any{
			"x": x - box.Model.Width/2, "y": y - box.Model.Height/2,
			"width": box.Model.Width, "height": box.Model.Height, "scale": 1,
		}
	}
	raw, err := m.CDP(ctx, t, "Page.captureScreenshot", params)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", err
	}
	bin, err := base64.StdEncoding.DecodeString(resp.Data)
	if err != nil {
		return nil, "", err
	}
	return bin, "image/" + format, nil
}

// EnableConsole/EnableNetwork start buffering; entries before enabling are
// not captured (documented behavior).
func (m *Manager) EnableConsole(ctx context.Context, t *Tab) error {
	return m.ensureDomain(ctx, t, "", "Runtime")
}

func (m *Manager) EnableNetwork(ctx context.Context, t *Tab) error {
	return m.ensureDomain(ctx, t, "", "Network")
}
