package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func mustPerms(t *testing.T, mode string, allow, ask, deny []string) Permissions {
	t.Helper()
	p, err := ParsePermissions(mode, allow, ask, deny)
	if err != nil {
		t.Fatalf("ParsePermissions: %v", err)
	}
	return p
}

// Given malformed rule strings, Then ParseRule errors; valid ones split into Tool and Arg.
func TestPermissionParseRule(t *testing.T) {
	for _, bad := range []string{"", "  ", "Bash(", "Bash(git", "Bash)", "(x)", "Bash()", "Bash(  )"} {
		if r, err := ParseRule(bad); err == nil {
			t.Errorf("ParseRule(%q) = %+v, want error", bad, r)
		}
	}
	for in, want := range map[string]Rule{"Read": {Tool: "Read"}, "Bash(git log:*)": {"Bash", "git log:*"}} {
		if got, err := ParseRule(in); err != nil || got != want {
			t.Errorf("ParseRule(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
}

// Given an unknown mode or a bad rule, Then ParsePermissions fails.
func TestPermissionParsePermissionsErrors(t *testing.T) {
	if _, err := ParsePermissions("yolo", nil, nil, nil); err == nil {
		t.Error("unknown mode must error")
	}
	if _, err := ParsePermissions("auto", []string{"Bash()"}, nil, nil); err == nil {
		t.Error("bad rule must error")
	}
}

func TestPermissionEach(t *testing.T) {
	p := Permissions{
		Allow: []Rule{{Tool: "Read", Arg: "allowed.txt"}},
		Ask:   []Rule{{Tool: "Bash", Arg: "git push:*"}},
		Deny:  []Rule{{Tool: "Edit", Arg: ".env"}},
	}
	type visit struct {
		decision Decision
		rule     Rule
	}
	var got []visit
	p.Each(func(decision Decision, r Rule) {
		got = append(got, visit{decision, r})
	})
	want := []visit{
		{Deny, Rule{Tool: "Edit", Arg: ".env"}},
		{Ask, Rule{Tool: "Bash", Arg: "git push:*"}},
		{Allow, Rule{Tool: "Read", Arg: "allowed.txt"}},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Each visits = %+v, want %+v", got, want)
	}
	(Permissions{}).Each(func(decision Decision, r Rule) {
		t.Errorf("empty Permissions visited decision %v, rule %+v", decision, r)
	})
}

// Given the same call in Allow, Ask and Deny, Then Deny wins, then Ask, then Allow.
func TestPermissionPrecedence(t *testing.T) {
	r := []string{"Read(a.txt)"}
	cases := []struct {
		name            string
		allow, ask, del []string
		want            Decision
	}{
		{"all three", r, r, r, Deny},
		{"ask+allow", r, r, nil, Ask},
		{"allow", r, nil, nil, Allow},
	}
	for _, c := range cases {
		if got := mustPerms(t, "auto", c.allow, c.ask, c.del).Decide("Read", "a.txt"); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Given an unset Decision or no matching rule, Then it is never Allow; plan denies, other modes ask.
func TestPermissionFailClosedAndModeFallback(t *testing.T) {
	var zero Decision
	if zero == Allow {
		t.Fatal("zero Decision must not be Allow")
	}
	for mode, want := range map[string]Decision{"": Ask, "default": Ask, "auto": Ask, "plan": Deny} {
		if got := mustPerms(t, mode, []string{"Read(x)"}, nil, nil).Decide("Read", "y"); got != want {
			t.Errorf("mode %q unmatched: got %v, want %v", mode, got, want)
		}
	}
}

// Given Bash(git log:*), Then it matches at a word boundary only.
func TestPermissionBashWordBoundary(t *testing.T) {
	p := mustPerms(t, "auto", []string{"Bash(git log:*)", "Bash(ls)"}, nil, nil)
	cases := map[string]Decision{"git log": Allow, "git log -1": Allow, "git logx": Ask, "git logx -1": Ask, "ls": Allow, "ls -l": Ask}
	for cmd, want := range cases {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
	}
}

// Given compound, wrapped and substituted commands with rm denied, Then the strictest part wins.
func TestPermissionBashSegments(t *testing.T) {
	p := mustPerms(t, "auto", []string{"Bash(git log:*)", "Bash(ls:*)"}, nil, []string{"Bash(rm:*)"})
	cases := map[string]Decision{
		"git log; rm -rf x": Deny, "ls && rm x": Deny, "(rm -rf x)": Deny, "FOO=1 rm x": Deny,
		"sudo rm x": Deny, `bash -c "rm x"`: Deny, "echo $(rm x)": Deny,
		"git log > /etc/x": Ask, "git logx": Ask, "ls | grep a": Ask,
		"git log -1": Allow, "ls && git log": Allow,
	}
	for cmd, want := range cases {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
	}
}

// Given path globs, Then ** crosses directories, * stays in one, and a leading ./ is ignored.
func TestPermissionPathGlobs(t *testing.T) {
	p := mustPerms(t, "auto", []string{"Edit(internal/**)", "Read(src/*.go)"}, nil, nil)
	cases := []struct {
		tool, arg string
		want      Decision
	}{
		{"Edit", "internal/a/b/c.go", Allow}, {"Edit", "./internal/a.go", Allow}, {"Edit", "other/a.go", Ask},
		{"Read", "src/a.go", Allow}, {"Read", "./src/a.go", Allow}, {"Read", "src/sub/a.go", Ask},
	}
	for _, c := range cases {
		if got := p.Decide(c.tool, c.arg); got != c.want {
			t.Errorf("%s(%s): got %v, want %v", c.tool, c.arg, got, c.want)
		}
	}
}

// Given WebFetch(domain:example.com), Then the host and its subdomains match, case-insensitively, not look-alikes.
func TestPermissionWebFetchDomain(t *testing.T) {
	p := mustPerms(t, "auto", []string{"WebFetch(domain:example.com)"}, nil, nil)
	cases := map[string]Decision{
		"example.com": Allow, "https://API.Example.com/x": Allow, "https://notexample.com": Ask, "https://example.com.evil.io": Ask,
	}
	for u, want := range cases {
		if got := p.Decide("WebFetch", u); got != want {
			t.Errorf("%q: got %v, want %v", u, got, want)
		}
	}
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
	if out.Permissions.Mode != "plan" || len(out.Permissions.Allow) != 1 || out.Permissions.Ask[0] != (Rule{"Bash", "git push:*"}) || out.Permissions.Deny[0].Arg != ".env*" {
		t.Errorf("round-trip lost data: %s -> %+v", b, out.Permissions)
	}
}

// Given Claude Code's Bash wildcard rules, Then "*" spans text and a trailing " *" keeps the word boundary.
func TestPermissionBashWildcard(t *testing.T) {
	p, err := ParsePermissions("", nil, nil, []string{"Bash(rm *)", "Bash(git * --force)"})
	if err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]Decision{
		"rm -rf x": Deny, "rm": Deny, "rmdir x": Ask, "git push origin --force": Deny, "git push": Ask,
	} {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("Decide(Bash, %q) = %v, want %v", cmd, got, want)
		}
	}
}

// A command that is only a substitution keeps its Deny under a blanket Bash allow (a Deny is never widened).
func TestDecideBareSubstitutionKeepsDeny(t *testing.T) {
	p := Permissions{Allow: []Rule{{Tool: "Bash"}}, Deny: []Rule{{Tool: "Bash", Arg: "curl:*"}}}
	for _, cmd := range []string{"$(curl x)", "`curl x`", "echo $(curl x)", "curl x"} {
		if got := p.Decide("Bash", cmd); got != Deny {
			t.Errorf("Decide(Bash, %q) = %v, want Deny", cmd, got)
		}
	}
}

// TestSafeModeDenyFindsFirstUnenforceable: only under --safe-mode, and only a Deny rule the predicate flags.
func TestSafeModeDenyFindsFirstUnenforceable(t *testing.T) {
	bash := func(r Rule) bool { return r.Tool == "Bash" }
	perms := Permissions{Allow: []Rule{{Tool: "Bash"}}, Deny: []Rule{{Tool: "Read"}, {Tool: "Bash", Arg: "rm:*"}}}
	if r, ok := SafeModeDeny(RunOptions{SafeMode: true, Permissions: perms}, bash); !ok || r.String() != "Bash(rm:*)" {
		t.Errorf("safe mode = %v %v, want Bash(rm:*)", r, ok)
	}
	if _, ok := SafeModeDeny(RunOptions{Permissions: perms}, bash); ok {
		t.Error("outside safe mode a Deny must not be refused")
	}
	if _, ok := SafeModeDeny(RunOptions{SafeMode: true, Permissions: Permissions{Allow: perms.Allow}}, bash); ok {
		t.Error("an Allow rule must not count as an unenforceable Deny")
	}
}

func TestPermissionUnicodePathDeny(t *testing.T) {
	for _, path := range []string{"café.txt", "資料.txt", "🔒.txt"} {
		p := mustPerms(t, "auto", []string{"Read"}, nil, []string{"Read(" + path + ")"})
		if got := p.Decide("Read", path); got != Deny {
			t.Errorf("Read(%q) = %v, want Deny", path, got)
		}
		if got := p.Decide("Read", "public.txt"); got != Allow {
			t.Errorf("Read(public.txt) = %v, want Allow", got)
		}
	}
}

func permsJSON(t *testing.T, p Permissions) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
