package actions

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTableArgv(t *testing.T) {
	for _, c := range []struct{ name, args, want string }{
		{"gate", ``, "worktree gate --run"},
		{"gate", `{"scope":["internal/pi","cmd"]}`, "worktree gate --run --scope internal/pi,cmd"},
		{"diff", `{"paths":["a.go","b"],"check":true}`, "agents diff --format json --path a.go --path b --check"},
	} {
		got, ok, err := Argv(c.name, json.RawMessage(c.args))
		if !ok || err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%s %s = %q, %v, %v; want %q", c.name, c.args, got, ok, err, c.want)
		}
	}
	for _, c := range []struct{ name, args string }{
		{"gate", `{"scope":["--run"]}`}, {"gate", `{"scope":["../x"]}`}, {"gate", `{"scope":[""]}`}, {"gate", `{"x":1}`},
		{"diff", `{"paths":["-x"]}`}, {"diff", `{"paths":[""]}`},
	} {
		if _, _, err := Argv(c.name, json.RawMessage(c.args)); err == nil {
			t.Errorf("%s %s: want an error", c.name, c.args)
		}
	}
}

func TestHarnessMCPReviewArguments(t *testing.T) {
	for _, name := range []string{"gate", "diff"} {
		for _, raw := range []string{`null`, `[]`, `{} {}`, `{} rubbish`, `{"unknown":true}`, `{"scope":[]}` + strings.Repeat(" ", 65536)} {
			if _, _, err := Argv(name, json.RawMessage(raw)); err == nil {
				t.Errorf("%s accepted %q", name, raw[:min(60, len(raw))])
			}
		}
		key := "scope"
		if name == "diff" {
			key = "paths"
		}
		raw := `{"` + key + `":[` + strings.Repeat(`"x",`, 256) + `"x"]}`
		if _, _, err := Argv(name, json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepted oversized array", name)
		}
	}
	for _, raw := range []string{`{"paths":["a,b"]}`, `{"paths":["a\u0000b"]}`} {
		if _, _, err := Argv("diff", json.RawMessage(raw)); err == nil {
			t.Errorf("diff accepted %s", raw)
		}
	}
	// Absolute diff paths are deliberately retained: agents diff validates them against its repo root.
	if got, _, err := Argv("diff", json.RawMessage(`{"paths":["/repo/a"]}`)); err != nil || got[len(got)-1] != "/repo/a" {
		t.Fatalf("absolute diff: %q, %v", got, err)
	}
}
