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
