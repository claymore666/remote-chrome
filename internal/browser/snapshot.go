package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Snapshot is perception's primary tool (PLAN §4): an accessibility-tree
// dump where every interactive element carries a stable uid that the
// interaction tools accept. uids are valid until the next snapshot or until
// the debugger detaches.
//
// Out-of-process iframes (Stripe, embedded logins, consent managers) are
// separate debugger targets; their trees are fetched via the auto-attached
// child sessions and grafted under their owner <iframe> node, with uids
// prefixed per frame ("f2.e17") so interactions route to the right session.

const maxSnapshotChars = 60_000

// axNode mirrors the wire shape of Accessibility.getFullAXTree nodes.
type axNode struct {
	NodeID  string `json:"nodeId"`
	Ignored bool   `json:"ignored"`
	Role    struct {
		Value string `json:"value"`
	} `json:"role"`
	Name struct {
		Value string `json:"value"`
	} `json:"name"`
	Value struct {
		Value any `json:"value"`
	} `json:"value"`
	Properties []struct {
		Name  string `json:"name"`
		Value struct {
			Value any `json:"value"`
		} `json:"value"`
	} `json:"properties"`
	ChildIDs         []string `json:"childIds"`
	ParentID         string   `json:"parentId"`
	BackendDOMNodeID int      `json:"backendDOMNodeId"`
}

// interactiveRoles get a uid and are accepted by click/type/etc.
var interactiveRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true,
	"checkbox": true, "radio": true, "combobox": true, "listbox": true,
	"option": true, "menuitem": true, "menuitemcheckbox": true,
	"menuitemradio": true, "tab": true, "switch": true, "slider": true,
	"spinbutton": true, "textarea": true, "MenuListPopup": true,
	"DisclosureTriangle": true,
}

// structuralRoles are rendered (without uid) to give the page shape.
var structuralRoles = map[string]bool{
	"heading": true, "navigation": true, "main": true, "banner": true,
	"contentinfo": true, "form": true, "search": true, "dialog": true,
	"alertdialog": true, "alert": true, "table": true, "row": true,
	"columnheader": true, "rowheader": true, "cell": true, "gridcell": true,
	"list": true, "listitem": true, "article": true, "region": true,
	"complementary": true, "img": true, "figure": true, "tabpanel": true,
	"tablist": true, "menu": true, "menubar": true, "toolbar": true,
	"tree": true, "treeitem": true, "RootWebArea": true, "WebArea": true,
	"group": true, "radiogroup": true, "progressbar": true, "status": true,
	"Iframe": true, "IframePresentational": true,
}

// renderedProps are AX properties worth showing to the model.
var renderedProps = map[string]bool{
	"checked": true, "selected": true, "disabled": true, "expanded": true,
	"focused": true, "required": true, "pressed": true, "invalid": true,
	"multiselectable": true, "readonly": true, "valuemin": true,
	"valuemax": true, "level": true,
}

// Snapshot captures the page's a11y tree (main frame + every attached
// OOPIF) and registers fresh uids on t.
func (m *Manager) Snapshot(ctx context.Context, t *Tab) (string, error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return "", err
	}

	frames := t.frameList()
	trees := map[string][]axNode{}
	fetch := func(sess string) error {
		if err := m.ensureDomain(ctx, t, sess, "Accessibility"); err != nil {
			return err
		}
		raw, err := m.cdp(ctx, t, sess, "Accessibility.getFullAXTree", nil)
		if err != nil {
			return err
		}
		var resp struct {
			Nodes []axNode `json:"nodes"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return err
		}
		trees[sess] = resp.Nodes
		return nil
	}
	if err := fetch(""); err != nil {
		return "", fmt.Errorf("accessibility tree: %w", err)
	}

	// Child frames are best-effort: one mid-navigation iframe must not kill
	// the whole snapshot, but its absence must be visible in the output.
	var unavailable []string
	graft := map[string]map[int]*frameInfo{} // parent session -> owner iframe backendNodeId -> child
	for _, f := range frames {
		if err := fetch(f.Session); err != nil {
			unavailable = append(unavailable, fmt.Sprintf("- Iframe [%s] (content unavailable: %s — snapshot again)", f.URL, err))
			continue
		}
		ownerID, err := m.frameOwner(ctx, t, f.Parent, f.Target)
		if err != nil {
			delete(trees, f.Session)
			unavailable = append(unavailable, fmt.Sprintf("- Iframe [%s] (content unavailable: %s — snapshot again)", f.URL, err))
			continue
		}
		if graft[f.Parent] == nil {
			graft[f.Parent] = map[int]*frameInfo{}
		}
		graft[f.Parent][ownerID] = f
	}

	uids := map[string]uidRef{}
	r := &axRenderer{trees: trees, graft: graft, uids: uids}
	text := r.render()
	if len(unavailable) > 0 {
		text += strings.Join(unavailable, "\n") + "\n"
	}

	t.mu.Lock()
	t.uids = uids
	t.snapshotGen++
	gen := t.snapshotGen
	t.mu.Unlock()

	header := fmt.Sprintf("[snapshot #%d — %d interactive elements; pass uid values to click/type/etc.]\n", gen, len(uids))
	return header + text, nil
}

// frameList returns the tab's attached OOPIF frames in attach order.
func (t *Tab) frameList() []*frameInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*frameInfo, 0, len(t.frames))
	for _, f := range t.frames {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
	return out
}

// frameOwner finds the backendNodeId of the <iframe> element hosting a
// child frame, looked up in the parent's session (targetId == frameId for
// iframe targets).
func (m *Manager) frameOwner(ctx context.Context, t *Tab, parentSession, frameID string) (int, error) {
	raw, err := m.cdp(ctx, t, parentSession, "DOM.getFrameOwner", map[string]any{"frameId": frameID})
	if err != nil {
		return 0, fmt.Errorf("frame owner: %w", err)
	}
	var resp struct {
		BackendNodeID int `json:"backendNodeId"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.BackendNodeID == 0 {
		return 0, fmt.Errorf("frame owner not found for frame %s", frameID)
	}
	return resp.BackendNodeID, nil
}

// axRenderer turns per-session flat node lists into one indented outline,
// grafting each child frame's tree under its owner iframe node and
// registering uids as it goes.
type axRenderer struct {
	trees map[string][]axNode
	graft map[string]map[int]*frameInfo
	uids  map[string]uidRef
	sb    strings.Builder
}

func (r *axRenderer) render() string {
	r.renderSession("", 0, 0)
	out := r.sb.String()
	if len(out) > maxSnapshotChars {
		out = out[:maxSnapshotChars] + "\n…[snapshot truncated — use scroll + snapshot again, or query(selector)]"
	}
	return out
}

func (r *axRenderer) renderSession(sess string, frameIdx, depth int) {
	nodes := r.trees[sess]
	byID := make(map[string]*axNode, len(nodes))
	for i := range nodes {
		byID[nodes[i].NodeID] = &nodes[i]
	}
	// Roots: nodes whose parent is absent from the dump.
	for i := range nodes {
		n := &nodes[i]
		if n.ParentID == "" || byID[n.ParentID] == nil {
			r.renderNode(byID, n, sess, frameIdx, depth)
		}
		if r.sb.Len() > maxSnapshotChars {
			return
		}
	}
}

func (r *axRenderer) renderNode(byID map[string]*axNode, n *axNode, sess string, frameIdx, depth int) {
	if r.sb.Len() > maxSnapshotChars {
		return
	}
	role := n.Role.Value
	name := strings.TrimSpace(n.Name.Value)
	interactive := interactiveRoles[role] && n.BackendDOMNodeID != 0
	structural := structuralRoles[role]
	isText := role == "StaticText" || role == "text"
	var child *frameInfo
	if n.BackendDOMNodeID != 0 && r.graft[sess] != nil {
		child = r.graft[sess][n.BackendDOMNodeID]
	}

	show := !n.Ignored && (interactive || (structural && (name != "" || role != "group")) || (isText && name != ""))
	childDepth := depth
	if show {
		r.sb.WriteString(strings.Repeat("  ", depth))
		r.sb.WriteString("- ")
		if isText {
			r.sb.WriteString(fmt.Sprintf("text %q", truncate(name, 300)))
		} else {
			r.sb.WriteString(role)
			if name != "" {
				r.sb.WriteString(fmt.Sprintf(" %q", truncate(name, 200)))
			}
		}
		if interactive {
			uid := fmt.Sprintf("e%d", n.BackendDOMNodeID)
			if frameIdx > 0 {
				uid = fmt.Sprintf("f%d.e%d", frameIdx, n.BackendDOMNodeID)
			}
			r.uids[uid] = uidRef{session: sess, backendID: n.BackendDOMNodeID}
			r.sb.WriteString(" [uid=" + uid + "]")
		}
		if child != nil {
			r.sb.WriteString(" [" + truncate(child.URL, 200) + "]")
		}
		if props := renderProps(n); props != "" {
			r.sb.WriteString(" (" + props + ")")
		}
		if v, ok := n.Value.Value.(string); ok && v != "" {
			r.sb.WriteString(fmt.Sprintf(" value=%q", truncate(v, 200)))
		}
		r.sb.WriteString("\n")
		childDepth = depth + 1
	}
	if child != nil {
		// The OOPIF's content lives in its own session's tree; whatever
		// stub children the parent tree holds for this node would
		// duplicate it.
		r.renderSession(child.Session, child.Idx, childDepth)
		return
	}
	// StaticText children (InlineTextBox) are never useful.
	if isText {
		return
	}
	for _, cid := range n.ChildIDs {
		if c := byID[cid]; c != nil {
			r.renderNode(byID, c, sess, frameIdx, childDepth)
		}
	}
}

// renderAXTree renders a single session's tree (no frames) — the simple
// entry point kept for unit tests.
func renderAXTree(nodes []axNode, uids map[string]uidRef) string {
	r := &axRenderer{trees: map[string][]axNode{"": nodes}, uids: uids}
	return r.render()
}

func renderProps(n *axNode) string {
	var parts []string
	for _, p := range n.Properties {
		if !renderedProps[p.Name] {
			continue
		}
		v := p.Value.Value
		if v == false || v == "false" || v == nil {
			continue
		}
		if v == true || v == "true" {
			parts = append(parts, p.Name)
		} else {
			parts = append(parts, fmt.Sprintf("%s=%v", p.Name, v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// resolveUID maps a snapshot uid back to its owning session + backend DOM
// node id.
func (t *Tab) resolveUID(uid string) (uidRef, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ref, ok := t.uids[uid]
	if !ok {
		if len(t.uids) == 0 {
			return uidRef{}, fmt.Errorf("no snapshot taken for this tab yet (or the page changed) — call snapshot first")
		}
		return uidRef{}, fmt.Errorf("unknown uid %q — it may be from a stale snapshot; call snapshot again", uid)
	}
	if ref.session != "" && t.frames[ref.session] == nil {
		return uidRef{}, fmt.Errorf("uid %q belongs to an iframe that went away (navigated or removed) — call snapshot again", uid)
	}
	return ref, nil
}

// FrameURLForUID reports the document URL of the out-of-process iframe
// owning uid, or "" for main-frame elements (and unknown uids — the
// follow-up action surfaces the proper error). The server gates frame
// interactions on this URL's domain: an embedded third-party widget must
// not inherit the host page's grants.
func (t *Tab) FrameURLForUID(uid string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ref, ok := t.uids[uid]
	if !ok || ref.session == "" {
		return ""
	}
	f := t.frames[ref.session]
	if f == nil {
		return ""
	}
	return f.URL
}
