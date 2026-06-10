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
}

// renderedProps are AX properties worth showing to the model.
var renderedProps = map[string]bool{
	"checked": true, "selected": true, "disabled": true, "expanded": true,
	"focused": true, "required": true, "pressed": true, "invalid": true,
	"multiselectable": true, "readonly": true, "valuemin": true,
	"valuemax": true, "level": true,
}

// Snapshot captures the page's a11y tree and registers fresh uids on t.
func (m *Manager) Snapshot(ctx context.Context, t *Tab) (string, error) {
	if err := m.EnsurePage(ctx, t); err != nil {
		return "", err
	}
	if err := m.ensureDomain(ctx, t, "Accessibility"); err != nil {
		return "", err
	}
	raw, err := m.CDP(ctx, t, "Accessibility.getFullAXTree", nil)
	if err != nil {
		return "", fmt.Errorf("accessibility tree: %w", err)
	}
	var resp struct {
		Nodes []axNode `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse AX tree: %w", err)
	}

	uids := map[string]int{}
	text := renderAXTree(resp.Nodes, uids)

	t.mu.Lock()
	t.uids = uids
	t.snapshotGen++
	gen := t.snapshotGen
	t.mu.Unlock()

	header := fmt.Sprintf("[snapshot #%d — %d interactive elements; pass uid values to click/type/etc.]\n", gen, len(uids))
	return header + text, nil
}

// renderAXTree turns the flat node list into an indented outline,
// registering uids for interactive nodes.
func renderAXTree(nodes []axNode, uids map[string]int) string {
	byID := make(map[string]*axNode, len(nodes))
	for i := range nodes {
		byID[nodes[i].NodeID] = &nodes[i]
	}
	// Roots: nodes whose parent is absent from the dump.
	var roots []*axNode
	for i := range nodes {
		n := &nodes[i]
		if n.ParentID == "" || byID[n.ParentID] == nil {
			roots = append(roots, n)
		}
	}
	var sb strings.Builder
	for _, r := range roots {
		renderNode(&sb, byID, r, 0, uids)
		if sb.Len() > maxSnapshotChars {
			break
		}
	}
	out := sb.String()
	if len(out) > maxSnapshotChars {
		out = out[:maxSnapshotChars] + "\n…[snapshot truncated — use scroll + snapshot again, or query(selector)]"
	}
	return out
}

func renderNode(sb *strings.Builder, byID map[string]*axNode, n *axNode, depth int, uids map[string]int) {
	if sb.Len() > maxSnapshotChars {
		return
	}
	role := n.Role.Value
	name := strings.TrimSpace(n.Name.Value)
	interactive := interactiveRoles[role] && n.BackendDOMNodeID != 0
	structural := structuralRoles[role]
	isText := role == "StaticText" || role == "text"

	show := !n.Ignored && (interactive || (structural && (name != "" || role != "group")) || (isText && name != ""))
	childDepth := depth
	if show {
		sb.WriteString(strings.Repeat("  ", depth))
		sb.WriteString("- ")
		if isText {
			sb.WriteString(fmt.Sprintf("text %q", truncate(name, 300)))
		} else {
			sb.WriteString(role)
			if name != "" {
				sb.WriteString(fmt.Sprintf(" %q", truncate(name, 200)))
			}
		}
		if interactive {
			uid := fmt.Sprintf("e%d", n.BackendDOMNodeID)
			uids[uid] = n.BackendDOMNodeID
			sb.WriteString(" [uid=" + uid + "]")
		}
		if props := renderProps(n); props != "" {
			sb.WriteString(" (" + props + ")")
		}
		if v, ok := n.Value.Value.(string); ok && v != "" {
			sb.WriteString(fmt.Sprintf(" value=%q", truncate(v, 200)))
		}
		sb.WriteString("\n")
		childDepth = depth + 1
	}
	// StaticText children (InlineTextBox) are never useful.
	if isText {
		return
	}
	for _, cid := range n.ChildIDs {
		if c := byID[cid]; c != nil {
			renderNode(sb, byID, c, childDepth, uids)
		}
	}
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

// resolveUID maps a snapshot uid back to a backend DOM node id.
func (t *Tab) resolveUID(uid string) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.uids[uid]
	if !ok {
		if len(t.uids) == 0 {
			return 0, fmt.Errorf("no snapshot taken for this tab yet (or the page changed) — call snapshot first")
		}
		return 0, fmt.Errorf("unknown uid %q — it may be from a stale snapshot; call snapshot again", uid)
	}
	return id, nil
}
