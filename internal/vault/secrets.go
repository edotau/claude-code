package vault

import (
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/shapes"
)

// Exit codes shared by every audit (scan/git/doctor/all): 0 clean, 1 finding(s), 2 usage error.
const (
	ExitClean    = 0
	ExitFindings = 1
)

// base64ish catches high-entropy tokens that don't match a known prefix.
var base64ish = regexp.MustCompile(`^[A-Za-z0-9_+/=-]{32,}$`)

// LooksLikeSecret is the credential-shape heuristic for a single, already-isolated value (a JSON leaf
// or a KEY=value RHS): known token prefixes, a JWT shape, or base64-ish noise; ${ENV} refs, file paths,
// and obvious placeholders are never flagged.
func LooksLikeSecret(val string) bool {
	return LooksLikeSecretGeneric(val, true)
}

// LooksLikeSecretGeneric is LooksLikeSecret with the bare base64-ish rule switchable via generic.
func LooksLikeSecretGeneric(val string, generic bool) bool {
	switch {
	case val == "":
		return false
	case strings.HasPrefix(val, "$"): // ${ENV} or $ENV reference
		return false
	case strings.HasPrefix(val, "/") || strings.HasPrefix(val, "~/"): // file path
		return false
	case strings.ContainsAny(val, "<>") || strings.Contains(val, "PLACEHOLDER") ||
		strings.Contains(val, "YOUR_") || strings.Contains(val, "xxx"):
		return false
	case shapes.HasGitHubPrefix(val):
		return true
	case matchSlack(val):
		return true
	case strings.HasPrefix(val, "AKIA"): // AWS
		return true
	case strings.HasPrefix(val, "sk-"): // OpenAI-style
		return true
	case strings.HasPrefix(val, "eyJ") && strings.Count(val, ".") >= 2: // JWT
		return true
	default:
		return generic && base64ish.MatchString(val)
	}
}

// matchSlack matches a Slack bot/user/app/legacy token (xox[baprs]-...).
func matchSlack(v string) bool {
	if len(v) < 5 || !strings.HasPrefix(v, "xox") || v[4] != '-' {
		return false
	}
	return strings.ContainsRune("baprs", rune(v[3]))
}

// secretPattern is the substring regex used over raw text (git diff lines) where a value cannot be
// isolated first; narrower than LooksLikeSecret (no bare base64 rule) to avoid flooding history scans
// with incidental base64 blobs.
var secretPattern = regexp.MustCompile(
	`(` + shapes.GitHubPattern(20) + `|` +
		`xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|sk-[A-Za-z0-9_-]{20,}|` +
		`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`)

// MasterKeyStatus resolves the vault's secret count and master-key source in one place, shared by the
// "vault master key" doctor row and launch's "vault" summary row so both describe the same check.
// count is 0 and err is non-nil when NamesErr itself failed (vault unreadable); count is 0 and err is
// nil when the vault holds no secrets (no master key needed); count > 0 and err non-nil when the
// resolved master key cannot open the vault.
func MasterKeyStatus() (count int, source string, err error) {
	names, err := NamesErr()
	if err != nil {
		return 0, "", err
	}
	count = len(names)
	if count == 0 {
		return 0, "not needed", nil
	}
	_, source, err = MasterKey()
	return count, source, err
}
