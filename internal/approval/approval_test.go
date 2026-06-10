package approval

import (
	"strings"
	"testing"

	"browserd/internal/perms"
)

func TestMessage(t *testing.T) {
	r := Request{Actions: []perms.Action{perms.Read, perms.Interact}, Domain: "instagram.com", Reason: "post your summary"}
	msg := r.Message()
	for _, want := range []string{"read + interact", "instagram.com", "post your summary"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	noReason := Request{Actions: []perms.Action{perms.Eval}, Domain: "x.com"}
	if strings.Contains(noReason.Message(), "reason") {
		t.Errorf("message should omit empty reason: %q", noReason.Message())
	}
}

// Dialog defaults per PLAN §3: read/navigate/interact -> session;
// upload/download/eval/profile -> once.
func TestDefaultDecision(t *testing.T) {
	cases := []struct {
		actions []perms.Action
		want    Decision
	}{
		{[]perms.Action{perms.Read}, Session},
		{[]perms.Action{perms.Navigate, perms.Interact}, Session},
		{[]perms.Action{perms.Upload}, Once},
		{[]perms.Action{perms.Eval}, Once},
		{[]perms.Action{perms.Download}, Once},
		{[]perms.Action{perms.Profile}, Once},
		{[]perms.Action{perms.Read, perms.Eval}, Once}, // most sensitive wins
	}
	for _, c := range cases {
		if got := (Request{Actions: c.actions}).DefaultDecision(); got != c.want {
			t.Errorf("DefaultDecision(%v) = %q, want %q", c.actions, got, c.want)
		}
	}
}

func TestParseDecision(t *testing.T) {
	for in, want := range map[string]Decision{
		"once": Once, "This Session": Session, " save to set ": Save, "deny": Deny,
	} {
		got, err := ParseDecision(in)
		if err != nil || got != want {
			t.Errorf("ParseDecision(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseDecision("always"); err == nil {
		t.Error("ParseDecision(always) should fail")
	}
}
