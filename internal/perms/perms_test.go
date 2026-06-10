package perms

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDomainOf(t *testing.T) {
	cases := []struct {
		url, want string
		wantErr   bool
	}{
		{url: "https://www.linkedin.com/feed/", want: "linkedin.com"},
		{url: "https://linkedin.com", want: "linkedin.com"},
		{url: "https://sub.deep.linkedin.com/x?y=1", want: "linkedin.com"},
		{url: "http://example.co.uk/path", want: "example.co.uk"},
		{url: "https://foo.example.co.uk", want: "example.co.uk"},
		{url: "http://localhost:8080/x", want: "localhost"},
		{url: "http://127.0.0.1:9999/", want: "127.0.0.1"},
		{url: "http://[::1]:8080/", want: "::1"},
		{url: "http://intranet-host/page", want: "intranet-host"},
		{url: "https://WWW.GitHub.COM/Foo", want: "github.com"},
		{url: "about:blank", want: "about:"},
		{url: "chrome://settings", want: "chrome:"},
		{url: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := DomainOf(c.url)
		if c.wantErr {
			if err == nil {
				t.Errorf("DomainOf(%q): expected error, got %q", c.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("DomainOf(%q): %v", c.url, err)
			continue
		}
		if got != c.want {
			t.Errorf("DomainOf(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestParseAction(t *testing.T) {
	for _, ok := range []string{"read", "Navigate", " INTERACT ", "upload", "download", "eval", "profile"} {
		if _, err := ParseAction(ok); err != nil {
			t.Errorf("ParseAction(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "write", "admin", "rea d"} {
		if _, err := ParseAction(bad); err == nil {
			t.Errorf("ParseAction(%q): expected error", bad)
		}
	}
}

func TestMatrixGrantAllowedRemove(t *testing.T) {
	m := NewMatrix()
	if m.Allowed(Read, "example.com") {
		t.Fatal("empty matrix must allow nothing")
	}
	m.Grant(Read, "example.com")
	if !m.Allowed(Read, "example.com") {
		t.Fatal("granted read×example.com not allowed")
	}
	if m.Allowed(Interact, "example.com") {
		t.Fatal("interact must not be implied by read")
	}
	if m.Allowed(Read, "other.com") {
		t.Fatal("other.com must not be implied")
	}
	// wildcard
	m.Grant(Read, "*")
	if !m.Allowed(Read, "anything.org") {
		t.Fatal("read×* must allow any domain")
	}
	if m.Allowed(Interact, "anything.org") {
		t.Fatal("wildcard must not leak across actions")
	}
	if !m.Remove(Read, "example.com") {
		t.Fatal("remove existing grant should report true")
	}
	if m.Remove(Read, "example.com") {
		t.Fatal("double remove should report false")
	}
	m.Reset()
	if m.Allowed(Read, "anything.org") || len(m.List()) != 0 {
		t.Fatal("reset must clear everything")
	}
}

func TestMatrixListSorted(t *testing.T) {
	m := NewMatrix()
	m.Grant(Interact, "b.com")
	m.Grant(Interact, "a.com")
	m.Grant(Eval, "z.com")
	l := m.List()
	if len(l) != 3 {
		t.Fatalf("want 3 grants, got %d", len(l))
	}
	if l[0].Action != Eval || l[1].Domain != "a.com" || l[2].Domain != "b.com" {
		t.Fatalf("unexpected order: %+v", l)
	}
}

func TestSetsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m := NewMatrix()
	m.Grant(Read, "linkedin.com")
	m.Grant(Interact, "linkedin.com")

	n, err := m.SaveSet(dir, "proj")
	if err != nil || n != 2 {
		t.Fatalf("SaveSet: n=%d err=%v", n, err)
	}

	set, err := ReadSet(dir, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Grants) != 2 || set.Name != "proj" {
		t.Fatalf("bad set: %+v", set)
	}

	m2 := NewMatrix()
	m2.Apply(set)
	if !m2.Allowed(Read, "linkedin.com") || !m2.Allowed(Interact, "linkedin.com") {
		t.Fatal("applied set grants missing")
	}
	if m2.LoadedSet() != "proj" {
		t.Fatalf("LoadedSet = %q", m2.LoadedSet())
	}

	names, err := ListSets(dir)
	if err != nil || len(names) != 1 || names[0] != "proj" {
		t.Fatalf("ListSets = %v, %v", names, err)
	}
}

func TestAppendToSet(t *testing.T) {
	dir := t.TempDir()
	g := Grant{Action: Read, Domain: "x.com"}
	if err := AppendToSet(dir, "default", g); err != nil {
		t.Fatal(err)
	}
	// idempotent
	if err := AppendToSet(dir, "default", g); err != nil {
		t.Fatal(err)
	}
	if err := AppendToSet(dir, "default", Grant{Action: Eval, Domain: "x.com"}); err != nil {
		t.Fatal(err)
	}
	set, err := ReadSet(dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Grants) != 2 {
		t.Fatalf("want 2 grants after dedup, got %+v", set.Grants)
	}
}

func TestSetNameValidation(t *testing.T) {
	dir := t.TempDir()
	m := NewMatrix()
	for _, bad := range []string{"", "../evil", "a/b", `a\b`, "dot.dot"} {
		if _, err := m.SaveSet(dir, bad); err == nil {
			t.Errorf("SaveSet(%q): expected error", bad)
		}
		if err := AppendToSet(dir, bad, Grant{Action: Read, Domain: "x"}); err == nil {
			t.Errorf("AppendToSet(%q): expected error", bad)
		}
	}
	// nothing escaped the dir
	entries, _ := os.ReadDir(filepath.Dir(dir))
	for _, e := range entries {
		if e.Name() == "evil.json" {
			t.Fatal("path traversal escaped the permission-sets dir")
		}
	}
}

func TestReadSetRejectsUnknownAction(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "permission-sets")
	os.MkdirAll(p, 0o700)
	os.WriteFile(filepath.Join(p, "bad.json"), []byte(`{"name":"bad","grants":[{"action":"sudo","domain":"x.com"}]}`), 0o600)
	if _, err := ReadSet(dir, "bad"); err == nil {
		t.Fatal("expected error for unknown action in set file")
	}
}
