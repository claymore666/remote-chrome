// Package approval models the human approval decision for permission grants
// and implements the native-dialog fallback (zenity on Linux, PowerShell on
// Windows) for MCP clients without elicitation support.
//
// The elicitation-based approver lives in internal/server (it needs the live
// MCP session); both paths share the Request/Decision types here so the
// semantics cannot drift.
package approval

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"browserd/internal/perms"
)

type Decision string

const (
	Once    Decision = "once"         // allow this single call, store nothing
	Session Decision = "this session" // grant in the live matrix
	Save    Decision = "save to set"  // grant + persist into the project permission set
	Deny    Decision = "deny"
)

// DialogTimeout bounds how long a tool call may block on a human dialog.
const DialogTimeout = 5 * time.Minute

type Request struct {
	Actions []perms.Action
	Domain  string
	Reason  string
}

// Message renders the question shown to the human, identical across the
// elicitation and native-dialog paths.
func (r Request) Message() string {
	acts := make([]string, len(r.Actions))
	for i, a := range r.Actions {
		acts[i] = string(a)
	}
	msg := fmt.Sprintf("Allow Claude %s on %s?", strings.Join(acts, " + "), r.Domain)
	if r.Reason != "" {
		msg += fmt.Sprintf(" — reason: %s", r.Reason)
	}
	return msg
}

// DefaultDecision suggests the dialog's preselected granularity per PLAN §3:
// read/navigate/interact default to "this session"; upload/download/eval and
// profile targeting default to "once".
func (r Request) DefaultDecision() Decision {
	for _, a := range r.Actions {
		switch a {
		case perms.Upload, perms.Download, perms.Eval, perms.Profile:
			return Once
		}
	}
	return Session
}

func ParseDecision(s string) (Decision, error) {
	switch Decision(strings.ToLower(strings.TrimSpace(s))) {
	case Once:
		return Once, nil
	case Session:
		return Session, nil
	case Save:
		return Save, nil
	case Deny:
		return Deny, nil
	}
	return "", fmt.Errorf("unknown decision %q", s)
}

// AskDialog raises a native OS dialog and blocks for the user's decision.
// Any error (no zenity, dialog dismissed, timeout) resolves to Deny.
func AskDialog(ctx context.Context, req Request) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, DialogTimeout)
	defer cancel()
	switch runtime.GOOS {
	case "windows":
		return askPowershell(ctx, req)
	default:
		return askZenity(ctx, req)
	}
}

// Confirm raises a plain yes/no native dialog (used for loading permission
// sets). Errors and dismissals resolve to false.
func Confirm(ctx context.Context, message string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, DialogTimeout)
	defer cancel()
	if runtime.GOOS == "windows" {
		script := fmt.Sprintf(`Add-Type -AssemblyName PresentationFramework;`+
			`$r=[System.Windows.MessageBox]::Show(%q,'browserd','YesNo','Warning');Write-Output $r`, message)
		out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
		if err != nil {
			return false, nil
		}
		return strings.TrimSpace(string(out)) == "Yes", nil
	}
	if _, err := exec.LookPath("zenity"); err != nil {
		return false, fmt.Errorf("zenity not found — install it or use an MCP client with elicitation support")
	}
	err := exec.CommandContext(ctx, "zenity", "--question", "--title", "browserd", "--text", message).Run()
	return err == nil, nil
}

func askZenity(ctx context.Context, req Request) (Decision, error) {
	if _, err := exec.LookPath("zenity"); err != nil {
		return Deny, fmt.Errorf("zenity not found — install it or use an MCP client with elicitation support")
	}
	def := req.DefaultDecision()
	args := []string{
		"--list", "--radiolist",
		"--title", "browserd permission request",
		"--text", req.Message(),
		"--column", "", "--column", "decision",
		"--height", "320",
	}
	for _, d := range []Decision{Once, Session, Save, Deny} {
		sel := "FALSE"
		if d == def {
			sel = "TRUE"
		}
		args = append(args, sel, string(d))
	}
	out, err := exec.CommandContext(ctx, "zenity", args...).Output()
	if err != nil {
		// Non-zero exit = dialog cancelled/closed/timeout → deny, not an error.
		return Deny, nil
	}
	d, perr := ParseDecision(string(out))
	if perr != nil {
		return Deny, nil
	}
	return d, nil
}

func askPowershell(ctx context.Context, req Request) (Decision, error) {
	// Three-way message box: Yes = default granularity, No = once, Cancel = deny.
	def := req.DefaultDecision()
	script := fmt.Sprintf(`Add-Type -AssemblyName PresentationFramework;`+
		`$r=[System.Windows.MessageBox]::Show(%q,'browserd permission request','YesNoCancel','Warning');`+
		`Write-Output $r`,
		req.Message()+fmt.Sprintf("\n\nYes = %s   No = once   Cancel = deny", def))
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return Deny, nil
	}
	switch strings.TrimSpace(string(out)) {
	case "Yes":
		return def, nil
	case "No":
		return Once, nil
	default:
		return Deny, nil
	}
}
