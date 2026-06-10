package browser

import (
	"encoding/json"
	"strings"
	"testing"
)

// canned AX tree: RootWebArea > heading, StaticText, button, link, ignored generic > textbox
const axFixture = `[
  {"nodeId":"1","ignored":false,"role":{"value":"RootWebArea"},"name":{"value":"Demo page"},"childIds":["2","3","4","5","6"],"backendDOMNodeId":100},
  {"nodeId":"2","parentId":"1","ignored":false,"role":{"value":"heading"},"name":{"value":"Welcome"},"properties":[{"name":"level","value":{"value":1}}],"childIds":[],"backendDOMNodeId":101},
  {"nodeId":"3","parentId":"1","ignored":false,"role":{"value":"StaticText"},"name":{"value":"Some intro text"},"childIds":["7"],"backendDOMNodeId":102},
  {"nodeId":"4","parentId":"1","ignored":false,"role":{"value":"button"},"name":{"value":"Submit order"},"properties":[{"name":"disabled","value":{"value":true}}],"childIds":[],"backendDOMNodeId":103},
  {"nodeId":"5","parentId":"1","ignored":false,"role":{"value":"link"},"name":{"value":"Help"},"childIds":[],"backendDOMNodeId":104},
  {"nodeId":"6","parentId":"1","ignored":true,"role":{"value":"generic"},"name":{"value":""},"childIds":["8"],"backendDOMNodeId":105},
  {"nodeId":"7","parentId":"3","ignored":false,"role":{"value":"InlineTextBox"},"name":{"value":"Some intro text"},"childIds":[],"backendDOMNodeId":106},
  {"nodeId":"8","parentId":"6","ignored":false,"role":{"value":"textbox"},"name":{"value":"Email"},"value":{"value":"a@b.c"},"childIds":[],"backendDOMNodeId":107}
]`

func parseFixture(t *testing.T, s string) []axNode {
	t.Helper()
	var nodes []axNode
	if err := json.Unmarshal([]byte(s), &nodes); err != nil {
		t.Fatal(err)
	}
	return nodes
}

func TestRenderAXTree(t *testing.T) {
	nodes := parseFixture(t, axFixture)
	uids := map[string]uidRef{}
	out := renderAXTree(nodes, uids)

	// Interactive elements get uids keyed by backend node id.
	if uids["e103"].backendID != 103 || uids["e104"].backendID != 104 || uids["e107"].backendID != 107 {
		t.Fatalf("expected uids e103/e104/e107, got %v", uids)
	}
	// Non-interactive nodes must not get uids.
	if len(uids) != 3 {
		t.Fatalf("expected exactly 3 uids, got %v", uids)
	}
	for _, want := range []string{
		`RootWebArea "Demo page"`,
		`heading "Welcome" (level=1)`,
		`text "Some intro text"`,
		`button "Submit order" [uid=e103] (disabled)`,
		`link "Help" [uid=e104]`,
		`textbox "Email" [uid=e107]`,
		`value="a@b.c"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q in:\n%s", want, out)
		}
	}
	// The ignored generic wrapper itself must not be rendered, but its
	// textbox child must be (children of skipped nodes are lifted).
	if strings.Contains(out, "generic") {
		t.Errorf("ignored node was rendered:\n%s", out)
	}
	// InlineTextBox children of StaticText are dropped.
	if strings.Count(out, "Some intro text") != 1 {
		t.Errorf("InlineTextBox duplicated text:\n%s", out)
	}
}

func TestRenderAXTreeIndentation(t *testing.T) {
	nodes := parseFixture(t, axFixture)
	out := renderAXTree(nodes, map[string]uidRef{})
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "heading") && !strings.HasPrefix(line, "  - ") {
			t.Errorf("child of root not indented: %q", line)
		}
		if strings.Contains(line, "RootWebArea") && !strings.HasPrefix(line, "- ") {
			t.Errorf("root should not be indented: %q", line)
		}
	}
}

func TestRenderAXTreeTruncation(t *testing.T) {
	// Build a tree large enough to exceed the cap.
	var nodes []axNode
	root := axNode{NodeID: "1", BackendDOMNodeID: 1}
	root.Role.Value = "RootWebArea"
	root.Name.Value = "big"
	for i := 2; i < 4000; i++ {
		n := axNode{NodeID: jsonID(i), ParentID: "1", BackendDOMNodeID: i}
		n.Role.Value = "link"
		n.Name.Value = strings.Repeat("x", 40)
		nodes = append(nodes, n)
		root.ChildIDs = append(root.ChildIDs, n.NodeID)
	}
	nodes = append([]axNode{root}, nodes...)
	out := renderAXTree(nodes, map[string]uidRef{})
	if len(out) > maxSnapshotChars+200 {
		t.Fatalf("snapshot not truncated: %d chars", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("truncation note missing")
	}
}

func jsonID(i int) string {
	return string(rune('a'+i%26)) + string(rune('0'+i%10)) + jsonIDNum(i)
}

func jsonIDNum(i int) string {
	out := ""
	for i > 0 {
		out += string(rune('0' + i%10))
		i /= 10
	}
	return out
}

func TestResolveUID(t *testing.T) {
	tab := &Tab{uids: map[string]uidRef{}, frames: map[string]*frameInfo{}}
	if _, err := tab.resolveUID("e1"); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("expected 'take a snapshot' error, got %v", err)
	}
	tab.uids["e1"] = uidRef{backendID: 1}
	if ref, err := tab.resolveUID("e1"); err != nil || ref.backendID != 1 || ref.session != "" {
		t.Fatalf("resolveUID = %+v, %v", ref, err)
	}
	if _, err := tab.resolveUID("e999"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale-uid error, got %v", err)
	}
	// A uid whose iframe session has detached must error as stale, never act.
	tab.uids["f1.e5"] = uidRef{session: "sess-gone", backendID: 5}
	if _, err := tab.resolveUID("f1.e5"); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("expected gone-iframe error, got %v", err)
	}
}

// TestRenderAXTreeGraftsIframe covers the OOPIF path: the child session's
// tree renders under the owner Iframe node, child uids carry the frame
// prefix and resolve to the child session.
func TestRenderAXTreeGraftsIframe(t *testing.T) {
	main := parseFixture(t, `[
	  {"nodeId":"1","ignored":false,"role":{"value":"RootWebArea"},"name":{"value":"Host"},"childIds":["2","3"],"backendDOMNodeId":100},
	  {"nodeId":"2","parentId":"1","ignored":false,"role":{"value":"Iframe"},"name":{"value":""},"childIds":["9"],"backendDOMNodeId":200},
	  {"nodeId":"3","parentId":"1","ignored":false,"role":{"value":"button"},"name":{"value":"Host button"},"childIds":[],"backendDOMNodeId":101},
	  {"nodeId":"9","parentId":"2","ignored":false,"role":{"value":"generic"},"name":{"value":"stub"},"childIds":[],"backendDOMNodeId":201}
	]`)
	child := parseFixture(t, `[
	  {"nodeId":"1","ignored":false,"role":{"value":"RootWebArea"},"name":{"value":"Widget"},"childIds":["2"],"backendDOMNodeId":1},
	  {"nodeId":"2","parentId":"1","ignored":false,"role":{"value":"button"},"name":{"value":"Pay now"},"childIds":[],"backendDOMNodeId":42}
	]`)
	f := &frameInfo{Session: "sessA", Parent: "", Target: "frame1", URL: "https://pay.example/checkout", Idx: 1}
	uids := map[string]uidRef{}
	r := &axRenderer{
		trees: map[string][]axNode{"": main, "sessA": child},
		graft: map[string]map[int]*frameInfo{"": {200: f}},
		uids:  uids,
	}
	out := r.render()

	if !strings.Contains(out, `Iframe [https://pay.example/checkout]`) {
		t.Errorf("iframe line missing URL:\n%s", out)
	}
	if !strings.Contains(out, `button "Pay now" [uid=f1.e42]`) {
		t.Errorf("child content not grafted with frame-prefixed uid:\n%s", out)
	}
	if uids["f1.e42"] != (uidRef{session: "sessA", backendID: 42}) {
		t.Errorf("child uid must resolve to the child session: %+v", uids["f1.e42"])
	}
	if uids["e101"] != (uidRef{backendID: 101}) {
		t.Errorf("main uid must stay session-less: %+v", uids["e101"])
	}
	// The parent's stub children for the iframe node must not render —
	// the grafted tree replaces them.
	if strings.Contains(out, "stub") {
		t.Errorf("parent stub children rendered alongside graft:\n%s", out)
	}
	// Child tree nests under the iframe line: host root (0) > Iframe (1) >
	// child RootWebArea (2) > button (3).
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `RootWebArea "Widget"`) && !strings.HasPrefix(line, "    - ") {
			t.Errorf("child root not indented under iframe: %q", line)
		}
		if strings.Contains(line, "Pay now") && !strings.HasPrefix(line, "      - ") {
			t.Errorf("grafted node not indented under child root: %q", line)
		}
	}
}
