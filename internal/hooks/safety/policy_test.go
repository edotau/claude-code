package safety

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
)

func TestParsePolicyEditRows(t *testing.T) {
	p := parsePolicy(settings.PermissionLists{Deny: []string{"Edit(.env)", "Write(~/x)", "Edit", "Write", "Edit( )", "Edit()"}})
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
		policyCache = paths.StampCache[policy]{}
		if got := sudoDenied(); got != want {
			t.Errorf("%s: sudoDenied = %v, want %v", deny, got, want)
		}
	}
}

func TestBareEditDenyMatchesEveryPath(t *testing.T) {
	globs := foldedGlobs(parsePolicy(settings.PermissionLists{Deny: []string{"Edit"}}).editDeny, "/w")
	if !matchesProtected("/anywhere/deep/file.txt", globs) {
		t.Errorf("bare Edit deny globs %q miss an arbitrary path", globs)
	}
}
