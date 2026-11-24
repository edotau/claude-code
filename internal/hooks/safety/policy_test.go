package safety

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/settings"
)

func policyTemplateFile(tb testing.TB) {
	tb.Helper()
	tb.Setenv("CLAUDE_CONFIG_DIR", tb.TempDir())
	if err := os.WriteFile(settings.SettingsFile(), settings.Template(), 0o600); err != nil {
		tb.Fatal(err)
	}
}

func TestPolicySizedReadTemplate(t *testing.T) {
	policyTemplateFile(t)
	lists, err := settings.LivePermissions()
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if want := baselineParsePolicy(lists); !reflect.DeepEqual(got, want) {
		t.Fatalf("policy differs from template: got %#v; want %#v", got, want)
	}
}

func TestPolicyReadValidationEquivalent(t *testing.T) {
	policyTemplateFile(t)
	const valid = `{"permissions":{"deny":[]}}`
	for _, body := range []string{
		`null`, `{}`, `{"permissions":null}`, `{"permissions":{}}`,
		`{"permissions":{"deny":null}}`, `{"permissions":"x"}`, `{"permissions":{"deny":"x"}}`,
		`{"permissions":{"deny":[1]}}`, `{"permissions":{"deny":[],"allow":1}}`,
		`{"permissions":{"deny":[],"ask":false}}`, `{bad`, valid + `{}`, valid,
		`{"permissions":{"deny":[],"deny":["Bash","Edit(x)"]}}`,
		`{"permissions":{"deny":["Bash"],"deny":null}}`,
		valid + strings.Repeat(" ", (4<<20)-len(valid)),
		valid + strings.Repeat(" ", (4<<20)+1-len(valid)),
	} {
		if err := os.WriteFile(settings.SettingsFile(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		lists, beforeErr := settings.LivePermissions()
		got, afterErr := readPolicy()
		if (beforeErr == nil) != (afterErr == nil) {
			t.Fatalf("length %d: baseline %v; sized %v", len(body), beforeErr, afterErr)
		}
		if beforeErr != nil {
			if beforeErr.Error() != afterErr.Error() ||
				errors.Is(beforeErr, settings.ErrTooLarge) != errors.Is(afterErr, settings.ErrTooLarge) {
				t.Errorf("baseline %v; sized %v", beforeErr, afterErr)
			}
		} else if want := baselineParsePolicy(lists); !reflect.DeepEqual(got, want) {
			t.Errorf("length %d: got %#v; want %#v", len(body), got, want)
		}
	}
}

func TestPolicyParseEquivalent(t *testing.T) {
	for _, lists := range []settings.PermissionLists{
		{},
		{Allow: []string{"Read", "Edit", "broken("}, Ask: []string{"Write(~/raw)", "Edit( )"}},
		{Deny: []string{"Bash", "Bash(echo *)", "Edit", "Write", "Edit()", "Write( /x )", "Read(x)"}},
	} {
		if got, want := parsePolicy(lists.Deny), baselineParsePolicy(lists); !reflect.DeepEqual(got, want) {
			t.Errorf("%#v: got %#v; want %#v", lists, got, want)
		}
	}
}

// baselineParsePolicy is the deny rows of the pre-3.21 parser, for output and allocation comparisons.
func baselineParsePolicy(l settings.PermissionLists) policy {
	var p policy
	for _, s := range l.Deny {
		if r, err := permission.ParseRule(s); err == nil && r.Tool == "Bash" {
			p.bashDeny = append(p.bashDeny, r)
			continue
		}
		p.editDeny = append(p.editDeny, baselineEditGlobs(s)...)
	}
	return p
}

// baselineEditGlobs preserves the pre-3.21 editGlobs.
func baselineEditGlobs(row string) []string {
	if row == "Edit" || row == "Write" { // a bare tool row covers every path
		return []string{"**/*"}
	}
	for _, tool := range []string{"Edit(", "Write("} {
		g, ok := strings.CutPrefix(row, tool)
		if !ok || !strings.HasSuffix(g, ")") {
			continue
		}
		if g = strings.TrimSpace(strings.TrimSuffix(g, ")")); g != "" {
			return []string{g}
		}
	}
	return nil
}

func BenchmarkPolicyRead(b *testing.B) {
	policyTemplateFile(b)
	for _, tc := range []struct {
		name string
		read func() (policy, error)
	}{
		{"baseline", func() (policy, error) {
			lists, err := settings.LivePermissions()
			return baselineParsePolicy(lists), err
		}},
		{"parsed", func() (policy, error) {
			lists, err := settings.LivePermissions()
			return parsePolicy(lists.Deny), err
		}},
		{"sized", readPolicy},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := tc.read(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestParsePolicyEditRows(t *testing.T) {
	p := parsePolicy([]string{"Edit(.env)", "Write(~/x)", "Edit", "Write", "Edit( )", "Edit()"})
	if want := []string{".env", "~/x", "**/*", "**/*"}; !slices.Equal(p.editDeny, want) {
		t.Errorf("editDeny = %q, want %q", p.editDeny, want)
	}
}

func TestRelativeEditGlobAnchorsAtCwd(t *testing.T) {
	if got := foldedGlobs([]string{".env"}, "/w/app"); len(got) != 1 {
		t.Errorf("foldedGlobs(.env) = %q, want one cwd-anchored glob", got)
	}
	if n := globNeedle(".env", "/home/u/"); n != ".env" {
		t.Errorf("globNeedle(.env) = %q, want .env", n)
	}
	if n := globNeedle("//etc/passwd", "/home/u/"); n == "" {
		t.Error("globNeedle(absolute) lost its needle")
	}
}

func TestSudoDeniedOnlyForWholeSudoRows(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for deny, want := range map[string]bool{
		"Bash(sudo:*)": true, "Bash(sudo *)": true, "Bash(sudo)": true, "Bash(sudo rm *)": false, "Bash(sudoedit *)": false,
	} {
		body := `{"permissions":{"deny":["` + deny + `"]}}`
		if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		policyCache = paths.NewStampCache[policy]("safety.policy")
		if got := sudoDenied(); got != want {
			t.Errorf("%s: sudoDenied = %v, want %v", deny, got, want)
		}
	}
}

func TestBareEditDenyMatchesEveryPath(t *testing.T) {
	globs := foldedGlobs(parsePolicy([]string{"Edit"}).editDeny, "/w")
	if !matchesProtected("/anywhere/deep/file.txt", globs) {
		t.Errorf("bare Edit deny globs %q miss an arbitrary path", globs)
	}
}
