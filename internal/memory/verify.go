package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// VerifyOptions selects a read-only claim scan; CWD defaults to the process working directory.
type VerifyOptions struct {
	CWD                 string
	Archives, AllStores bool
	HelpVerb            func(string) bool
}

type verifyStore struct {
	dir, repo string
	aliases   []string
}

type verifyClaim struct {
	file, id, kind, text, value, scope string
	active, current                    bool
}

type verifyResult struct{ status, evidence string }

type claimVerifier struct {
	store    verifyStore
	objects  map[string]map[string]string
	cache    map[string]verifyResult
	helpVerb func(string) bool
}

var (
	verifyCodeRE       = regexp.MustCompile("`([^`\n]+)`")
	verifyHexRE        = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	verifySymbolRE     = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\(`)
	verifyFileSymbolRE = regexp.MustCompile(`(?:~?/|[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.go:([A-Za-z_][A-Za-z0-9_]*)`)
	verifyCommandRE    = regexp.MustCompile(`\bclaude-code ((?:[a-z][a-z0-9-]*[ \t]+)+)--[a-z][a-z0-9-]*`)
	verifyFlagRE       = regexp.MustCompile(`--[a-z][a-z0-9-]*`)
	verifyForwardRE    = regexp.MustCompile("(?i)\\b(pushed|installed)\\s+(?:(?:through|at|via|as|commit)\\s+)?`?([0-9a-f]{7,40})`?\\b")
	verifyBackwardRE   = regexp.MustCompile("(?i)\\b([0-9a-f]{7,40})`?(?:[ ,]+(?:is|was|now|currently|committed|installed|and|has|been))*[ ,]+(pushed|installed)\\b")
	verifySentenceRE   = regexp.MustCompile(`[.!?;](?:\s|$)`)
)

// Verify prints mechanical evidence only; stale findings are data, not command failures.
func Verify(w io.Writer, o VerifyOptions) error {
	stores, err := verifyStores(o)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, store := range stores {
		claims, err := verifyClaims(store, o.Archives)
		if err != nil {
			return err
		}
		v := claimVerifier{store: store, cache: map[string]verifyResult{}, helpVerb: o.HelpVerb}
		v.loadObjects(claims)
		for _, c := range claims {
			r := v.check(c)
			if r.status == "stale" && !c.active {
				r = verifyResult{"unverifiable", r.evidence + "; historical or non-current file"}
			}
			counts[r.status]++
			if _, err := fmt.Fprintf(w, "%s %s:%s — %s — %s\n", r.status, c.file, c.id, c.text, r.evidence); err != nil {
				return err
			}
		}
	}
	total := counts["ok"] + counts["stale"] + counts["unverifiable"]
	share := 0.0
	if total > 0 {
		share = 100 * float64(counts["ok"]) / float64(total)
	}
	_, err = fmt.Fprintf(w, "summary: ok=%d stale=%d unverifiable=%d total=%d ok_share=%.2f%%\n",
		counts["ok"], counts["stale"], counts["unverifiable"], total, share)
	return err
}

func verifyStores(o VerifyOptions) ([]verifyStore, error) {
	origins, err := verifyOrigins()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(memstore.StoreDir())
	if err != nil {
		return nil, err
	}
	var stores []verifyStore
	for _, e := range entries {
		if !e.IsDir() || (memstore.StoreReservedDirs[e.Name()] && e.Name() != "home") {
			continue
		}
		s := verifyStore{dir: filepath.Join(memstore.StoreDir(), e.Name())}
		if e.Name() != "home" {
			s.repo, s.aliases = origins[e.Name()].Origin, origins[e.Name()].Aliases
			if e.Name() == memstore.HarnessStoreKey() {
				s.repo = memstore.ResolvePath(paths.ConfigDir())
			}
		}
		stores = append(stores, s)
	}
	if o.AllStores {
		return stores, nil
	}
	return verifySelectStore(stores, o.CWD)
}

// verifyOrigins avoids ReadOrigin/RepoDirs, whose legacy fallback imports rows into the vault.
func verifyOrigins() (map[string]memstore.Origin, error) {
	out := map[string]memstore.Origin{}
	data, err := os.ReadFile(filepath.Join(memstore.StoreDir(), memstore.CatalogFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, rest, ok := strings.Cut(strings.TrimPrefix(line, "- "), " — ")
		_, raw, found := strings.CutLast(rest, "<!-- origin: ")
		var origin memstore.Origin
		if ok && found && bank.ValidBareName(key) == nil && json.Unmarshal([]byte(strings.TrimSuffix(raw, " -->")), &origin) == nil {
			out[key] = origin
		}
	}
	err = vault.View(func(tx *vault.Tx) error {
		for key, raw := range tx.All(vault.MemoryOrigins) {
			var origin memstore.Origin
			if json.Unmarshal([]byte(raw), &origin) == nil && origin.Key == key && origin.Version == memstore.OriginVersion {
				out[key] = origin
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read memory origins: %w", err)
	}
	return out, nil
}

func verifySelectStore(stores []verifyStore, cwd string) ([]verifyStore, error) {
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	cwd = memstore.ResolvePath(cwd)
	root := memstore.MainCheckout(cwd)
	best, length, home := -1, 0, -1
	for i, s := range stores {
		if filepath.Base(s.dir) == "home" {
			home = i
		}
		for _, candidate := range append([]string{s.dir, s.repo}, s.aliases...) {
			if candidate == "" {
				continue
			}
			candidate = memstore.ResolvePath(candidate)
			if (verifyWithin(candidate, cwd) || candidate == root) && len(candidate) > length {
				best, length = i, len(candidate)
			}
		}
	}
	if best < 0 && root == "" {
		best = home
	}
	if best < 0 {
		return nil, fmt.Errorf("no memory store for %s", cwd)
	}
	return []verifyStore{stores[best]}, nil
}

func verifyWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func verifyClaims(store verifyStore, archives bool) ([]verifyClaim, error) {
	var claims []verifyClaim
	err := filepath.WalkDir(store.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != store.dir && (!archives || !verifyWithin(memstore.ArchiveDir(store.dir), path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		kind := bank.ClassifyMemoryFile(filepath.Dir(path), filepath.Base(path))
		if kind != bank.KindBank && kind != bank.KindTopic {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(memstore.StoreDir(), path)
		active := filepath.Dir(path) == store.dir && verifyActive(filepath.Base(path))
		for _, block := range verifyBlocks(filepath.Base(path), string(data), kind) {
			id := bank.BlockID(block)
			if id == "" {
				id = bank.ContentRef(block)
			}
			claims = append(claims, verifyExtract(filepath.ToSlash(rel), id, block, active)...)
		}
		return nil
	})
	return claims, err
}

func verifyActive(name string) bool {
	return name == "activeContext.md" || name == "progress.md" || name == "conventions.md" ||
		strings.HasPrefix(name, "feedback-") || strings.HasPrefix(name, "project-")
}

func verifyBlocks(name, body string, kind bank.FileKind) []string {
	if kind == bank.KindTopic {
		return []string{body}
	}
	for _, base := range bank.BankFiles() {
		if strings.HasPrefix(name, strings.TrimSuffix(base, ".md")+"-") {
			name = base
			break
		}
	}
	header, blocks := bank.EntryBlocks(name, body)
	if strings.TrimSpace(header) != "" {
		blocks = append([]string{header}, blocks...)
	}
	return blocks
}

func verifyExtract(file, id, block string, active bool) []verifyClaim {
	var out []verifyClaim
	seen := map[string]bool{}
	add := func(kind, text, value, scope string, current bool) {
		key := kind + "\x00" + text
		if !seen[key] {
			seen[key] = true
			out = append(out, verifyClaim{file, id, kind, text, value, scope, active, current})
		}
	}
	for _, m := range verifyCodeRE.FindAllStringSubmatch(block, -1) {
		if verifyHexRE.MatchString(m[1]) {
			add("hash", m[0], strings.ToLower(m[1]), "", false)
		} else if verifyPathToken(m[1]) && !verifyFileSymbolRE.MatchString(m[1]) {
			path := strings.SplitN(m[1], ":", 2)[0]
			add("path", m[0], path, "", false)
		}
	}
	for _, m := range verifyFileSymbolRE.FindAllStringSubmatch(block, -1) {
		file, _, _ := strings.Cut(m[0], ":")
		add("symbol", m[0], m[1], file, false)
	}
	unscoped := verifyFileSymbolRE.ReplaceAllString(block, "")
	for _, m := range verifySymbolRE.FindAllStringSubmatch(unscoped, -1) {
		add("symbol", m[0], m[1], "", false)
	}
	for _, m := range verifyCommandRE.FindAllStringSubmatch(block, -1) {
		add("flag", m[0], verifyFlagRE.FindString(m[0]), strings.TrimSpace(m[1]), false)
	}
	for _, sentence := range verifySentenceRE.Split(strings.Join(strings.Fields(block), " "), -1) {
		verifyStateClaims(sentence, add)
	}
	return out
}

func verifyPathToken(s string) bool {
	for _, prefix := range []string{"internal/", "rules/", "agents/", "skills/", "commands/", "../", "/", "~/"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func verifyStateClaims(sentence string, add func(string, string, string, string, bool)) {
	for i, re := range []*regexp.Regexp{verifyForwardRE, verifyBackwardRE} {
		for _, loc := range re.FindAllStringSubmatchIndex(sentence, -1) {
			text := sentence[loc[0]:loc[1]]
			kind, sha := sentence[loc[2]:loc[3]], sentence[loc[4]:loc[5]]
			if i == 1 {
				kind, sha = sha, kind
			}
			prefix := strings.ToLower(sentence[:loc[0]])
			if strings.HasSuffix(prefix, "not ") || strings.HasSuffix(prefix, "never ") || strings.HasSuffix(prefix, "will be ") {
				continue
			}
			add(strings.ToLower(kind), text, strings.ToLower(sha), "", strings.Contains(strings.ToLower(sentence), "currently"))
		}
	}
}

func (v *claimVerifier) loadObjects(claims []verifyClaim) {
	var tokens []string
	for _, c := range claims {
		if c.kind == "hash" || c.kind == "pushed" || c.kind == "installed" {
			tokens = append(tokens, c.value)
		}
	}
	slices.Sort(tokens)
	tokens = slices.Compact(tokens)
	v.objects = map[string]map[string]string{}
	for _, repo := range []string{v.store.repo, memstore.StoreDir()} {
		if repo != "" {
			v.objects[repo] = verifyBatch(repo, tokens)
		}
	}
}

func verifyGit(repo, input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := []string{"--no-pager", "-c", "core.fsmonitor=false", "-C", repo}
	if repo == memstore.StoreDir() {
		base = append(base, "--git-dir="+filepath.Join(repo, ".git"), "--work-tree="+repo)
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = append(proc.GitEnv(), "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func verifyBatch(repo string, tokens []string) map[string]string {
	out := map[string]string{}
	if len(tokens) == 0 {
		return out
	}
	data, err := verifyGit(repo, strings.Join(tokens, "\n")+"\n", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return out
	}
	for i, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if i < len(tokens) && len(fields) == 2 && verifyHexRE.MatchString(fields[0]) &&
			(fields[1] == "commit" || fields[1] == "tree" || fields[1] == "blob" || fields[1] == "tag") {
			out[tokens[i]] = fields[0]
		}
	}
	return out
}

func (v *claimVerifier) check(c verifyClaim) verifyResult {
	key := c.kind + "\x00" + c.value + "\x00" + c.scope + fmt.Sprint(c.current)
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
		switch c.kind {
		case "path":
			result = v.path(c.value)
		case "symbol":
			result = v.symbol(c)
		case "flag":
			result = v.flag(c)
		case "pushed":
			result = v.pushed(c.value)
		case "installed":
			result = verifyResult{"unverifiable", "running binary belongs to another repository"}
			if paths.SamePath(v.store.repo, paths.ConfigDir()) {
				result = verifyInstalled(c)
			}
		}
	}
	v.cache[key] = result
	return result
}

func (v *claimVerifier) hash(token string) verifyResult {
	for _, repo := range []string{v.store.repo, memstore.StoreDir()} {
		if sha := v.objects[repo][token]; sha != "" {
			return verifyResult{"ok", repo + " object " + sha}
		}
	}
	files, err := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", token+"*.jsonl"))
	if err == nil {
		for _, path := range files {
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
	path = memstore.ResolvePath(path)
	return path, verifyWithin(memstore.ResolvePath(v.store.repo), path)
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
		return verifyResult{"stale", "missing " + path}
	default:
		return verifyResult{"unverifiable", "cannot stat " + path}
	}
}

func (v *claimVerifier) symbol(c verifyClaim) verifyResult {
	args := []string{"grep", "-l", "-w", "-F", "-e", c.value, "--"}
	if c.scope == "" {
		args = append(args, "*.go")
	} else if strings.Contains(c.scope, "/") {
		path, ok := v.scopedPath(c.scope)
		if !ok {
			return verifyResult{"unverifiable", "symbol path is outside store repository"}
		}
		rel, _ := filepath.Rel(v.store.repo, path)
		args = append(args, ":(literal)"+filepath.ToSlash(rel))
	} else {
		args = append(args, ":(glob)**/"+c.scope)
	}
	out, err := verifyGit(v.store.repo, "", args...)
	if err == nil {
		return verifyResult{"ok", "git grep -w: " + firstLine(out)}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return verifyResult{"stale", "git grep -w: no " + c.value + " in " + v.store.repo}
	}
	return verifyResult{"unverifiable", "git grep unavailable in " + v.store.repo}
}

func (v *claimVerifier) pushed(token string) verifyResult {
	sha := v.objects[v.store.repo][token]
	if sha == "" {
		if _, err := verifyGit(v.store.repo, "", "rev-parse", "--git-dir"); err != nil {
			return verifyResult{"unverifiable", "repository unavailable"}
		}
		return verifyResult{"stale", "pushed claim: sha does not resolve in " + v.store.repo}
	}
	if _, err := verifyGit(v.store.repo, "", "rev-parse", "--verify", "origin/main^{commit}"); err != nil {
		return verifyResult{"unverifiable", "origin/main unavailable locally; no fetch attempted"}
	}
	_, err := verifyGit(v.store.repo, "", "merge-base", "--is-ancestor", sha, "origin/main")
	if err == nil {
		return verifyResult{"ok", sha + " is an ancestor of local origin/main"}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return verifyResult{"stale", "pushed claim: " + sha + " is not an ancestor of local origin/main"}
	}
	return verifyResult{"unverifiable", "cannot check pushed ancestry"}
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
	var command []string
	for i, word := range words {
		if i > 0 {
			help, err := v.help(command)
			if err != nil || !verifyHelpCommand(help, word) {
				return verifyResult{"unverifiable", "subcommand not listed by parent -h"}
			}
		}
		command = append(command, word)
	}
	help, err := v.help(command)
	if err != nil || !strings.Contains(help, "usage:") {
		return verifyResult{"unverifiable", c.scope + " -h unavailable"}
	}
	for line := range strings.SplitSeq(help, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.TrimLeft(fields[0], "-") == strings.TrimLeft(c.value, "-") {
			return verifyResult{"ok", c.scope + " -h lists " + c.value}
		}
	}
	return verifyResult{"stale", c.scope + " -h does not list " + c.value}
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
