package permission

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/hosttest"
)

func mustPerms(t *testing.T, mode string, allow, ask, deny []string) Permissions {
	t.Helper()
	p := Permissions{Mode: mode}
	for _, set := range []struct {
		in  []string
		out *[]Rule
	}{{allow, &p.Allow}, {ask, &p.Ask}, {deny, &p.Deny}} {
		for _, in := range set.in {
			r, err := ParseRule(in)
			if err != nil {
				t.Fatalf("ParseRule(%q): %v", in, err)
			}
			*set.out = append(*set.out, r)
		}
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
		if got := mustPerms(t, "auto", c.allow, c.ask, c.del).DecideAt("", "Read", "a.txt"); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Given an unset Decision or no matching rule, Then it is never Allow; allow allows, other modes ask.
func TestPermissionFailClosedAndModeFallback(t *testing.T) {
	var zero Decision
	if zero == Allow {
		t.Fatal("zero Decision must not be Allow")
	}
	for mode, want := range map[string]Decision{"": Ask, "default": Ask, PermAllow: Allow} {
		if got := mustPerms(t, mode, []string{"Read(x)"}, nil, nil).DecideAt("", "Read", "y"); got != want {
			t.Errorf("mode %q unmatched: got %v, want %v", mode, got, want)
		}
	}
}

// Given Bash(git log:*), Then it matches at a word boundary only.
func checkBash(t *testing.T, p Permissions, cases map[string]Decision) {
	t.Helper()
	for cmd, want := range cases {
		if got := p.DecideAt("", "Bash", cmd); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
	}
}

// Given the file-redirect deny, Then only a write to a file is refused: stream merges and a quoted `>` pass.
func TestPermissionFileRedirectDeny(t *testing.T) {
	p := Permissions{Deny: []Rule{{"Bash", RedirectFileArg}},
		Allow: []Rule{{"Bash", "go test *"}, {"Bash", "tail *"}, {"Bash", "echo *"}, {"Bash", "cat *"}}}
	checkBash(t, p, map[string]Decision{
		"go test ./x 2>&1 | tail -5": Allow, "go test ./x 2>&-": Allow, "go test ./x >&2": Allow, "go test ./x 1>&2": Allow,
		`echo "a > b"`: Allow, "echo 'x>y'": Allow, `echo a\>b`: Allow,
		"go test ./x > o": Deny, "go test ./x >> o": Deny, "go test ./x >| o": Deny, "go test ./x &> o": Deny,
		"go test ./x 2> o": Deny, "go test ./x >&o": Deny, "go test ./x 2>&1 > o": Deny, "go test ./x >(tee o)": Ask,
		"cat <<EOF > f\nx\nEOF": Deny, "echo $(go test > f)": Deny, "go test ./x 2>&1 >o | tail": Deny,
		"env FOO=1 go test ./x > o": Deny, "time go test ./x >> o": Deny, "env FOO=1 go test ./x 2>&1 | tail": Allow,
		"cat <<< hi": Ask,
	})
}

// Given a cd rule for the workdir, Then only a bare absolute cd into it or below passes.
func TestPermissionCdInsideWorkdir(t *testing.T) {
	p := Permissions{Allow: []Rule{cdRule("/work/repo"), {"Bash", "go test *"}}}
	checkBash(t, p, map[string]Decision{
		"cd /work/repo": Allow, "cd /work/repo/internal/x": Allow, "cd /work/repo && go test ./...": Allow,
		`cd "/work/repo/a"`: Allow, "cd /work/repo/": Allow,
		"cd /work": Ask, "cd /work/repo/../other": Ask, "cd /work/repository": Ask, "cd /": Ask, "cd": Ask,
		"cd internal": Ask, "cd ~": Ask, "cd $HOME": Ask, "cd /work/repo/$X": Ask, "cd /work/repo/a*": Ask,
		"cd -P /work/repo": Ask, "cd /work/repo /etc": Ask, "cd /work/repo; cd /etc": Ask, "pushd /work/repo": Ask,
		"cd /work/repo/..": Ask, "cd /work/repo/./..": Ask, "cd /work/repo/a/../..": Ask,
		"cd /work/repo/..x": Allow, "cd /work/repo/a/..": Allow, "cd /work/repo//a": Allow,
	})
	// an empty workdir must allow no cd at all, not every absolute one
	checkBash(t, Permissions{Allow: []Rule{cdRule("")}}, map[string]Decision{"cd /": Ask, "cd /etc": Ask})
	// a home-normalized store path still matches its cd rule (commandWords spells $HOME as ~)
	home, _ := os.UserHomeDir()
	store := Permissions{Allow: []Rule{cdRule(filepath.ToSlash(home) + "/.claude/memory")}}
	checkBash(t, store, map[string]Decision{"cd ~/.claude/memory": Allow, "cd ~/.claude/memory/x": Allow, "cd ~/.claude": Ask, "cd ~": Ask})
}

// Given a Bash deny, Then rtk, path-named and $HOME spellings meet it; `rtk read` stays unstripped.
func TestPermissionCommandWordsNormalize(t *testing.T) {
	p := Permissions{Allow: []Rule{{"Bash", "*"}}, Deny: []Rule{{"Bash", "rm -rf ~"}}}
	checkBash(t, p, map[string]Decision{
		"rm -rf ~": Deny, "rtk rm -rf ~": Deny, "/bin/rm -rf $HOME": Deny, "rtk rm -rf ${HOME}": Deny,
		"rm -rf /tmp/x": Allow, "rtk read f": Allow,
	})
}

// Given go test allowed, Then a bare env or time prefix is granted by the normalized form; unsafe prefixes are not.
func TestPermissionWrapperNormalizationGrants(t *testing.T) {
	p := Permissions{Allow: []Rule{{"Bash", "go test *"}}, Deny: []Rule{{"Bash", "rm:*"}, {"Bash", "FOO=9 *"}}}
	checkBash(t, p, map[string]Decision{
		"FOO=1 go test ./x": Allow, "env FOO=1 go test ./x": Allow, "time go test ./x": Allow, "time FOO=1 go test": Allow,
		"GOOS=linux go test ./x": Allow, "A=1 B=2 go test ./x": Allow,
		"PATH=/tmp go test": Ask, "NODE_OPTIONS=--x go test": Ask, "HTTPS_PROXY=x go test": Ask, "HARNESS_X=1 go test": Ask,
		"OPENAI_API_KEY=x go test": Ask, "GOFLAGS=-toolexec=x go test": Ask, "LD_PRELOAD=x go test": Ask,
		"env -i go test": Ask, "sudo go test": Ask, "/usr/bin/env FOO=1 go test": Ask, "time -p go test": Ask,
		"nohup go test": Ask, "FOO=1 rm x": Deny, "time rm x": Deny, "env FOO=1 rm x": Deny, "FOO=9 go test": Deny,
		"path=/tmp go test": Ask, "Ld_Preload=x go test": Ask,
		"PATH= go test": Ask, "FOO= go test": Allow, "time": Ask, "env": Ask, "FOO=1": Ask, "time FOO=1": Ask,
	})
}

// Given a curl deny, Then the command xargs runs is judged, past its options and their values; a plain grep stays allowed.
func TestPermissionXargsUnwrapped(t *testing.T) {
	p := Permissions{Allow: []Rule{{"Bash", "echo *"}, {"Bash", "xargs *"}, {"Bash", "grep *"}}, Deny: []Rule{{"Bash", "curl *"}}}
	checkBash(t, p, map[string]Decision{
		"echo u | xargs curl x": Deny, "echo u | xargs -n 1 curl x": Deny, "echo u | xargs -I {} curl {}": Deny,
		"echo u | xargs --max-args 1 curl x": Deny, "echo u | xargs -P4 -0 curl x": Deny, "echo u | xargs nice curl x": Deny,
		"echo u | xargs -- curl x": Deny, "echo u | xargs grep x": Allow, "echo u | xargs -n 1 grep curl": Allow,
		"echo u | xargs -rn 1 curl x": Deny, "echo u | xargs -rn 1 grep needle": Allow, "echo u | xargs -rn1 curl x": Deny,
		"echo u | xargs --replace curl x": Deny, "echo u | xargs --replace=R curl R": Deny, "echo u | xargs -i curl x": Deny,
		"echo u | xargs --eof curl x": Deny, "echo u | xargs -I{} grep {}": Allow, "echo u | xargs -tI R curl R": Deny,
	})
}

// A segment that only assigns or only prints the environment is judged like any command; an empty one is skipped.
func TestPermissionWordlessSegmentJudged(t *testing.T) {
	p := Permissions{Allow: []Rule{{"Bash", "grep *"}, {"Bash", "wc *"}}}
	checkBash(t, p, map[string]Decision{
		"env | grep TOKEN": Ask, "env | wc -l": Ask, "x=1; grep a f": Ask, "nohup env | grep x": Ask, "exec env | wc -l": Ask,
		"grep a f;": Allow, "grep a f\n\ngrep b f": Allow, "grep a f; ; wc -l f": Allow,
	})
}

// Given the sed print rule, Then only `sed -n <range>p FILE` passes: no -i, -e, w/e commands or extra options.
func TestPermissionSedPrintOnly(t *testing.T) {
	p := Permissions{Allow: []Rule{{"Bash", SedPrintArg}}, Deny: []Rule{{"Bash", RedirectFileArg}}}
	checkBash(t, p, map[string]Decision{
		"sed -n 5p f.go": Allow, "sed -n '10,20p' a b": Allow, "sed -n '$p' f": Allow, "sed -n 3,$p f": Allow,
		"sed -i 5p f": Ask, "sed -n -i 5p f": Ask, "sed -n 5p -i f": Ask, "sed -n 'w out' f": Ask,
		"sed -n '5p;w out' f": Ask, "sed -n '1e id' f": Ask, "sed -n 5p": Ask, "sed -n -e 5p f": Ask,
		"sed 5p f": Ask, "sed -n 5w f": Ask, "sed -n 5p f > o": Deny, "sed -n 5p -- f": Ask,
	})
}

func TestPermissionBashWordBoundary(t *testing.T) {
	p := mustPerms(t, "auto", []string{"Bash(git log:*)", "Bash(ls)"}, nil, nil)
	cases := map[string]Decision{"git log": Allow, "git log -1": Allow, "git logx": Ask, "git logx -1": Ask, "ls": Allow, "ls -l": Ask}
	for cmd, want := range cases {
		if got := p.DecideAt("", "Bash", cmd); got != want {
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
		if got := p.DecideAt("", "Bash", cmd); got != want {
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
		if got := p.DecideAt("", c.tool, c.arg); got != c.want {
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
		if got := p.DecideAt("", "WebFetch", u); got != want {
			t.Errorf("%q: got %v, want %v", u, got, want)
		}
	}
}

// Given Claude Code's Bash wildcard rules, Then "*" spans text and a trailing " *" keeps the word boundary.
func TestPermissionBashWildcard(t *testing.T) {
	p := mustPerms(t, "", nil, nil, []string{"Bash(rm *)", "Bash(git * --force)"})
	for cmd, want := range map[string]Decision{
		"rm -rf x": Deny, "rm": Deny, "rmdir x": Ask, "git push origin --force": Deny, "git push": Ask,
	} {
		if got := p.DecideAt("", "Bash", cmd); got != want {
			t.Errorf("Decide(Bash, %q) = %v, want %v", cmd, got, want)
		}
	}
}

// A command that is only a substitution keeps its Deny under a blanket Bash allow (a Deny is never widened).
func TestDecideBareSubstitutionKeepsDeny(t *testing.T) {
	p := Permissions{Allow: []Rule{{Tool: "Bash"}}, Deny: []Rule{{Tool: "Bash", Arg: "curl:*"}}}
	for _, cmd := range []string{"$(curl x)", "`curl x`", "echo $(curl x)", "curl x"} {
		if got := p.DecideAt("", "Bash", cmd); got != Deny {
			t.Errorf("Decide(Bash, %q) = %v, want Deny", cmd, got)
		}
	}
}

func TestPermissionUnicodePathDeny(t *testing.T) {
	for _, path := range []string{"café.txt", "資料.txt", "🔒.txt"} {
		p := mustPerms(t, "auto", []string{"Read"}, nil, []string{"Read(" + path + ")"})
		if got := p.DecideAt("", "Read", path); got != Deny {
			t.Errorf("Read(%q) = %v, want Deny", path, got)
		}
		if got := p.DecideAt("", "Read", "public.txt"); got != Allow {
			t.Errorf("Read(public.txt) = %v, want Allow", got)
		}
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
		if got := p.DecideAt("", "Write", c.path+"/f.txt"); got != Deny {
			t.Errorf("rule under %s, path under %s: got %v, want Deny", c.rule, c.path, got)
		}
		if got := p.DecideAt("", "Write", t.TempDir()+"/f.txt"); got == Deny {
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
			p := mustPerms(t, "", sets[Allow], sets[Ask], sets[Deny])
			if want != Allow {
				p.Allow = []Rule{{Tool: "Read"}}
			}
			if got := p.DecideAt("", "Read", c.target); got != want {
				t.Errorf("rule %q target %q: got %v, want %v", c.rule, c.target, got, want)
			}
			if got := p.DecideAt("", "Read", "/unrelated/x"); got == want {
				t.Errorf("rule %q widened to unrelated path", c.rule)
			}
			if want == Deny && c.rule != "~/x" && p.DecideAt("", "Read", c.rule) != Deny {
				t.Errorf("deny %q lost its literal match", c.rule)
			}
		}
	}
}

// gatedPerms is a gate-shaped plan policy: interpreters and go allowed, rm/git push and file redirects denied.
func gatedPerms(t *testing.T, extraAllow ...string) Permissions {
	t.Helper()
	allow := append([]string{"Bash(python3 *)", "Bash(go run *)", "Bash(go test *)", "Bash(cat *)", "Bash(git commit *)"}, extraAllow...)
	return mustPerms(t, "", allow, nil, []string{"Bash(rm:*)", "Bash(git push:*)", "Bash(" + RedirectFileArg + ")"})
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
		"python3 - <<'EOF' | sh\nprint('rm x')\nEOF": Ask,
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
		"go test \"$(echo \"x #y\"); rm z)\"": Ask,
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
	rmOK := mustPerms(t, "", []string{"Bash(go test *)", "Bash(rm *)"}, nil, nil)
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
		p := Permissions{Allow: []Rule{cdRule(slash(c.rule)), {"Bash", "go test *"}}}
		checkBash(t, p, map[string]Decision{
			"cd " + slash(c.spelled):                         Allow,
			"cd " + slash(c.spelled) + "/sub && go test ./x": Allow,
			"cd " + slash(c.rule):                            Allow,
			// controls
			"cd " + slash(filepath.Dir(real)):   Ask,
			"cd " + slash(other):                Ask,
			"cd " + slash(c.spelled) + "/..":    Ask,
			"cd " + slash(c.spelled) + "x":      Ask,
			"cd " + slash(c.spelled) + "/$X":    Ask,
			"cd " + slash(c.spelled) + "; rm x": Ask,
			"cd sub":                            Ask,
			"cd /":                              Ask,
			"cd " + slash(c.spelled) + " /etc":  Ask,
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
	p := mustPerms(t, "", []string{"Bash(cd:/work/repo)", "Bash(go test *)"}, nil, []string{"Bash(cd:/work/repo/secret)"})
	const repo = "/work/repo"
	checkBashAt(t, p, map[[2]string]Decision{
		{repo, "cd sub"}: Allow, {repo, "cd sub/deeper"}: Allow, {repo, "cd ./sub"}: Allow, {repo, "cd sub/.."}: Allow,
		{repo, "cd ."}: Allow, {repo, "cd sub && go test ./x"}: Allow, {repo, "cd a && cd b"}: Allow,
		{repo, `cd "sub dir"`}: Allow, {repo + "/a", "cd .."}: Allow, {"/elsewhere", "cd /work/repo && cd sub"}: Allow,
		{"/work", "cd repo"}: Allow, {"/work", "cd repo && cd internal"}: Allow, {repo, "cd sub && cd .."}: Allow,
		{repo + "/", "cd sub"}: Allow, {repo, "cd 'sub'"}: Allow,
		// controls: escapes, denied targets, dynamic targets, broken chains, no usable cwd
		{repo, "cd .."}: Ask, {repo, "cd ../other"}: Ask, {repo, "cd sub/../.."}: Ask, {repo, "cd ../repository"}: Ask,
		{repo, "cd secret"}: Deny, {repo, "cd ./secret/x"}: Deny, {repo, "cd a/../secret"}: Deny,
		{repo, "cd sub && cd ../secret"}: Deny, {repo, "cd sub && cd ../.."}: Ask,
		{repo, `cd "$X"`}: Ask, {repo, "cd -"}: Ask, {repo, "cd $(pwd)"}: Ask, {repo, "cd `pwd`"}: Ask,
		{repo, "cd sub$(pwd)"}: Ask, {repo, "cd ~"}: Ask, {repo, "cd sub*"}: Ask, {repo, "cd"}: Ask,
		{repo, "cd -P sub"}: Ask, {repo, "env cd sub"}: Ask, {repo, "cd sub; cd b"}: Ask, {repo, "cd x || cd b"}: Ask,
		{repo, `bash -c "cd sub"`}: Ask, {"repo", "cd sub"}: Ask, {"", "cd sub"}: Ask, {"/work", "cd repo/.."}: Ask,
	})
	// outside plan mode a wildcard allow must not let a relative spelling reach a denied directory
	open := mustPerms(t, "default", []string{"Bash(cd *)"}, nil, []string{"Bash(cd:/work/repo/secret)"})
	checkBashAt(t, open, map[[2]string]Decision{{repo, "cd secret"}: Deny, {repo, "cd sub/../secret/x"}: Deny,
		{repo, "cd sub"}: Allow, {"/work/repo/secret", "cd ."}: Deny})
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
	p := Permissions{Allow: []Rule{cdRule(slash(real))},
		Deny: []Rule{{"Bash", cdPrefix + slash(secret)}}}
	checkBashAt(t, p, map[[2]string]Decision{
		{slash(other), "cd link"}: Allow, {slash(other), "cd link/sub"}: Allow, {slash(other), "cd link && cd sub"}: Allow,
		{slash(real), "cd esc"}: Deny, {slash(real), "cd esc/x"}: Deny, {slash(other), "cd link/esc"}: Deny,
		{slash(other), "cd link && cd esc"}: Deny,
	})
	if got := p.DecideAt("", "Bash", "cd "+slash(real)+"/esc"); got != Deny {
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

func cdRule(dir string) Rule { return Rule{Tool: "Bash", Arg: cdPrefix + dir} }
