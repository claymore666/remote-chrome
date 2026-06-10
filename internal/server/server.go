// Package server wires the MCP tool surface to the browser manager, with
// every consequential tool gated by the permission matrix + human approval.
//
// The invariant (CLAUDE.md #1): every gated tool goes through ensureGrant
// BEFORE any command reaches the bridge. The model can request anything;
// nothing executes without a grant; only the human creates grants.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"browserd/internal/approval"
	"browserd/internal/audit"
	"browserd/internal/browser"
	"browserd/internal/config"
	"browserd/internal/perms"
)

const Version = "0.1.0"

// Bridger is the slice of bridge.Bridge the server needs directly (the rest
// goes through browser.Manager); an interface so module tests can fake the
// browser side entirely.
type Bridger interface {
	Profiles() []string
	DetachAll(ctx context.Context)
}

type Server struct {
	MCP    *mcp.Server
	cfg    *config.Config
	dir    string // state dir (config, audit, permission sets)
	bridge Bridger
	mgr    *browser.Manager
	matrix *perms.Matrix
	aud    *audit.Log
	log    *slog.Logger

	// approvalMu serializes human dialogs: parallel tool calls must not
	// stack five dialogs on the screen, and the first approval may already
	// cover the second call.
	approvalMu sync.Mutex
}

func New(cfg *config.Config, dir string, br Bridger, mgr *browser.Manager, aud *audit.Log, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:    cfg,
		dir:    dir,
		bridge: br,
		mgr:    mgr,
		matrix: perms.NewMatrix(),
		aud:    aud,
		log:    log,
	}
	s.MCP = mcp.NewServer(&mcp.Implementation{
		Name:    "browserd",
		Title:   "Browser control (your real Chrome, approval-gated)",
		Version: Version,
	}, nil)
	s.registerTools()
	return s
}

// Run serves MCP on stdio until the client disconnects or ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	return s.MCP.Run(ctx, &mcp.StdioTransport{})
}

// ---- profile / tab resolution ----

// resolveProfile picks the target profile and gates non-default targeting
// (PLAN §3: "Targeting a different browser profile is itself a grant").
func (s *Server) resolveProfile(ctx context.Context, session *mcp.ServerSession, requested string) (string, error) {
	connected := s.bridge.Profiles()
	if len(connected) == 0 {
		return "", fmt.Errorf("no browser extension connected — is Chrome running with the browserd bridge extension configured? (run `browserd setup` for instructions)")
	}
	def := s.cfg.DefaultProfile
	if def == "" && len(connected) == 1 {
		def = connected[0]
	}
	target := requested
	if target == "" {
		target = def
	}
	if target == "" {
		return "", fmt.Errorf("several profiles are connected (%s) and no default_profile is configured — pass profile explicitly", strings.Join(connected, ", "))
	}
	found := false
	for _, c := range connected {
		if c == target {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("profile %q is not connected (connected: %s)", target, strings.Join(connected, ", "))
	}
	if target != def {
		if err := s.ensureGrant(ctx, session, []perms.Action{perms.Profile}, target,
			fmt.Sprintf("target the %q browser profile", target)); err != nil {
			return "", err
		}
	}
	return target, nil
}

// targetTab resolves tab_id 0 to the profile's active tab.
func (s *Server) targetTab(ctx context.Context, profile string, tabID int) (int, error) {
	if tabID != 0 {
		return tabID, nil
	}
	tabs, err := s.mgr.ListTabs(ctx, []string{profile})
	if err != nil {
		return 0, err
	}
	for _, t := range tabs {
		if t.Active {
			return t.TabID, nil
		}
	}
	return 0, fmt.Errorf("profile %q has no active tab", profile)
}

// tabDomain returns the eTLD+1 of the tab's current URL plus the raw URL.
func (s *Server) tabDomain(ctx context.Context, profile string, tabID int) (domain, url string, err error) {
	url, _, err = s.mgr.TabURL(ctx, profile, tabID)
	if err != nil {
		return "", "", err
	}
	if url == "" {
		return "", "", fmt.Errorf("tab %d has no URL (still loading?)", tabID)
	}
	domain, err = perms.DomainOf(url)
	if err != nil {
		return "", "", err
	}
	return domain, url, nil
}

// gateTab is the standard gate: resolve profile + tab, derive the domain
// from the tab's CURRENT url, require action×domain.
func (s *Server) gateTab(ctx context.Context, req *mcp.CallToolRequest, action perms.Action, profileArg string, tabArg int, reason string) (profile string, tabID int, err error) {
	profile, err = s.resolveProfile(ctx, req.Session, profileArg)
	if err != nil {
		return "", 0, err
	}
	tabID, err = s.targetTab(ctx, profile, tabArg)
	if err != nil {
		return "", 0, err
	}
	domain, _, err := s.tabDomain(ctx, profile, tabID)
	if err != nil {
		return "", 0, err
	}
	if err := s.ensureGrant(ctx, req.Session, []perms.Action{action}, domain, reason); err != nil {
		return "", 0, err
	}
	return profile, tabID, nil
}

// ---- grants & approval ----

// ensureGrant blocks until action(s)×domain are granted, raising one human
// approval if needed. Returns a structured error on denial so the model can
// adapt or ask the user in chat (PLAN §3).
func (s *Server) ensureGrant(ctx context.Context, session *mcp.ServerSession, actions []perms.Action, domain, reason string) error {
	missing := s.missingGrants(actions, domain)
	if len(missing) == 0 {
		return nil
	}

	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	// Re-check under the lock: a parallel call's approval may cover us now.
	if missing = s.missingGrants(actions, domain); len(missing) == 0 {
		return nil
	}

	areq := approval.Request{Actions: missing, Domain: domain, Reason: reason}
	decision, err := s.askHuman(ctx, session, areq)
	if err != nil {
		s.audit(audit.Entry{Kind: "approval", Domain: domain, Action: joinActions(missing), Decision: "error", Detail: map[string]any{"err": err.Error()}})
		return fmt.Errorf("approval unavailable: %w", err)
	}
	s.audit(audit.Entry{Kind: "approval", Domain: domain, Action: joinActions(missing), Decision: string(decision), Detail: map[string]any{"reason": reason}})

	switch decision {
	case approval.Deny:
		return fmt.Errorf("permission denied by user: %s on %s — you may explain in chat why it is needed, or proceed differently; the user can also load a permission set", joinActions(missing), domain)
	case approval.Once:
		return nil // allowed, nothing stored
	case approval.Save:
		for _, a := range missing {
			s.matrix.Grant(a, domain)
			g := perms.Grant{Action: a, Domain: domain}
			if err := perms.AppendToSet(s.dir, s.cfg.PermissionSet, g); err != nil {
				s.log.Warn("could not persist grant to set", "set", s.cfg.PermissionSet, "err", err)
			}
			s.audit(audit.Entry{Kind: "grant", Domain: domain, Action: string(a), Decision: "saved:" + s.cfg.PermissionSet})
		}
		return nil
	default: // Session
		for _, a := range missing {
			s.matrix.Grant(a, domain)
			s.audit(audit.Entry{Kind: "grant", Domain: domain, Action: string(a), Decision: "session"})
		}
		return nil
	}
}

func (s *Server) missingGrants(actions []perms.Action, domain string) []perms.Action {
	var missing []perms.Action
	for _, a := range actions {
		if !s.matrix.Allowed(a, domain) {
			missing = append(missing, a)
		}
	}
	return missing
}

// askHuman raises the approval dialog: MCP elicitation when the client
// supports it, else the native OS dialog. Config can force either.
func (s *Server) askHuman(ctx context.Context, session *mcp.ServerSession, areq approval.Request) (approval.Decision, error) {
	mode := s.cfg.Approval
	canElicit := false
	if session != nil {
		if init := session.InitializeParams(); init != nil && init.Capabilities != nil && init.Capabilities.Elicitation != nil {
			canElicit = true
		}
	}
	useElicit := mode == "elicit" || (mode != "dialog" && canElicit)
	if useElicit && session != nil {
		return s.elicit(ctx, session, areq)
	}
	return approval.AskDialog(ctx, areq)
}

func (s *Server) elicit(ctx context.Context, session *mcp.ServerSession, areq approval.Request) (approval.Decision, error) {
	res, err := session.Elicit(ctx, &mcp.ElicitParams{
		Message: areq.Message(),
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"decision": map[string]any{
					"type": "string",
					"enum": []string{string(approval.Once), string(approval.Session), string(approval.Save), string(approval.Deny)},
					"description": fmt.Sprintf("once = this call only; this session = until disconnect; save to set = persist into permission set %q; suggested: %s",
						s.cfg.PermissionSet, areq.DefaultDecision()),
				},
			},
			"required": []string{"decision"},
		},
	})
	if err != nil {
		return approval.Deny, fmt.Errorf("elicitation failed: %w", err)
	}
	if res.Action != "accept" {
		return approval.Deny, nil
	}
	raw, _ := res.Content["decision"].(string)
	d, perr := approval.ParseDecision(raw)
	if perr != nil {
		return approval.Deny, nil
	}
	return d, nil
}

// navigationDenied enforces the config deny list, independent of grants.
func (s *Server) navigationDenied(domain string) bool {
	for _, d := range s.cfg.DenyNavigation {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}

func (s *Server) audit(e audit.Entry) {
	s.aud.Write(e)
}

func joinActions(as []perms.Action) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = string(a)
	}
	return strings.Join(parts, "+")
}

// Matrix exposes the live matrix (tests, diagnostics).
func (s *Server) Matrix() *perms.Matrix { return s.matrix }
