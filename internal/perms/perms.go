// Package perms implements the (action group × eTLD+1) permission matrix.
//
// The rule: Claude can request anything; nothing executes without a matching
// grant; only the human creates grants (via approval dialogs in the server
// layer). Every session starts with an empty matrix.
package perms

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"
)

// Action groups, per PLAN.md §3.
type Action string

const (
	Read     Action = "read"     // snapshot, read_page, get_dom, query, screenshot, console/network
	Navigate Action = "navigate" // navigate, back/forward/reload, tabs, wait_for
	Interact Action = "interact" // click, type, select, check, hover, scroll, press_key
	Upload   Action = "upload"   // upload_file
	Download Action = "download" // reserved (downloads not in v1)
	Eval     Action = "eval"     // eval_js
	Profile  Action = "profile"  // targeting a non-default profile; "domain" = profile label
)

var ValidActions = []Action{Read, Navigate, Interact, Upload, Download, Eval, Profile}

func ParseAction(s string) (Action, error) {
	a := Action(strings.ToLower(strings.TrimSpace(s)))
	for _, v := range ValidActions {
		if a == v {
			return a, nil
		}
	}
	return "", fmt.Errorf("unknown action group %q (valid: read, navigate, interact, upload, download, eval, profile)", s)
}

// DomainOf reduces a URL to its registrable domain (eTLD+1):
// https://www.linkedin.com/feed -> linkedin.com. Hosts without a public
// suffix (localhost, IPs, intranet names) are used verbatim, lowercased.
func DomainOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("unparseable URL %q: %w", rawURL, err)
	}
	// Non-web schemes (about:blank, chrome://settings, file://…) map to the
	// scheme as a pseudo-domain ("chrome:"), so grants for them are possible
	// but can never accidentally match a real site.
	if u.Scheme != "http" && u.Scheme != "https" {
		if u.Scheme == "" {
			return "", fmt.Errorf("URL %q has no scheme", rawURL)
		}
		return u.Scheme + ":", nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("URL %q has no host", rawURL)
	}
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host, nil // localhost, single-label intranet names, etc.
	}
	return etld1, nil
}

// Grant is one cell of the matrix.
type Grant struct {
	Action Action `json:"action"`
	Domain string `json:"domain"` // eTLD+1, "*", or a profile label for Action=Profile
}

// Matrix is the session's live permission state. Safe for concurrent use.
type Matrix struct {
	mu     sync.RWMutex
	grants map[Action]map[string]bool
	loaded string // name of the loaded permission set, for display
}

func NewMatrix() *Matrix {
	return &Matrix{grants: map[Action]map[string]bool{}}
}

// Allowed reports whether action×domain is granted (exact or wildcard).
func (m *Matrix) Allowed(a Action, domain string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d := m.grants[a]
	return d != nil && (d[domain] || d["*"])
}

func (m *Matrix) Grant(a Action, domain string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.grants[a] == nil {
		m.grants[a] = map[string]bool{}
	}
	m.grants[a][domain] = true
}

// Remove deletes a grant. Removing is always free (shrinking access is safe).
func (m *Matrix) Remove(a Action, domain string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.grants[a] == nil || !m.grants[a][domain] {
		return false
	}
	delete(m.grants[a], domain)
	return true
}

func (m *Matrix) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants = map[Action]map[string]bool{}
	m.loaded = ""
}

func (m *Matrix) LoadedSet() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.loaded
}

// List returns all grants sorted by action then domain.
func (m *Matrix) List() []Grant {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Grant
	for a, doms := range m.grants {
		for d := range doms {
			out = append(out, Grant{Action: a, Domain: d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Action != out[j].Action {
			return out[i].Action < out[j].Action
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}

// ---- Named permission sets (per-project), stored as JSON files ----

type Set struct {
	Name   string  `json:"name"`
	Grants []Grant `json:"grants"`
}

func setPath(dir, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return "", fmt.Errorf("invalid permission set name %q (no path separators or dots)", name)
	}
	return filepath.Join(dir, "permission-sets", name+".json"), nil
}

// SaveSet writes the matrix's current grants as the named set.
func (m *Matrix) SaveSet(dir, name string) (int, error) {
	path, err := setPath(dir, name)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	set := Set{Name: name, Grants: m.List()}
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return 0, err
	}
	return len(set.Grants), os.WriteFile(path, data, 0o600)
}

// AppendToSet adds one grant to the named set on disk (creating it if
// needed) without touching the rest of the set.
func AppendToSet(dir, name string, g Grant) error {
	set, err := ReadSet(dir, name)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		set = &Set{Name: name}
	}
	for _, have := range set.Grants {
		if have == g {
			return nil
		}
	}
	set.Grants = append(set.Grants, g)
	path, err := setPath(dir, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ReadSet loads a named set from disk without applying it.
func ReadSet(dir, name string) (*Set, error) {
	path, err := setPath(dir, name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var set Set
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, g := range set.Grants {
		if _, err := ParseAction(string(g.Action)); err != nil {
			return nil, fmt.Errorf("set %q: %w", name, err)
		}
	}
	return &set, nil
}

// Apply merges a set's grants into the matrix and records its name.
func (m *Matrix) Apply(set *Set) {
	for _, g := range set.Grants {
		m.Grant(g.Action, g.Domain)
	}
	m.mu.Lock()
	m.loaded = set.Name
	m.mu.Unlock()
}

// ListSets returns the names of saved sets.
func ListSets(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "permission-sets"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}
