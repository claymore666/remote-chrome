package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"remote-chrome/internal/approval"
	"remote-chrome/internal/audit"
	"remote-chrome/internal/perms"
)

// text wraps a string as a tool result.
func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func textf(format string, args ...any) *mcp.CallToolResult {
	return text(fmt.Sprintf(format, args...))
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return text(string(data)), nil
}

// validateNavURL restricts navigation to web-ish schemes; chrome.debugger
// cannot attach to chrome:// or the Web Store anyway, and file:// stays off
// by default.
func validateNavURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https", "about":
		return u, nil
	default:
		return nil, fmt.Errorf("navigation to %q refused: only http, https and about: URLs are allowed", raw)
	}
}

// Common argument fragments.
type tabArgs struct {
	Profile string `json:"profile,omitempty" jsonschema:"browser profile label; omit for the default/only connected profile"`
	TabID   int    `json:"tab_id,omitempty" jsonschema:"target tab id from list_tabs; omit for the profile's active tab"`
}

func (s *Server) registerTools() {
	s.registerNavigation()
	s.registerReading()
	s.registerInteraction()
	s.registerPermissions()
	s.registerControl()
}

// ---------------------------------------------------------------- navigation

func (s *Server) registerNavigation() {
	type navigateArgs struct {
		tabArgs
		URL string `json:"url" jsonschema:"absolute URL to open (http/https)"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "navigate",
		Description: "Navigate a tab to a URL and wait for it to load. Requires a navigate grant for the destination domain.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a navigateArgs) (*mcp.CallToolResult, any, error) {
		u, err := validateNavURL(a.URL)
		if err != nil {
			return nil, nil, err
		}
		domain, err := perms.DomainOf(u.String())
		if err != nil {
			return nil, nil, err
		}
		if s.navigationDenied(domain) {
			return nil, nil, fmt.Errorf("navigation to %s is blocked by the deny_navigation config list", domain)
		}
		profile, err := s.resolveProfile(ctx, req.Session, a.Profile)
		if err != nil {
			return nil, nil, err
		}
		if err := s.ensureGrant(ctx, req.Session, []perms.Action{perms.Navigate}, domain, "navigate to "+a.URL); err != nil {
			return nil, nil, err
		}
		tabID, err := s.targetTab(ctx, profile, a.TabID)
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "navigate", Profile: profile, Domain: domain, Detail: map[string]any{"url": a.URL, "tab": tabID}})
		t := s.mgr.Tab(profile, tabID)
		loadErr := s.mgr.Navigate(ctx, t, u.String(), 20*time.Second)
		curURL, title, _ := s.mgr.TabURL(ctx, profile, tabID)
		msg := fmt.Sprintf("tab %d is at %s — %q", tabID, curURL, title)
		if loadErr != nil {
			msg += "\nnote: " + loadErr.Error()
		}
		return text(msg), nil, nil
	})

	type historyArgs struct{ tabArgs }
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "back", Description: "Go back one entry in the tab's history."},
		func(ctx context.Context, req *mcp.CallToolRequest, a historyArgs) (*mcp.CallToolResult, any, error) {
			return s.historyTool(ctx, req, a.tabArgs, -1)
		})
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "forward", Description: "Go forward one entry in the tab's history."},
		func(ctx context.Context, req *mcp.CallToolRequest, a historyArgs) (*mcp.CallToolResult, any, error) {
			return s.historyTool(ctx, req, a.tabArgs, +1)
		})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "reload", Description: "Reload the tab and wait for it to load."},
		func(ctx context.Context, req *mcp.CallToolRequest, a historyArgs) (*mcp.CallToolResult, any, error) {
			profile, tabID, err := s.gateTab(ctx, req, perms.Navigate, a.Profile, a.TabID, "reload the page")
			if err != nil {
				return nil, nil, err
			}
			s.audit(audit.Entry{Kind: "tool", Tool: "reload", Profile: profile, Detail: map[string]any{"tab": tabID}})
			if err := s.mgr.Reload(ctx, s.mgr.Tab(profile, tabID), 20*time.Second); err != nil {
				return nil, nil, err
			}
			return text("reloaded"), nil, nil
		})

	type waitArgs struct {
		tabArgs
		Condition string `json:"condition" jsonschema:"one of: load, network_idle, selector, text"`
		Value     string `json:"value,omitempty" jsonschema:"CSS selector or text to wait for (for selector/text conditions)"`
		TimeoutS  int    `json:"timeout_s,omitempty" jsonschema:"max seconds to wait (default 15)"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "wait_for",
		Description: "Wait until the page settles: load event, network idle, a CSS selector appears, or text appears.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a waitArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Navigate, a.Profile, a.TabID, "wait for "+a.Condition)
		if err != nil {
			return nil, nil, err
		}
		if err := s.mgr.WaitFor(ctx, s.mgr.Tab(profile, tabID), a.Condition, a.Value, time.Duration(a.TimeoutS)*time.Second); err != nil {
			return nil, nil, err
		}
		return text("condition met"), nil, nil
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "list_tabs",
		Description: "List open tabs across all connected profiles (id, url, title, active, profile label).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		tabs, err := s.mgr.ListTabs(ctx, s.bridge.Profiles())
		if err != nil {
			return nil, nil, err
		}
		res, err := jsonResult(tabs)
		return res, nil, err
	})

	type newTabArgs struct {
		Profile string `json:"profile,omitempty" jsonschema:"browser profile label; omit for the default/only connected profile"`
		URL     string `json:"url,omitempty" jsonschema:"URL to open; omit for a blank tab"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "new_tab", Description: "Open a new tab, optionally at a URL."},
		func(ctx context.Context, req *mcp.CallToolRequest, a newTabArgs) (*mcp.CallToolResult, any, error) {
			profile, err := s.resolveProfile(ctx, req.Session, a.Profile)
			if err != nil {
				return nil, nil, err
			}
			if a.URL != "" {
				u, err := validateNavURL(a.URL)
				if err != nil {
					return nil, nil, err
				}
				domain, err := perms.DomainOf(u.String())
				if err != nil {
					return nil, nil, err
				}
				if s.navigationDenied(domain) {
					return nil, nil, fmt.Errorf("navigation to %s is blocked by the deny_navigation config list", domain)
				}
				if err := s.ensureGrant(ctx, req.Session, []perms.Action{perms.Navigate}, domain, "open "+a.URL); err != nil {
					return nil, nil, err
				}
			}
			tabID, err := s.mgr.NewTab(ctx, profile, a.URL)
			if err != nil {
				return nil, nil, err
			}
			s.audit(audit.Entry{Kind: "tool", Tool: "new_tab", Profile: profile, Detail: map[string]any{"url": a.URL, "tab": tabID}})
			return textf("opened tab %d", tabID), nil, nil
		})

	type tabIDArgs struct {
		Profile string `json:"profile,omitempty" jsonschema:"browser profile label; omit for the default/only connected profile"`
		TabID   int    `json:"tab_id" jsonschema:"tab id from list_tabs"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "close_tab", Description: "Close a tab."},
		func(ctx context.Context, req *mcp.CallToolRequest, a tabIDArgs) (*mcp.CallToolResult, any, error) {
			profile, tabID, err := s.gateTab(ctx, req, perms.Navigate, a.Profile, a.TabID, "close the tab")
			if err != nil {
				return nil, nil, err
			}
			s.audit(audit.Entry{Kind: "tool", Tool: "close_tab", Profile: profile, Detail: map[string]any{"tab": tabID}})
			if err := s.mgr.CloseTab(ctx, profile, tabID); err != nil {
				return nil, nil, err
			}
			return text("closed"), nil, nil
		})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "select_tab", Description: "Bring a tab to the foreground."},
		func(ctx context.Context, req *mcp.CallToolRequest, a tabIDArgs) (*mcp.CallToolResult, any, error) {
			profile, tabID, err := s.gateTab(ctx, req, perms.Navigate, a.Profile, a.TabID, "switch to the tab")
			if err != nil {
				return nil, nil, err
			}
			if err := s.mgr.SelectTab(ctx, profile, tabID); err != nil {
				return nil, nil, err
			}
			return text("selected"), nil, nil
		})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "list_profiles",
		Description: "List connected Chrome profiles (extension instances) by label.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		res, err := jsonResult(map[string]any{
			"connected": s.bridge.Profiles(),
			"default":   s.cfg.DefaultProfile,
		})
		return res, nil, err
	})
}

func (s *Server) historyTool(ctx context.Context, req *mcp.CallToolRequest, a tabArgs, delta int) (*mcp.CallToolResult, any, error) {
	name := "back"
	if delta > 0 {
		name = "forward"
	}
	profile, tabID, err := s.gateTab(ctx, req, perms.Navigate, a.Profile, a.TabID, "go "+name)
	if err != nil {
		return nil, nil, err
	}
	s.audit(audit.Entry{Kind: "tool", Tool: name, Profile: profile, Detail: map[string]any{"tab": tabID}})
	if err := s.mgr.History(ctx, s.mgr.Tab(profile, tabID), delta); err != nil {
		return nil, nil, err
	}
	curURL, title, _ := s.mgr.TabURL(ctx, profile, tabID)
	return textf("now at %s — %q", curURL, title), nil, nil
}

// ------------------------------------------------------------------- reading

func (s *Server) registerReading() {
	type readTabArgs struct{ tabArgs }

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "snapshot",
		Description: "PRIMARY perception tool: capture the page's accessibility tree with stable uids for every interactive element. Pass those uids to click/type/etc. Take a fresh snapshot after navigation or significant page changes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a readTabArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Read, a.Profile, a.TabID, "read the page structure")
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "snapshot", Profile: profile, Detail: map[string]any{"tab": tabID}})
		out, err := s.mgr.Snapshot(ctx, s.mgr.Tab(profile, tabID))
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	type readPageArgs struct {
		tabArgs
		Mode           string `json:"mode,omitempty" jsonschema:"text (default): full rendered text; article: extract the main content as Markdown with headings/links/lists/tables preserved and nav/banner noise dropped (falls back to text when the page is not article-like)"`
		WithScreenshot bool   `json:"with_screenshot,omitempty" jsonschema:"also return a viewport screenshot (JPEG) so you see the page as the user does"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "read_page",
		Description: "Read the page's content. mode=article gives clean Markdown of the main content (best for articles/posts); mode=text gives the full rendered text. with_screenshot adds the visual rendering in the same call.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a readPageArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Read, a.Profile, a.TabID, "read the page")
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "read_page", Profile: profile,
			Detail: map[string]any{"tab": tabID, "mode": a.Mode, "screenshot": a.WithScreenshot}})
		t := s.mgr.Tab(profile, tabID)
		curURL, title, _ := s.mgr.TabURL(ctx, profile, tabID)

		var body string
		switch a.Mode {
		case "", "text":
			out, err := s.mgr.ReadPage(ctx, t)
			if err != nil {
				return nil, nil, err
			}
			body = fmt.Sprintf("%s — %q\n\n%s", curURL, title, out)
		case "article":
			art, err := s.mgr.ReadArticle(ctx, t)
			if err != nil {
				return nil, nil, err
			}
			if art == nil {
				out, err := s.mgr.ReadPage(ctx, t)
				if err != nil {
					return nil, nil, err
				}
				body = fmt.Sprintf("%s — %q\n[no article-like main content detected — full page text instead]\n\n%s", curURL, title, out)
			} else {
				header := fmt.Sprintf("%s — %q (article extraction)", curURL, art.Title)
				if art.Byline != "" {
					header += "\nby " + art.Byline
				}
				body = header + "\n\n" + art.Markdown
			}
		default:
			return nil, nil, fmt.Errorf("unknown mode %q (text, article)", a.Mode)
		}

		contents := []mcp.Content{&mcp.TextContent{Text: body}}
		if a.WithScreenshot {
			data, mime, err := s.mgr.Screenshot(ctx, t, false, "", "jpeg", 0)
			if err != nil {
				contents = append(contents, &mcp.TextContent{Text: "[screenshot failed: " + err.Error() + "]"})
			} else {
				contents = append(contents, &mcp.ImageContent{Data: data, MIMEType: mime})
			}
		}
		return &mcp.CallToolResult{Content: contents}, nil, nil
	})

	type getDOMArgs struct {
		tabArgs
		UID      string `json:"uid,omitempty" jsonschema:"element uid from snapshot"`
		Selector string `json:"selector,omitempty" jsonschema:"CSS selector (used when uid is omitted)"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "get_dom",
		Description: "Get the outerHTML of one element, by snapshot uid or CSS selector.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a getDOMArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Read, a.Profile, a.TabID, "read page HTML")
		if err != nil {
			return nil, nil, err
		}
		out, err := s.mgr.GetDOM(ctx, s.mgr.Tab(profile, tabID), a.UID, a.Selector)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	type queryArgs struct {
		tabArgs
		Selector string `json:"selector" jsonschema:"CSS selector"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "query",
		Description: "Return outerHTML of up to 20 elements matching a CSS selector (fallback when snapshot is not enough).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a queryArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Read, a.Profile, a.TabID, "query page elements")
		if err != nil {
			return nil, nil, err
		}
		out, err := s.mgr.Query(ctx, s.mgr.Tab(profile, tabID), a.Selector)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	type screenshotArgs struct {
		tabArgs
		FullPage bool   `json:"full_page,omitempty" jsonschema:"capture the whole scrollable page, not just the viewport"`
		UID      string `json:"uid,omitempty" jsonschema:"capture just this element (uid from snapshot)"`
		Format   string `json:"format,omitempty" jsonschema:"jpeg (default, cheaper) or png"`
		Quality  int    `json:"quality,omitempty" jsonschema:"jpeg quality 1-100 (default 70)"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "screenshot",
		Description: "Screenshot the viewport (default), full page, or one element. JPEG by default to keep tokens down.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a screenshotArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Read, a.Profile, a.TabID, "screenshot the page")
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "screenshot", Profile: profile, Detail: map[string]any{"tab": tabID, "full": a.FullPage}})
		data, mime, err := s.mgr.Screenshot(ctx, s.mgr.Tab(profile, tabID), a.FullPage, a.UID, a.Format, a.Quality)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.ImageContent{Data: data, MIMEType: mime},
		}}, nil, nil
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "read_console",
		Description: "Read buffered console messages for a tab. Buffering starts the first time this is called for the tab.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a readTabArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Read, a.Profile, a.TabID, "read the console")
		if err != nil {
			return nil, nil, err
		}
		t := s.mgr.Tab(profile, tabID)
		if err := s.mgr.EnableConsole(ctx, t); err != nil {
			return nil, nil, err
		}
		entries := t.Console()
		if len(entries) == 0 {
			return text("no console messages buffered (capture starts when this tool is first used on a tab — interact with the page and read again)"), nil, nil
		}
		res, err := jsonResult(entries)
		return res, nil, err
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "read_network",
		Description: "Read the buffered network request log for a tab. Buffering starts the first time this is called for the tab.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a readTabArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Read, a.Profile, a.TabID, "read network requests")
		if err != nil {
			return nil, nil, err
		}
		t := s.mgr.Tab(profile, tabID)
		if err := s.mgr.EnableNetwork(ctx, t); err != nil {
			return nil, nil, err
		}
		entries := t.Network()
		if len(entries) == 0 {
			return text("no requests buffered yet (capture starts when this tool is first used on a tab — reload or interact and read again)"), nil, nil
		}
		res, err := jsonResult(entries)
		return res, nil, err
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "get_url",
		Description: "Get a tab's current URL and title (metadata only, ungated like list_tabs).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a readTabArgs) (*mcp.CallToolResult, any, error) {
		profile, err := s.resolveProfile(ctx, req.Session, a.Profile)
		if err != nil {
			return nil, nil, err
		}
		tabID, err := s.targetTab(ctx, profile, a.TabID)
		if err != nil {
			return nil, nil, err
		}
		curURL, title, err := s.mgr.TabURL(ctx, profile, tabID)
		if err != nil {
			return nil, nil, err
		}
		res, err := jsonResult(map[string]any{"url": curURL, "title": title, "tabId": tabID, "profile": profile})
		return res, nil, err
	})
}

// --------------------------------------------------------------- interaction

func (s *Server) registerInteraction() {
	type clickArgs struct {
		tabArgs
		UID    string `json:"uid" jsonschema:"element uid from snapshot"`
		Double bool   `json:"double,omitempty" jsonschema:"double-click instead of single click"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "click",
		Description: "Click an element (real mouse events). Get uids from snapshot.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a clickArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Interact, a.Profile, a.TabID, "click an element")
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "click", Profile: profile, Detail: map[string]any{"tab": tabID, "uid": a.UID}})
		if err := s.mgr.Click(ctx, s.mgr.Tab(profile, tabID), a.UID, a.Double); err != nil {
			return nil, nil, err
		}
		return text("clicked — page may have changed; snapshot again before further interaction"), nil, nil
	})

	type typeArgs struct {
		tabArgs
		UID        string `json:"uid" jsonschema:"element uid from snapshot (an input, textarea or contenteditable)"`
		Text       string `json:"text" jsonschema:"text to type"`
		Clear      bool   `json:"clear,omitempty" jsonschema:"clear the field first"`
		PressEnter bool   `json:"press_enter,omitempty" jsonschema:"press Enter after typing"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "type",
		Description: "Focus an element and type text into it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a typeArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Interact, a.Profile, a.TabID, "type into a field")
		if err != nil {
			return nil, nil, err
		}
		// Audit length, not content: the text may be sensitive (it is still
		// the user's own machine, but the log should not hoard secrets).
		s.audit(audit.Entry{Kind: "tool", Tool: "type", Profile: profile, Detail: map[string]any{"tab": tabID, "uid": a.UID, "chars": len(a.Text)}})
		if err := s.mgr.Type(ctx, s.mgr.Tab(profile, tabID), a.UID, a.Text, a.Clear, a.PressEnter); err != nil {
			return nil, nil, err
		}
		return text("typed"), nil, nil
	})

	type selectArgs struct {
		tabArgs
		UID    string   `json:"uid" jsonschema:"uid of the <select> element"`
		Values []string `json:"values" jsonschema:"option values or visible labels to select"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "select_option",
		Description: "Select option(s) in a <select> dropdown by value or label.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a selectArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Interact, a.Profile, a.TabID, "select a dropdown option")
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "select_option", Profile: profile, Detail: map[string]any{"tab": tabID, "uid": a.UID, "values": a.Values}})
		selected, err := s.mgr.SelectOption(ctx, s.mgr.Tab(profile, tabID), a.UID, a.Values)
		if err != nil {
			return nil, nil, err
		}
		return textf("selected: %s", selected), nil, nil
	})

	type checkArgs struct {
		tabArgs
		UID     string `json:"uid" jsonschema:"uid of the checkbox/radio"`
		Checked *bool  `json:"checked,omitempty" jsonschema:"desired state (default true)"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "check",
		Description: "Set a checkbox or radio button to a desired state.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a checkArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Interact, a.Profile, a.TabID, "toggle a checkbox")
		if err != nil {
			return nil, nil, err
		}
		want := true
		if a.Checked != nil {
			want = *a.Checked
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "check", Profile: profile, Detail: map[string]any{"tab": tabID, "uid": a.UID, "checked": want}})
		if err := s.mgr.Check(ctx, s.mgr.Tab(profile, tabID), a.UID, want); err != nil {
			return nil, nil, err
		}
		return text("done"), nil, nil
	})

	type hoverArgs struct {
		tabArgs
		UID string `json:"uid" jsonschema:"element uid from snapshot"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "hover",
		Description: "Move the mouse over an element (opens hover menus, tooltips).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a hoverArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Interact, a.Profile, a.TabID, "hover over an element")
		if err != nil {
			return nil, nil, err
		}
		if err := s.mgr.Hover(ctx, s.mgr.Tab(profile, tabID), a.UID); err != nil {
			return nil, nil, err
		}
		return text("hovering"), nil, nil
	})

	type scrollArgs struct {
		tabArgs
		UID       string `json:"uid,omitempty" jsonschema:"scroll this element into view; omit to scroll the page"`
		Direction string `json:"direction,omitempty" jsonschema:"up, down (default), left or right"`
		Amount    int    `json:"amount,omitempty" jsonschema:"pixels to scroll (default 600)"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "scroll",
		Description: "Scroll the page, or scroll an element into view.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a scrollArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Interact, a.Profile, a.TabID, "scroll the page")
		if err != nil {
			return nil, nil, err
		}
		if err := s.mgr.Scroll(ctx, s.mgr.Tab(profile, tabID), a.UID, a.Direction, a.Amount); err != nil {
			return nil, nil, err
		}
		return text("scrolled"), nil, nil
	})

	type pressKeyArgs struct {
		tabArgs
		Key       string   `json:"key" jsonschema:"key name (Enter, Tab, Escape, ArrowDown, …) or a single character"`
		Modifiers []string `json:"modifiers,omitempty" jsonschema:"held modifiers: Control, Shift, Alt, Meta"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "press_key",
		Description: "Press a key (with optional modifiers) on the focused element.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a pressKeyArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Interact, a.Profile, a.TabID, "press "+a.Key)
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "press_key", Profile: profile, Detail: map[string]any{"tab": tabID, "key": a.Key, "mods": a.Modifiers}})
		if err := s.mgr.PressKey(ctx, s.mgr.Tab(profile, tabID), a.Key, a.Modifiers); err != nil {
			return nil, nil, err
		}
		return text("pressed"), nil, nil
	})

	type uploadArgs struct {
		tabArgs
		UID   string   `json:"uid" jsonschema:"uid of the file input element"`
		Paths []string `json:"paths" jsonschema:"absolute paths of local files to attach"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "upload_file",
		Description: "Attach local file(s) to a file input. Gated separately (upload grant, default once-per-use).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a uploadArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Upload, a.Profile, a.TabID, fmt.Sprintf("upload %s", strings.Join(a.Paths, ", ")))
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "upload_file", Profile: profile, Detail: map[string]any{"tab": tabID, "paths": a.Paths}})
		if err := s.mgr.UploadFile(ctx, s.mgr.Tab(profile, tabID), a.UID, a.Paths); err != nil {
			return nil, nil, err
		}
		return text("files attached"), nil, nil
	})

	type evalArgs struct {
		tabArgs
		Expression string `json:"expression" jsonschema:"JavaScript expression to evaluate in the page"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "eval_js",
		Description: "Evaluate JavaScript in the page (escape hatch). Gated separately (eval grant, default once-per-use).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a evalArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Eval, a.Profile, a.TabID, "evaluate JavaScript")
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "tool", Tool: "eval_js", Profile: profile, Detail: map[string]any{"tab": tabID, "expr": truncateStr(a.Expression, 300)}})
		out, err := s.mgr.Eval(ctx, s.mgr.Tab(profile, tabID), a.Expression)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	type dialogArgs struct {
		tabArgs
		Accept bool   `json:"accept" jsonschema:"true to accept/confirm the next dialog, false to dismiss"`
		Text   string `json:"text,omitempty" jsonschema:"text to enter when the next dialog is a prompt"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "handle_dialog",
		Description: "Arm the response for the NEXT JavaScript dialog (alert/confirm/prompt/beforeunload) on the tab, and report the last auto-handled one. Unarmed dialogs are auto-dismissed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a dialogArgs) (*mcp.CallToolResult, any, error) {
		profile, tabID, err := s.gateTab(ctx, req, perms.Interact, a.Profile, a.TabID, "handle a page dialog")
		if err != nil {
			return nil, nil, err
		}
		t := s.mgr.Tab(profile, tabID)
		if err := s.mgr.EnsurePage(ctx, t); err != nil {
			return nil, nil, err
		}
		t.SetDialogPolicy(a.Accept, a.Text)
		msg := fmt.Sprintf("armed: next dialog will be %s", map[bool]string{true: "accepted", false: "dismissed"}[a.Accept])
		if last := t.LastDialog(); last != nil {
			msg += fmt.Sprintf("\nlast dialog: %s %q (accepted=%v)", last.Type, last.Message, last.Accepted)
		}
		return text(msg), nil, nil
	})
}

// --------------------------------------------------------------- permissions

func (s *Server) registerPermissions() {
	type requestPermArgs struct {
		Actions []string `json:"actions" jsonschema:"action groups to request: read, navigate, interact, upload, eval"`
		Domain  string   `json:"domain" jsonschema:"registrable domain (e.g. linkedin.com) or * for anywhere"`
		Reason  string   `json:"reason" jsonschema:"short human-readable reason shown in the approval dialog"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "request_permission",
		Description: "Proactively request grants for a task so the user sees ONE approval dialog instead of several (e.g. read+navigate+interact on linkedin.com before starting).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a requestPermArgs) (*mcp.CallToolResult, any, error) {
		if len(a.Actions) == 0 {
			return nil, nil, fmt.Errorf("actions must not be empty")
		}
		var actions []perms.Action
		for _, raw := range a.Actions {
			act, err := perms.ParseAction(raw)
			if err != nil {
				return nil, nil, err
			}
			actions = append(actions, act)
		}
		domain, err := normalizeDomain(a.Domain)
		if err != nil {
			return nil, nil, err
		}
		if err := s.ensureGrant(ctx, req.Session, actions, domain, a.Reason); err != nil {
			return nil, nil, err
		}
		return textf("granted: %s on %s", joinActions(actions), domain), nil, nil
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "list_permissions",
		Description: "Show the current session's permission matrix (free).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		res, err := jsonResult(map[string]any{
			"grants":     s.matrix.List(),
			"loaded_set": s.matrix.LoadedSet(),
			"saves_to":   s.cfg.PermissionSet,
		})
		return res, nil, err
	})

	type removePermArgs struct {
		Action string `json:"action" jsonschema:"action group of the grant to remove"`
		Domain string `json:"domain" jsonschema:"domain of the grant to remove"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "remove_permission",
		Description: "Remove a grant from the session matrix (free — shrinking access is always safe).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a removePermArgs) (*mcp.CallToolResult, any, error) {
		act, err := perms.ParseAction(a.Action)
		if err != nil {
			return nil, nil, err
		}
		if !s.matrix.Remove(act, a.Domain) {
			return textf("no such grant: %s on %s", act, a.Domain), nil, nil
		}
		s.audit(audit.Entry{Kind: "grant", Action: string(act), Domain: a.Domain, Decision: "removed"})
		return text("removed"), nil, nil
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "reset_permissions",
		Description: "Clear the entire session permission matrix (free).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		s.matrix.Reset()
		s.audit(audit.Entry{Kind: "grant", Decision: "reset"})
		return text("permission matrix cleared"), nil, nil
	})

	type setNameArgs struct {
		Name string `json:"name" jsonschema:"permission set name (letters, digits, dashes)"`
	}
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "save_permission_set",
		Description: "Save the current session matrix as a named per-project permission set.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a setNameArgs) (*mcp.CallToolResult, any, error) {
		n, err := s.matrix.SaveSet(s.dir, a.Name)
		if err != nil {
			return nil, nil, err
		}
		s.audit(audit.Entry{Kind: "grant", Decision: "saved_set:" + a.Name, Detail: map[string]any{"grants": n}})
		return textf("saved %d grants as set %q", n, a.Name), nil, nil
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "load_permission_set",
		Description: "Load a named permission set into the session matrix. The user approves the whole set in one dialog.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a setNameArgs) (*mcp.CallToolResult, any, error) {
		set, err := perms.ReadSet(s.dir, a.Name)
		if err != nil {
			return nil, nil, err
		}
		if len(set.Grants) == 0 {
			return textf("set %q is empty", a.Name), nil, nil
		}
		var lines []string
		for _, g := range set.Grants {
			lines = append(lines, fmt.Sprintf("%s × %s", g.Action, g.Domain))
		}
		message := fmt.Sprintf("Load permission set %q for this session?\n%s", a.Name, strings.Join(lines, "\n"))

		s.approvalMu.Lock()
		ok, err := s.confirmHuman(ctx, req.Session, message)
		s.approvalMu.Unlock()
		if err != nil {
			return nil, nil, fmt.Errorf("approval unavailable: %w", err)
		}
		s.audit(audit.Entry{Kind: "approval", Decision: fmt.Sprintf("load_set:%s:%v", a.Name, ok)})
		if !ok {
			return nil, nil, fmt.Errorf("user declined loading permission set %q", a.Name)
		}
		s.matrix.Apply(set)
		return textf("loaded set %q: %d grants active", a.Name, len(set.Grants)), nil, nil
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "list_permission_sets",
		Description: "List saved permission set names (free).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		names, err := perms.ListSets(s.dir)
		if err != nil {
			return nil, nil, err
		}
		res, err := jsonResult(names)
		return res, nil, err
	})
}

// confirmHuman is a yes/no approval (elicitation or native dialog).
func (s *Server) confirmHuman(ctx context.Context, session *mcp.ServerSession, message string) (bool, error) {
	mode := s.cfg.Approval
	canElicit := false
	if session != nil {
		if init := session.InitializeParams(); init != nil && init.Capabilities != nil && init.Capabilities.Elicitation != nil {
			canElicit = true
		}
	}
	if (mode == "elicit" || (mode != "dialog" && canElicit)) && session != nil {
		res, err := session.Elicit(ctx, &mcp.ElicitParams{
			Message: message,
			RequestedSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"approve": map[string]any{"type": "boolean", "description": "true to load the set"},
				},
				"required": []string{"approve"},
			},
		})
		if err != nil {
			return false, err
		}
		if res.Action != "accept" {
			return false, nil
		}
		ok, _ := res.Content["approve"].(bool)
		return ok, nil
	}
	return approval.Confirm(ctx, message)
}

// ------------------------------------------------------------------- control

func (s *Server) registerControl() {
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "kill_switch",
		Description: "Emergency stop: detach the debugger from every tab in every profile (the Chrome banner disappears) and clear all session grants.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		s.bridge.DetachAll(ctx)
		s.matrix.Reset()
		s.audit(audit.Entry{Kind: "kill"})
		return text("all debugger sessions detached; permission matrix cleared"), nil, nil
	})

	mcp.AddTool(s.MCP, &mcp.Tool{Name: "diagnostics",
		Description: "Server diagnostics: connected profiles, version, state dir (free).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		res, err := jsonResult(map[string]any{
			"version":   Version,
			"profiles":  s.bridge.Profiles(),
			"state_dir": s.dir,
			"grants":    len(s.matrix.List()),
		})
		return res, nil, err
	})
}

// normalizeDomain accepts "linkedin.com", "https://www.linkedin.com/x", or
// "*" and returns the registrable domain.
func normalizeDomain(d string) (string, error) {
	d = strings.TrimSpace(d)
	if d == "*" {
		return "*", nil
	}
	if !strings.Contains(d, "://") {
		d = "https://" + d
	}
	return perms.DomainOf(d)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
