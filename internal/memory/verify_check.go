package memory

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

func (v *claimVerifier) check(c verifyClaim) verifyResult {
	key := c.kind + "\x00" + c.value + "\x00" + c.scope + strconv.FormatBool(c.current)
	if result, ok := v.cache[key]; ok {
		return result
	}
	result := verifyResult{"unverifiable", "store has no repository"}
	if c.kind == "hash" {
		result = v.hash(c.value)
	} else if v.store.repo != "" {
		if fi, err := os.Stat(v.store.repo); err != nil || !fi.IsDir() {
			return verifyResult{"unverifiable", "store repository is unavailable"}
		}
		result = v.checkInRepo(c)
	}
	v.cache[key] = result
	return result
}

// checkInRepo verifies a non-hash claim against the store's repository, which is known to exist.
func (v *claimVerifier) checkInRepo(c verifyClaim) verifyResult {
	switch c.kind {
	case "path":
		return v.path(c.value)
	case "symbol":
		return v.symbol(c)
	case "flag":
		return v.flag(c)
	case "pushed":
		return v.pushed(c.value)
	case "installed":
		if paths.SamePath(v.store.repo, paths.ConfigDir()) {
			return verifyInstalled(c)
		}
		return verifyResult{"unverifiable", "running binary belongs to another repository"}
	}
	return verifyResult{"unverifiable", "store has no repository"}
}

func (v *claimVerifier) hash(token string) verifyResult {
	for _, repo := range []string{v.store.repo, memstore.StoreDir()} {
		if sha := v.objects[repo][token]; sha != "" {
			return verifyResult{"ok", repo + " object " + sha}
		}
	}
	for _, path := range v.sessions {
		if strings.HasPrefix(filepath.Base(path), token) {
			if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
				return verifyResult{"ok", "session " + path}
			}
		}
	}
	return verifyResult{"unverifiable", "not resolved in repo, memory repo, or session ids"}
}

func (v *claimVerifier) scopedPath(token string) (string, bool) {
	path := token
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(v.store.repo, path)
	}
	for p := path; p != filepath.Dir(p); p = filepath.Dir(p) {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if _, err := os.Stat(p); err != nil {
				return "", false
			}
		}
	}
	path = paths.CanonDeep(path)
	return path, verifyWithin(paths.CanonDeep(v.store.repo), path)
}

func (v *claimVerifier) path(token string) verifyResult {
	if strings.ContainsAny(token, "*?{}[] \t") {
		return verifyResult{"unverifiable", "path is a pattern or includes arguments"}
	}
	path, ok := v.scopedPath(token)
	if !ok {
		return verifyResult{"unverifiable", "path is outside store repository"}
	}
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return verifyResult{"ok", "exists " + path}
	case errors.Is(err, fs.ErrNotExist):
		rel, err := filepath.Rel(paths.CanonDeep(v.store.repo), path)
		if err != nil {
			return verifyResult{"unverifiable", "cannot locate path in store repository"}
		}
		top, _, _ := strings.Cut(rel, string(filepath.Separator))
		if fi, err := os.Stat(filepath.Join(v.store.repo, top)); err != nil || !fi.IsDir() {
			return verifyResult{"unverifiable", "top-level directory is absent from store repository: " + top}
		}
		return verifyResult{"stale", "missing " + path}
	default:
		return verifyResult{"unverifiable", "cannot stat " + path}
	}
}

func (v *claimVerifier) symbol(c verifyClaim) verifyResult {
	if c.scope == "" {
		return v.symbols[c.value]
	}
	files := v.scopeFiles(c.scope)
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(c.value) + `\b`)
	checked, uncertain := "", false
	for _, file := range files {
		path, state := v.symbolFile(file, word)
		switch state {
		case symbolFound:
			return verifyResult{"ok", "symbol " + c.value + " in " + path}
		case symbolMissing:
			checked = path
		case symbolUncertain:
			uncertain = true
		}
	}
	if checked != "" && !uncertain {
		return verifyResult{"stale", "no " + c.value + " in existing file scope " + c.scope + " in " + v.store.repo}
	}
	return verifyResult{"unverifiable", "symbol file is missing, outside the repository, or unreadable: " + c.scope}
}

// scopeFiles is the scope itself plus, for a bare file name, every repo Go file of that name.
func (v *claimVerifier) scopeFiles(scope string) []string {
	files := []string{scope}
	if !strings.Contains(scope, "/") {
		for _, file := range v.sourceFiles {
			if file != scope && filepath.Base(file) == scope {
				files = append(files, file)
			}
		}
	}
	return files
}

// Outcomes of symbolFile: the word is in the file, the file lacks it, the file could not be judged, or is absent.
const (
	symbolFound = iota
	symbolMissing
	symbolUncertain
	symbolAbsent
)

// symbolFile greps one scoped file for word and returns its resolved path with the outcome.
func (v *claimVerifier) symbolFile(file string, word *regexp.Regexp) (string, int) {
	path, ok := v.scopedPath(file)
	if !ok {
		return "", symbolUncertain
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, symbolAbsent
	}
	if err != nil || !fi.Mode().IsRegular() {
		return path, symbolUncertain
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return path, symbolUncertain
	}
	if word.Match(data) {
		return path, symbolFound
	}
	return path, symbolMissing
}

func (v *claimVerifier) defaultBranch(repo string) string {
	if branch, ok := v.branches[repo]; ok {
		return branch
	}
	branch, _ := verifyGit(repo, "", "symbolic-ref", "refs/remotes/origin/HEAD")
	for _, candidate := range []string{branch, "origin/main"} {
		if candidate == "" {
			continue
		}
		if _, err := verifyGit(repo, "", "rev-parse", "--verify", candidate+"^{commit}"); err == nil {
			v.branches[repo] = candidate
			return candidate
		}
	}
	v.branches[repo] = ""
	return ""
}

func (v *claimVerifier) pushed(token string) verifyResult {
	if result, ok := v.pushedResults[token]; ok {
		return result
	}
	result := verifyResult{"unverifiable", "pushed claim: sha does not resolve in any known store repository"}
	for _, repo := range v.repos {
		sha := v.objects[repo][token]
		if sha == "" {
			continue
		}
		var final bool
		if result, final = v.pushedIn(repo, sha, result); final {
			break
		}
	}
	v.pushedResults[token] = result
	return result
}

// pushedIn judges sha against repo's default branch; prev is the verdict so far (a stale one is never softened) and
// final is true when the claim is settled as ok.
func (v *claimVerifier) pushedIn(repo, sha string, prev verifyResult) (result verifyResult, final bool) {
	result = prev
	branch := v.defaultBranch(repo)
	if branch == "" {
		if result.status != "stale" {
			result = verifyResult{"unverifiable", "origin/HEAD and origin/main unavailable locally in " + repo + "; no fetch attempted"}
		}
		return result, false
	}
	_, err := verifyGit(repo, "", "merge-base", "--is-ancestor", sha, branch)
	if err == nil {
		return verifyResult{"ok", sha + " is an ancestor of local " + strings.TrimPrefix(branch, "refs/remotes/") + " in " + repo}, true
	}
	if remotes, _ := verifyGit(repo, "", "branch", "-r", "--contains", sha); strings.TrimSpace(remotes) != "" {
		return verifyResult{"ok", sha + " is on remote branch " + strings.Fields(remotes)[0] + " in " + repo}, true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		result = verifyResult{"stale", "pushed claim: " + sha + " is not an ancestor of local " + strings.TrimPrefix(branch, "refs/remotes/") + " in " + repo}
	} else if result.status != "stale" {
		result = verifyResult{"unverifiable", "cannot check pushed ancestry in " + repo}
	}
	return result, false
}

func verifyInstalled(c verifyClaim) verifyResult {
	if !c.current {
		return verifyResult{"unverifiable", "installed claim is not explicitly current"}
	}
	revision, dirty := "", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				revision = s.Value
			}
			if s.Key == "vcs.modified" {
				dirty = s.Value == "true"
			}
		}
	}
	if revision == "" || dirty {
		return verifyResult{"unverifiable", "running binary " + textutil.BuildVersion() + " has no clean VCS revision"}
	}
	if strings.HasPrefix(revision, c.value) {
		return verifyResult{"ok", "running binary revision " + revision}
	}
	return verifyResult{"stale", "currently installed claim differs from running binary " + revision}
}

func (v *claimVerifier) flag(c verifyClaim) verifyResult {
	if !paths.SamePath(v.store.repo, paths.ConfigDir()) {
		return verifyResult{"unverifiable", "claude-code help belongs to another repository"}
	}
	words := strings.Fields(c.scope)
	if len(words) == 0 || v.helpVerb == nil || !v.helpVerb(words[0]) {
		return verifyResult{"unverifiable", "no local read-only help handler for " + c.scope}
	}
	if !v.subcommandsListed(words) {
		return verifyResult{"unverifiable", "subcommand not listed by parent -h"}
	}
	help, err := v.help(words)
	if err != nil || !strings.Contains(help, "usage:") {
		return verifyResult{"unverifiable", c.scope + " -h unavailable"}
	}
	if helpListsFlag(help, c.value) {
		return verifyResult{"ok", c.scope + " -h lists " + c.value}
	}
	return verifyResult{"stale", c.scope + " -h does not list " + c.value}
}

// subcommandsListed reports whether each word after the first is listed in its parent command's -h.
func (v *claimVerifier) subcommandsListed(words []string) bool {
	for i := 1; i < len(words); i++ {
		help, err := v.help(words[:i])
		if err != nil || !verifyHelpCommand(help, words[i]) {
			return false
		}
	}
	return true
}

// helpListsFlag reports whether a help line starts with flag (dashes ignored).
func helpListsFlag(help, flag string) bool {
	for line := range strings.SplitSeq(help, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.TrimLeft(fields[0], "-") == strings.TrimLeft(flag, "-") {
			return true
		}
	}
	return false
}

func verifyHelpCommand(help, word string) bool {
	commands := false
	for line := range strings.SplitSeq(help, "\n") {
		if line == "COMMANDS" {
			commands = true
			continue
		}
		if commands && line == "" {
			commands = false
		}
		fields := strings.Fields(line)
		if commands && len(fields) > 0 && fields[0] == word {
			return true
		}
	}
	return false
}

func (v *claimVerifier) help(command []string) (string, error) {
	key := "help\x00" + strings.Join(command, " ")
	if result, ok := v.cache[key]; ok {
		if result.status != "ok" {
			return "", errors.New(result.evidence)
		}
		return result.evidence, nil
	}
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append(slices.Clone(command), "-h")...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	result := verifyResult{"ok", string(out)}
	if err != nil {
		result = verifyResult{"unverifiable", "help command failed"}
	}
	v.cache[key] = result
	return string(out), err
}
