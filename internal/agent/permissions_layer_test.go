package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func permsJSON(t *testing.T, p Permissions) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A vault overlay's default policy reaches the engine under the request: Deny holds, the request's allow and mode widen.
func TestRunLayersAgentDefaultsUnderTheRequest(t *testing.T) {
	hermetic(t, "")
	vaulttest.Put(t, vault.Overlay, "agents",
		`{"agents":{"fake":{"permission":{"defaults":{"mode":"plan","ask":["Bash(git push:*)"],"deny":["Bash(rm:*)"]}}}}}`)
	f := &fakeRunner{caps: Caps{Permissions: Native}, errs: []error{nil}}
	req := RunOptions{Prompt: "p", Permissions: mustPerms(t, "auto", []string{"Bash(git push:*)", "Bash(git log:*)"}, nil, nil)}
	if _, err := Run(context.Background(), f, req, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := f.seen[0].Permissions
	want := `{"mode":"auto","allow":[{"Tool":"Bash","Arg":"git push:*"},{"Tool":"Bash","Arg":"git log:*"}],"deny":[{"Tool":"Bash","Arg":"rm:*"}]}`
	if permsJSON(t, got) != want {
		t.Errorf("engine saw %s\nwant %s", permsJSON(t, got), want)
	}
	if d := got.Decide("Bash", "git log && rm -rf x"); d != Deny {
		t.Errorf("the default Deny must hold against a compound command, got %v", d)
	}
}

// An unparseable default rule refuses the dispatch instead of running without the default policy.
func TestRunRefusesAnUnreadableDefault(t *testing.T) {
	hermetic(t, "")
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"fake":{"permission":{"defaults":{"deny":["Bash("]}}}}}`)
	f := &fakeRunner{caps: Caps{Permissions: Native}, errs: []error{nil}}
	_, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "fake default permissions") || len(f.seen) != 0 {
		t.Fatalf("err %v, runs %d; want a refusal naming the default", err, len(f.seen))
	}
}

// Layers add up; widened lists only what a later layer grants beyond the earlier ones, never a denied rule.
func TestResolvePermissionsWidening(t *testing.T) {
	def := mustPerms(t, "plan", []string{"Read"}, []string{"Bash(git push:*)"}, []string{"Bash(rm:*)"})
	cases := []struct {
		name    string
		req     Permissions
		want    string
		widened string
	}{
		{"no request keeps the default", Permissions{}, permsJSON(t, def), ""},
		{"narrowing is free", mustPerms(t, "plan", nil, []string{"Edit"}, []string{"WebFetch"}),
			`{"mode":"plan","allow":[{"Tool":"Read","Arg":""}],"ask":[{"Tool":"Bash","Arg":"git push:*"},{"Tool":"Edit","Arg":""}],"deny":[{"Tool":"Bash","Arg":"rm:*"},{"Tool":"WebFetch","Arg":""}]}`, ""},
		{"allowing a default ask widens it", mustPerms(t, "", []string{"Bash(git push:*)"}, nil, nil),
			`{"mode":"plan","allow":[{"Tool":"Read","Arg":""},{"Tool":"Bash","Arg":"git push:*"}],"deny":[{"Tool":"Bash","Arg":"rm:*"}]}`, "allow Bash(git push:*)"},
		{"a denied allow is no widening", mustPerms(t, "", []string{"Bash(rm:*)"}, nil, nil),
			`{"mode":"plan","allow":[{"Tool":"Read","Arg":""},{"Tool":"Bash","Arg":"rm:*"}],"ask":[{"Tool":"Bash","Arg":"git push:*"}],"deny":[{"Tool":"Bash","Arg":"rm:*"}]}`, ""},
		{"leaving plan widens the mode", mustPerms(t, "auto", nil, nil, nil),
			`{"mode":"auto","allow":[{"Tool":"Read","Arg":""}],"ask":[{"Tool":"Bash","Arg":"git push:*"}],"deny":[{"Tool":"Bash","Arg":"rm:*"}]}`, "mode auto"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, widened := ResolvePermissions(def, c.req)
			if permsJSON(t, got) != c.want || strings.Join(widened, "; ") != c.widened {
				t.Errorf("got %s widened %q\nwant %s widened %q", permsJSON(t, got), widened, c.want, c.widened)
			}
		})
	}
}
