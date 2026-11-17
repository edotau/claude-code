package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
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
func checkBash(t *testing.T, p Permissions, cases map[string]Decision) {
	t.Helper()
	for cmd, want := range cases {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
	}
}

// Given the file-redirect deny, Then only a write to a file is refused: stream merges and a quoted `>` pass.
func TestPermissionFileRedirectDeny(t *testing.T) {
	p := Permissions{Mode: "plan", Deny: []Rule{{"Bash", RedirectFileArg}},
		Allow: []Rule{{"Bash", "go test *"}, {"Bash", "tail *"}, {"Bash", "echo *"}, {"Bash", "cat *"}}}
	checkBash(t, p, map[string]Decision{
		"go test ./x 2>&1 | tail -5": Allow, "go test ./x 2>&-": Allow, "go test ./x >&2": Allow, "go test ./x 1>&2": Allow,
		`echo "a > b"`: Allow, "echo 'x>y'": Allow, `echo a\>b`: Allow,
		"go test ./x > o": Deny, "go test ./x >> o": Deny, "go test ./x >| o": Deny, "go test ./x &> o": Deny,
		"go test ./x 2> o": Deny, "go test ./x >&o": Deny, "go test ./x 2>&1 > o": Deny, "go test ./x >(tee o)": Deny,
		"cat <<EOF > f\nx\nEOF": Deny, "echo $(go test > f)": Deny, "go test ./x 2>&1 >o | tail": Deny,
		"env FOO=1 go test ./x > o": Deny, "time go test ./x >> o": Deny, "env FOO=1 go test ./x 2>&1 | tail": Allow,
		"cat <<< hi": Ask,
	})
}

// Given a cd rule for the workdir, Then only a bare absolute cd into it or below passes.
func TestPermissionCdInsideWorkdir(t *testing.T) {
	p := Permissions{Mode: "plan", Allow: []Rule{CdRule("/work/repo"), {"Bash", "go test *"}}}
	checkBash(t, p, map[string]Decision{
		"cd /work/repo": Allow, "cd /work/repo/internal/x": Allow, "cd /work/repo && go test ./...": Allow,
		`cd "/work/repo/a"`: Allow, "cd /work/repo/": Allow,
		"cd /work": Deny, "cd /work/repo/../other": Deny, "cd /work/repository": Deny, "cd /": Deny, "cd": Deny,
		"cd internal": Deny, "cd ~": Deny, "cd $HOME": Deny, "cd /work/repo/$X": Deny, "cd /work/repo/a*": Deny,
		"cd -P /work/repo": Deny, "cd /work/repo /etc": Deny, "cd /work/repo; cd /etc": Deny, "pushd /work/repo": Deny,
		"cd /work/repo/..": Deny, "cd /work/repo/./..": Deny, "cd /work/repo/a/../..": Deny,
		"cd /work/repo/..x": Allow, "cd /work/repo/a/..": Allow, "cd /work/repo//a": Allow,
	})
	// an empty workdir must allow no cd at all, not every absolute one
	checkBash(t, Permissions{Mode: "plan", Allow: []Rule{CdRule("")}}, map[string]Decision{"cd /": Deny, "cd /etc": Deny})
}

// Given go test allowed, Then a bare env or time prefix is granted by the normalized form; unsafe prefixes are not.
func TestPermissionWrapperNormalizationGrants(t *testing.T) {
	p := Permissions{Mode: "plan", Allow: []Rule{{"Bash", "go test *"}}, Deny: []Rule{{"Bash", "rm:*"}, {"Bash", "FOO=9 *"}}}
	checkBash(t, p, map[string]Decision{
		"FOO=1 go test ./x": Allow, "env FOO=1 go test ./x": Allow, "time go test ./x": Allow, "time FOO=1 go test": Allow,
		"GOOS=linux go test ./x": Allow, "A=1 B=2 go test ./x": Allow,
		"PATH=/tmp go test": Deny, "NODE_OPTIONS=--x go test": Deny, "HTTPS_PROXY=x go test": Deny, "HARNESS_X=1 go test": Deny,
		"OPENAI_API_KEY=x go test": Deny, "GOFLAGS=-toolexec=x go test": Deny, "LD_PRELOAD=x go test": Deny,
		"env -i go test": Deny, "sudo go test": Deny, "/usr/bin/env FOO=1 go test": Deny, "time -p go test": Deny,
		"nohup go test": Deny, "FOO=1 rm x": Deny, "time rm x": Deny, "env FOO=1 rm x": Deny, "FOO=9 go test": Deny,
		"path=/tmp go test": Deny, "Ld_Preload=x go test": Deny,
		"PATH= go test": Deny, "FOO= go test": Allow, "time": Deny, "env": Deny, "FOO=1": Deny, "time FOO=1": Deny,
	})
}

// Given the sed print rule, Then only `sed -n <range>p FILE` passes: no -i, -e, w/e commands or extra options.
func TestPermissionSedPrintOnly(t *testing.T) {
	p := Permissions{Mode: "plan", Allow: []Rule{{"Bash", SedPrintArg}}, Deny: []Rule{{"Bash", RedirectFileArg}}}
	checkBash(t, p, map[string]Decision{
		"sed -n 5p f.go": Allow, "sed -n '10,20p' a b": Allow, "sed -n '$p' f": Allow, "sed -n 3,$p f": Allow,
		"sed -i 5p f": Deny, "sed -n -i 5p f": Deny, "sed -n 5p -i f": Deny, "sed -n 'w out' f": Deny,
		"sed -n '5p;w out' f": Deny, "sed -n '1e id' f": Deny, "sed -n 5p": Deny, "sed -n -e 5p f": Deny,
		"sed 5p f": Deny, "sed -n 5w f": Deny, "sed -n 5p f > o": Deny, "sed -n 5p -- f": Deny,
	})
}

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
			got, widened := resolvePermissions(def, c.req)
			if permsJSON(t, got) != c.want || strings.Join(widened, "; ") != c.widened {
				t.Errorf("got %s widened %q\nwant %s widened %q", permsJSON(t, got), widened, c.want, c.widened)
			}
		})
	}
}

// Given a Deny Write rule spelled through a symlinked root, Then the other spelling is denied too, both ways.
func TestPermissionPathRuleMatchesEitherSymlinkSpelling(t *testing.T) {
	hosttest.SkipOnWindows(t, "symlinks")
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, c := range []struct{ rule, path string }{{link, real}, {real, link}} {
		p := mustPerms(t, "default", nil, nil, []string{"Write(" + c.rule + "/**)"})
		if got := p.Decide("Write", c.path+"/f.txt"); got != Deny {
			t.Errorf("rule under %s, path under %s: got %v, want Deny", c.rule, c.path, got)
		}
		if got := p.Decide("Write", t.TempDir()+"/f.txt"); got == Deny {
			t.Errorf("rule under %s denied an unrelated path", c.rule)
		}
	}
}

func TestPermissionHomePaths(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/permission-home")
	for _, c := range []struct{ rule, target string }{
		{"~", "/permission-home"}, {"~/x", "/permission-home/x"},
		{"~" + u.Username + "/x", filepath.Join(u.HomeDir, "x")},
		{"~claude-code-no-such-user/x", "~claude-code-no-such-user/x"},
	} {
		for _, want := range []Decision{Allow, Deny, Ask} {
			sets := make([][]string, 3)
			sets[want] = []string{"Read(" + c.rule + ")"}
			p := mustPerms(t, "plan", sets[Allow], sets[Ask], sets[Deny])
			if want != Allow {
				p.Allow = []Rule{{Tool: "Read"}}
			}
			if got := p.Decide("Read", c.target); got != want {
				t.Errorf("rule %q target %q: got %v, want %v", c.rule, c.target, got, want)
			}
			if got := p.Decide("Read", "/unrelated/x"); got == want {
				t.Errorf("rule %q widened to unrelated path", c.rule)
			}
			if want == Deny && c.rule != "~/x" && p.Decide("Read", c.rule) != Deny {
				t.Errorf("deny %q lost its literal match", c.rule)
			}
		}
	}
}

// gatedPerms is a gate-shaped plan policy: interpreters and go allowed, rm/git push and file redirects denied.
func gatedPerms(t *testing.T, extraAllow ...string) Permissions {
	t.Helper()
	allow := append([]string{"Bash(python3 *)", "Bash(go run *)", "Bash(go test *)", "Bash(cat *)", "Bash(git commit *)"}, extraAllow...)
	return mustPerms(t, "plan", allow, nil, []string{"Bash(rm:*)", "Bash(git push:*)", "Bash(" + RedirectFileArg + ")"})
}

// Given a heredoc fed to a data consumer, Then its body is stdin, not commands; a shell-fed or expanded body still is.
func TestPermissionHeredocBodyIsData(t *testing.T) {
	commit := "git commit -m \"$(cat <<'EOF'\nfix: x\n\nrm the old cache; git push later\nEOF\n)\""
	checkBash(t, gatedPerms(t), map[string]Decision{
		"python3 - <<'EOF'\nimport os\nrm = 1\nEOF":           Ask,
		"go run ./cmd/x <<'EOF'\nrm -rf build\ngit push\nEOF": Ask,
		"python3 - <<EOF\nprint(1)\n# rm x\nEOF":              Ask,
		"python3 - <<-\"EOF\"\n\trm x\n\tEOF":                 Ask,
		commit:                                                Ask,
		commit + " && go test ./x":                            Ask,
		// controls: a body a shell runs, an expanded body, or code outside the body is still judged
		"bash <<'EOF'\nrm -rf x\nEOF":                Deny,
		"sh -s <<'EOF'\nrm x\nEOF":                   Deny,
		"cat <<'EOF' | sh\nrm x\nEOF":                Deny,
		"python3 - <<EOF\n$(rm x)\nEOF":              Deny,
		"python3 - <<'EOF'\nprint(1)\nEOF\nrm x":     Deny,
		"eval \"$(cat <<'EOF'\nrm x\nEOF\n)\"":       Deny,
		"bash -c \"$(\ncat <<'EOF'\nrm x\nEOF\n)\"":  Deny,
		"echo \"<<EOF\"\nrm x\nEOF":                  Deny,
		"python3 - <<'EOF'\nrm x\n":                  Deny,
		"$SHELL <<'EOF'\nrm x\nEOF":                  Deny,
		"ssh host <<'EOF'\nrm x\nEOF":                Deny,
		"cat <<'EOF' | xargs rm\na\nEOF":             Deny,
		"cat > run.sh <<'EOF'\nrm x\nEOF":            Deny,
		"cat <<'EOF' >> x.sh\nrm x\nEOF\n. x.sh":     Deny,
		"python3 - <<'EOF' && ./run\nrm x\nEOF":      Deny,
		"python3 - <<'EOF' | sh\nprint('rm x')\nEOF": Deny,
	})
	// a deny-only policy: the body no longer denies, the heredoc head still asks
	codex := mustPerms(t, "default", nil, nil, []string{"Bash(rm:*)"})
	checkBash(t, codex, map[string]Decision{commit: Ask, "cat <<'EOF'\nrm x\nEOF": Ask, "bash <<'EOF'\nrm x\nEOF": Deny,
		"cat <<'EOF' > f\nrm x\nEOF": Deny, "rm x <<'EOF'\nok\nEOF": Deny})
}

func bigHeredoc(lines int) string {
	return "python3 - <<'EOF'\n" + strings.Repeat("print('a line of the heredoc body')\n", lines) + "EOF"
}

// Given a 5,000-line heredoc, Then scanning its body does not copy the rest of the command once per line.
func TestShellCodeHeredocLinearAllocs(t *testing.T) {
	cmd := bigHeredoc(5000)
	if got := testing.AllocsPerRun(5, func() { shellCode(cmd) }); got > 100 {
		t.Fatalf("shellCode allocs = %v, want <= 100", got)
	}
}

func BenchmarkShellCodeHeredoc(b *testing.B) {
	cmd := bigHeredoc(5000)
	for b.Loop() {
		shellCode(cmd)
	}
}

// Given shell comments, Then the commented text is not a command; quoted, escaped or mid-word `#` is no comment.
func TestPermissionShellComments(t *testing.T) {
	checkBash(t, gatedPerms(t), map[string]Decision{
		"# run the package tests\ngo test ./internal/x": Allow,
		"go test ./x # then rm the cache; ok":           Allow,
		"  # indented\ngo test ./x\n# trailing":         Allow,
		"go test ./x # rm `x` $(rm y)":                  Allow,
		"go test ./x;# rm y":                            Allow,
		// controls
		"# c\nrm x":                           Deny,
		"go test ./x; rm y # z":               Deny,
		"go test ./x '#'; rm y":               Deny,
		"go test ./x \"a #\"; rm y":           Deny,
		"go test a#b; rm y":                   Deny,
		"go test ${x:- #}; rm y":              Deny,
		"go test \"$(echo \"x #y\"); rm z)\"": Deny,
		"go test $(# c\nrm x)":                Deny,
		"go test ./x # c\nrm y":               Deny,
		"go test ./x \\# c; rm y":             Deny,
		"go test ./x\\\n#b; rm y":             Deny, // the continuation joins `#` to the word
		"go test $#; rm y":                    Deny,
		"go test ./x > o # c":                 Deny,
		// a quote or backslash inside a comment hides nothing: bash runs the next line
		"go test ./x # it's\nrm -rf y #'": Deny,
		"go test ./x #c\\\nrm y":          Deny,
		"go test ./x # \"q\nrm y #\"":     Deny,
	})
	// the same shapes hiding an allowed command stay allowed
	rmOK := mustPerms(t, "plan", []string{"Bash(go test *)", "Bash(rm *)"}, nil, nil)
	checkBash(t, rmOK, map[string]Decision{"go test ./x # it's\nrm -rf y #'": Allow, "go test ./x #c\\\nrm y": Allow})
	codex := mustPerms(t, "default", nil, nil, []string{"Bash(rm:*)"})
	checkBash(t, codex, map[string]Decision{"go test ./x # it's\nrm -rf y #'": Deny, "go test ./x #c\\\nrm y": Deny,
		"cat <<EOF\n# it's\nEOF\nrm y #'": Deny})
}

// Given a cd rule for a workdir, Then a cd naming it through a symlink (/tmp for /private/tmp) passes either way.
func TestPermissionCdCanonicalSpelling(t *testing.T) {
	hosttest.SkipOnWindows(t, "symlinks")
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	link := filepath.Join(other, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	slash := filepath.ToSlash
	for _, c := range []struct{ rule, spelled string }{{link, real}, {real, link}} {
		p := Permissions{Mode: "plan", Allow: []Rule{CdRule(slash(c.rule)), {"Bash", "go test *"}}}
		checkBash(t, p, map[string]Decision{
			"cd " + slash(c.spelled):                         Allow,
			"cd " + slash(c.spelled) + "/sub && go test ./x": Allow,
			"cd " + slash(c.rule):                            Allow,
			// controls
			"cd " + slash(filepath.Dir(real)):   Deny,
			"cd " + slash(other):                Deny,
			"cd " + slash(c.spelled) + "/..":    Deny,
			"cd " + slash(c.spelled) + "x":      Deny,
			"cd " + slash(c.spelled) + "/$X":    Deny,
			"cd " + slash(c.spelled) + "; rm x": Deny,
			"cd sub":                            Deny,
			"cd /":                              Deny,
			"cd " + slash(c.spelled) + " /etc":  Deny,
		})
	}
}

// checkBashAt is checkBash with the hook's cwd: each row is {cwd, command} to verdict.
func checkBashAt(t *testing.T, p Permissions, cases map[[2]string]Decision) {
	t.Helper()
	for c, want := range cases {
		if got := p.DecideAt(c[0], "Bash", c[1]); got != want {
			t.Errorf("cwd %q %q: got %v, want %v", c[0], c[1], got, want)
		}
	}
}

// Given a cd rule and the hook cwd, Then a relative cd is judged as its absolute target, chains left to right.
func TestPermissionCdRelativeToCwd(t *testing.T) {
	p := mustPerms(t, "plan", []string{"Bash(cd:/work/repo)", "Bash(go test *)"}, nil, []string{"Bash(cd:/work/repo/secret)"})
	const repo = "/work/repo"
	checkBashAt(t, p, map[[2]string]Decision{
		{repo, "cd sub"}: Allow, {repo, "cd sub/deeper"}: Allow, {repo, "cd ./sub"}: Allow, {repo, "cd sub/.."}: Allow,
		{repo, "cd ."}: Allow, {repo, "cd sub && go test ./x"}: Allow, {repo, "cd a && cd b"}: Allow,
		{repo, `cd "sub dir"`}: Allow, {repo + "/a", "cd .."}: Allow, {"/elsewhere", "cd /work/repo && cd sub"}: Allow,
		{"/work", "cd repo"}: Allow, {"/work", "cd repo && cd internal"}: Allow, {repo, "cd sub && cd .."}: Allow,
		{repo + "/", "cd sub"}: Allow, {repo, "cd 'sub'"}: Allow,
		// controls: escapes, denied targets, dynamic targets, broken chains, no usable cwd
		{repo, "cd .."}: Deny, {repo, "cd ../other"}: Deny, {repo, "cd sub/../.."}: Deny, {repo, "cd ../repository"}: Deny,
		{repo, "cd secret"}: Deny, {repo, "cd ./secret/x"}: Deny, {repo, "cd a/../secret"}: Deny,
		{repo, "cd sub && cd ../secret"}: Deny, {repo, "cd sub && cd ../.."}: Deny,
		{repo, `cd "$X"`}: Deny, {repo, "cd -"}: Deny, {repo, "cd $(pwd)"}: Deny, {repo, "cd `pwd`"}: Deny,
		{repo, "cd sub$(pwd)"}: Deny, {repo, "cd ~"}: Deny, {repo, "cd sub*"}: Deny, {repo, "cd"}: Deny,
		{repo, "cd -P sub"}: Deny, {repo, "env cd sub"}: Deny, {repo, "cd sub; cd b"}: Deny, {repo, "cd x || cd b"}: Deny,
		{repo, `bash -c "cd sub"`}: Deny, {"repo", "cd sub"}: Deny, {"", "cd sub"}: Deny, {"/work", "cd repo/.."}: Deny,
	})
	// outside plan mode a wildcard allow must not let a relative spelling reach a denied directory
	open := mustPerms(t, "default", []string{"Bash(cd *)"}, nil, []string{"Bash(cd:/work/repo/secret)"})
	checkBashAt(t, open, map[[2]string]Decision{{repo, "cd secret"}: Deny, {repo, "cd sub/../secret/x"}: Deny,
		{repo, "cd sub"}: Allow, {"/work/repo/secret", "cd ."}: Deny})
	// a Read deny binds a relative cd exactly as the absolute one
	ro := Permissions{Mode: "plan", Allow: []Rule{ReadOnlyAllow, CdRule(repo)}, Deny: []Rule{{"Read", repo + "/secret/**"}}}
	checkBashAt(t, ro, map[[2]string]Decision{{repo, "cd sub"}: Allow, {repo, "cd secret"}: Deny, {repo, "cd .."}: Deny})
	if got := ro.Decide("Bash", "cd /work/repo/secret"); got != Deny {
		t.Errorf("absolute spelling under the Read deny: got %v, want Deny", got)
	}
}

// Given symlinked dirs, Then a relative cd through a link lands where the absolute spelling lands, deny included.
func TestPermissionCdRelativeSymlink(t *testing.T) {
	hosttest.SkipOnWindows(t, "symlinks")
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret, other := t.TempDir(), t.TempDir()
	if err := errors.Join(os.Symlink(real, filepath.Join(other, "link")),
		os.Symlink(secret, filepath.Join(real, "esc"))); err != nil {
		t.Skipf("symlink: %v", err)
	}
	slash := filepath.ToSlash
	p := Permissions{Mode: "plan", Allow: []Rule{CdRule(slash(real))},
		Deny: []Rule{{"Bash", cdPrefix + slash(secret)}}}
	checkBashAt(t, p, map[[2]string]Decision{
		{slash(other), "cd link"}: Allow, {slash(other), "cd link/sub"}: Allow, {slash(other), "cd link && cd sub"}: Allow,
		{slash(real), "cd esc"}: Deny, {slash(real), "cd esc/x"}: Deny, {slash(other), "cd link/esc"}: Deny,
		{slash(other), "cd link && cd esc"}: Deny,
	})
	if got := p.Decide("Bash", "cd "+slash(real)+"/esc"); got != Deny {
		t.Errorf("absolute spelling through the link: got %v, want Deny", got)
	}
}

func BenchmarkShellCode(b *testing.B) {
	var c, h strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&c, "echo step %d > /tmp/out_%d.txt # note %d about the step's output\n", i, i, i)
	}
	h.WriteString("cat > /tmp/notes/docs/notes.md <<EOF\n")
	for i := range 5000 {
		if i%10 == 0 {
			fmt.Fprintf(&h, "## Section %d (part %d)\n", i, i/10)
		} else {
			fmt.Fprintf(&h, "Line %d of the doc: it's plain text with a #tag and notes.\n", i)
		}
	}
	h.WriteString("EOF\n")
	for name, cmd := range map[string]string{"comments": c.String(), "heredoc": h.String()} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				shellCode(cmd)
			}
		})
	}
}
