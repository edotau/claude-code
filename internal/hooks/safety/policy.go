package safety

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/settings"
)

// policy is the settings.json permissions.deny safety-scan reads: Bash rules, Edit/Write globs as written.
type policy struct {
	bashDeny []permission.Rule
	editDeny []string
}

var (
	policyCache    = paths.NewStampCache[policy]("safety.policy")
	templatePolicy = sync.OnceValue(func() policy {
		var doc struct {
			Permissions settings.PermissionLists `json:"permissions"`
		}
		_ = json.Unmarshal(settings.Template(), &doc) // the embedded template is tested
		return parsePolicy(doc.Permissions.Deny)
	})
)

// livePolicy is the user settings policy; a missing or broken settings.json falls back to the template's.
func livePolicy() policy {
	p, err := policyCache.Get(settings.SettingsFile(), readPolicy)
	if err != nil {
		noteFallback(err)
		return templatePolicy()
	}
	return p
}

// readPolicy is settings.LivePermissions with a stat-sized buffer: on a 5 KB file 38 to 31 allocs, 13.7 to 8.4 KB.
func readPolicy() (p policy, err error) {
	const maxBytes = 4 << 20
	f, err := os.Open(settings.SettingsFile())
	if err != nil {
		return p, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return p, err
	}
	// ReadFrom needs spare capacity to probe EOF without growing a correctly sized buffer.
	buf := bytes.NewBuffer(make([]byte, 0, min(info.Size(), maxBytes)+bytes.MinRead+1))
	if _, err := buf.ReadFrom(io.LimitReader(f, maxBytes+1)); err != nil {
		return p, err
	}
	if buf.Len() > maxBytes {
		return p, fmt.Errorf("%w (%s): trim it below 4 MiB or safety-scan keeps using template permissions",
			settings.ErrTooLarge, filepath.Base(settings.SettingsFile()))
	}
	var doc struct {
		Permissions *struct {
			Allow []string  `json:"allow"` // allow and ask are decoded only so a malformed list rejects the file
			Ask   []string  `json:"ask"`
			Deny  *[]string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		return p, fmt.Errorf("settings.json: %w", err)
	}
	if doc.Permissions == nil || doc.Permissions.Deny == nil {
		return p, errors.New("settings.json: permissions.deny is missing")
	}
	return parsePolicy(*doc.Permissions.Deny), nil
}

var fallbackOnce sync.Once

// noteFallback says once per process why the template permissions apply; a missing file is a normal install.
func noteFallback(err error) {
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	fallbackOnce.Do(func() {
		cause := "settings.json unreadable or invalid (" + err.Error() + ")"
		if errors.Is(err, settings.ErrTooLarge) {
			cause = "settings.json over 4 MiB"
		}
		fmt.Fprintln(os.Stderr, "safety-scan: "+cause+": using template permissions; custom deny rows not applied")
	})
}

// parsePolicy keeps Bash rules and Edit/Write path args; an unparseable rule is skipped, as Claude Code skips it.
func parsePolicy(deny []string) policy {
	var p policy
	for _, s := range deny {
		if r, err := permission.ParseRule(s); err == nil && r.Tool == "Bash" {
			p.bashDeny = append(p.bashDeny, r)
			continue
		}
		if g := editGlob(s); g != "" {
			p.editDeny = append(p.editDeny, g)
		}
	}
	return p
}

// editGlob is the raw path glob of an Edit/Write row, else "" (raw arg: ParseRule's ~ expansion loses the anchor).
func editGlob(row string) string {
	if row == "Edit" || row == "Write" { // a bare tool row covers every path
		return "**/*"
	}
	for _, tool := range [...]string{"Edit(", "Write("} {
		g, ok := strings.CutPrefix(row, tool)
		if !ok || !strings.HasSuffix(g, ")") {
			continue
		}
		if g = strings.TrimSpace(strings.TrimSuffix(g, ")")); g != "" {
			return g
		}
	}
	return ""
}

// settingsRuleHit is the first of rules a segment of cmd runs (wrappers, sh -c, substitutions seen through), else "".
func settingsRuleHit(cmd, cwd string, rules []permission.Rule) string {
	judge := func(rs []permission.Rule) bool {
		return permission.Permissions{Mode: permission.PermAllow, Deny: rs}.DecideAt(cwd, "Bash", cmd) == permission.Deny
	}
	if len(rules) == 0 || !judge(rules) {
		return ""
	}
	for _, r := range rules {
		if judge([]permission.Rule{r}) {
			return r.String()
		}
	}
	return "command nests too deep to check" // decideBash fails closed past depth 8
}

// sudoDenied reports a settings deny row aimed at sudo; read raw, since DecideAt unwraps sudo as a wrapper.
func sudoDenied() bool {
	return slices.ContainsFunc(livePolicy().bashDeny, func(r permission.Rule) bool {
		a := strings.TrimSpace(r.Arg)
		return r.Tool == "Bash" && (a == "sudo" || a == "sudo:*" || a == "sudo *")
	})
}

// anchorGlob resolves a user-settings path pattern: //abs, ~/home, /config-dir, **/anywhere, else cwd ("" if none).
func anchorGlob(g, cwd string) string {
	switch {
	case strings.HasPrefix(g, "//"):
		g = g[1:]
	case strings.HasPrefix(g, "~/"):
		g = paths.ExpandHome(g)
	case strings.HasPrefix(g, "/"):
		g = filepath.Join(paths.ConfigDir(), g)
	case strings.HasPrefix(g, "**/"):
	case cwd == "":
		return ""
	default:
		g = filepath.Join(cwd, strings.TrimPrefix(g, "./"))
	}
	return paths.FoldCase(paths.CleanGlob(g))
}

// foldedGlobs are raw globs anchored at cwd and case-folded for matchesProtected; unanchorable ones drop.
func foldedGlobs(raw []string, cwd string) []string {
	var out []string
	for _, g := range raw {
		if a := anchorGlob(g, cwd); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// globNeedle is the longest wildcard-free run of an anchored glob, home-relative, that a script must mention.
func globNeedle(g, home string) string {
	var best string
	a := anchorGlob(g, "")
	if a == "" { // a cwd-relative glob has no anchor yet; its own text is the needle
		a = paths.FoldCase(paths.CleanGlob(strings.TrimPrefix(g, "./")))
	}
	for _, part := range strings.FieldsFunc(strings.TrimPrefix(a, paths.FoldCase(home)), func(r rune) bool {
		return r == '*' || r == '?'
	}) {
		if part = strings.TrimLeft(part, "/"); len(part) > len(best) {
			best = part
		}
	}
	if len(best) < 4 {
		return ""
	}
	return best
}
