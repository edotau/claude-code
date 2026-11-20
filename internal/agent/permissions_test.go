package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/permission"
)

func mustPerms(t *testing.T, mode string, allow, ask, deny []string) permission.Permissions {
	t.Helper()
	p, err := permission.ParsePermissions(mode, allow, ask, deny)
	if err != nil {
		t.Fatalf("ParsePermissions: %v", err)
	}
	return p
}

// Given RunOptions, Then a zero Permissions is omitted from JSON and a populated one round-trips.
func TestPermissionRunOptionsJSON(t *testing.T) {
	b, _ := json.Marshal(RunOptions{Prompt: "x"})
	if strings.Contains(string(b), "permissions") {
		t.Errorf("zero Permissions serialised: %s", b)
	}
	in := RunOptions{Prompt: "x", Permissions: mustPerms(t, "plan", []string{"Read"}, []string{"Bash(git push:*)"}, []string{"Edit(.env*)"})}
	b, _ = json.Marshal(in)
	var out RunOptions
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Permissions.Mode != "plan" || len(out.Permissions.Allow) != 1 || out.Permissions.Ask[0] != (permission.Rule{Tool: "Bash", Arg: "git push:*"}) || out.Permissions.Deny[0].Arg != ".env*" {
		t.Errorf("round-trip lost data: %s -> %+v", b, out.Permissions)
	}
}

// TestSafeModeDenyFindsFirstUnenforceable: only under --safe-mode, and only a Deny rule the predicate flags.
func TestSafeModeDenyFindsFirstUnenforceable(t *testing.T) {
	bash := func(r permission.Rule) bool { return r.Tool == "Bash" }
	perms := permission.Permissions{Allow: []permission.Rule{{Tool: "Bash"}}, Deny: []permission.Rule{{Tool: "Read"}, {Tool: "Bash", Arg: "rm:*"}}}
	if r, ok := SafeModeDeny(RunOptions{SafeMode: true, Permissions: perms}, bash); !ok || r.String() != "Bash(rm:*)" {
		t.Errorf("safe mode = %v %v, want Bash(rm:*)", r, ok)
	}
	if _, ok := SafeModeDeny(RunOptions{Permissions: perms}, bash); ok {
		t.Error("outside safe mode a Deny must not be refused")
	}
	if _, ok := SafeModeDeny(RunOptions{SafeMode: true, Permissions: permission.Permissions{Allow: perms.Allow}}, bash); ok {
		t.Error("an Allow rule must not count as an unenforceable Deny")
	}
}

func permsJSON(t *testing.T, p permission.Permissions) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
