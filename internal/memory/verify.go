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
	"strconv"
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
	store         verifyStore
	repos         []string
	objects       map[string]map[string]string
	sessions      []string
	branches      map[string]string
	pushedResults map[string]verifyResult
	symbols       map[string]verifyResult
	sourceFiles   []string
	cache         map[string]verifyResult
	helpVerb      func(string) bool
}

var (
	verifyCodeRE       = regexp.MustCompile("`([^`\n]+)`")
	verifyHexRE        = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	verifySymbolRE     = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\(`)
	verifyWordRE       = regexp.MustCompile(`[\pL\pN_]+`)
	verifyFileSymbolRE = regexp.MustCompile(`(?:~?/|[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.go:([A-Za-z_][A-Za-z0-9_]*)`)
	verifyCommandRE    = regexp.MustCompile(`\bclaude-code ((?:[a-z][a-z0-9-]*[ \t]+)+)--[a-z][a-z0-9-]*`)
	verifyFlagRE       = regexp.MustCompile(`--[a-z][a-z0-9-]*`)
	verifyForwardRE    = regexp.MustCompile("(?i)\\b(pushed|installed)\\s+(?:(?:through|at|via|as|commit)\\s+)?`?([0-9a-f]{7,40})\\b`?")
	verifyBackwardRE   = regexp.MustCompile("(?i)`?\\b([0-9a-f]{7,40})`?(?:[ ,]+(?:is|was|now|currently|committed|installed|and|has|been))*[ ,]+(pushed|installed)\\b")
	verifySentenceRE   = regexp.MustCompile(`[.!?;](?:\s|$)`)
	verifyNotStateRE   = regexp.MustCompile(`\b(not|not yet|never|wasn't|isn't|hasn't been|will be|to be)\s+$`)
)

// Verify prints mechanical evidence only; stale findings are data, not command failures.
func Verify(w io.Writer, o VerifyOptions) error {
	known, err := verifyStores(VerifyOptions{AllStores: true})
	if err != nil {
		return err
	}
	stores := known
	if !o.AllStores {
		stores, err = verifySelectStore(known, o.CWD)
		if err != nil {
			return err
		}
	}
	claims := make([][]verifyClaim, len(stores))
	var allClaims []verifyClaim
	for i, store := range stores {
		claims[i], err = verifyClaims(store, o.Archives)
		if err != nil {
			return err
		}
		allClaims = append(allClaims, claims[i]...)
	}
	v := claimVerifier{helpVerb: o.HelpVerb, branches: map[string]string{}, pushedResults: map[string]verifyResult{}}
	v.loadObjects(known, allClaims)
	v.sessions, _ = filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "*.jsonl"))
	counts := map[string]int{}
	for i, store := range stores {
		v.store, v.cache = store, map[string]verifyResult{}
		v.loadSymbols(claims[i])
		for _, c := range claims[i] {
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
	root := paths.MainCheckout(cwd, memstore.GitOut)
	if root != "" {
		root = memstore.ResolvePath(root)
	}
	inStore := verifyWithin(memstore.ResolvePath(memstore.StoreDir()), cwd)
	best, length, home := -1, 0, -1
	for i, s := range stores {
		if filepath.Base(s.dir) == "home" {
			home = i
		}
		candidates := append([]string{s.dir, s.repo}, s.aliases...)
		if inStore {
			candidates = []string{s.dir}
		}
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			candidate = memstore.ResolvePath(candidate)
			if (verifyWithin(candidate, cwd) || candidate == root) && len(candidate) > length {
				best, length = i, len(candidate)
			}
		}
	}
	if best < 0 && (root == "" || inStore) {
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
	for _, base := range bank.BankFiles() {
		if strings.HasPrefix(name, strings.TrimSuffix(base, ".md")+"-") || strings.HasSuffix(name, "-"+base) {
			name, kind = base, bank.KindBank
			break
		}
	}
	if kind == bank.KindTopic {
		return []string{body}
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
			path, _, _ := strings.Cut(m[1], ":")
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
			if verifyNotStateRE.MatchString(prefix) {
				continue
			}
			add(strings.ToLower(kind), text, strings.ToLower(sha), "", strings.Contains(strings.ToLower(sentence), "currently"))
		}
	}
}

func (v *claimVerifier) loadObjects(stores []verifyStore, claims []verifyClaim) {
	var tokens []string
	for _, c := range claims {
		if c.kind == "hash" || c.kind == "pushed" || c.kind == "installed" {
			tokens = append(tokens, c.value)
		}
	}
	slices.Sort(tokens)
	tokens = slices.Compact(tokens)
	v.objects = map[string]map[string]string{}
	for _, store := range stores {
		if store.repo != "" && v.objects[store.repo] == nil {
			v.repos = append(v.repos, store.repo)
			v.objects[store.repo] = verifyBatch(store.repo, tokens)
		}
	}
	if v.objects[memstore.StoreDir()] == nil {
		v.objects[memstore.StoreDir()] = verifyBatch(memstore.StoreDir(), tokens)
	}
}

func (v *claimVerifier) loadSymbols(claims []verifyClaim) {
	v.symbols, v.sourceFiles = map[string]verifyResult{}, nil
	if v.store.repo == "" {
		return
	}
	var tokens []string
	needFiles := false
	for _, c := range claims {
		if c.kind != "symbol" {
			continue
		}
		if c.scope == "" {
			if _, ok := v.symbols[c.value]; !ok {
				tokens = append(tokens, c.value)
				v.symbols[c.value] = verifyResult{"unverifiable", "bare symbol has no file scope: " + c.value}
			}
		} else if !strings.Contains(c.scope, "/") {
			needFiles = true
		}
	}
	if needFiles {
		if out, err := verifyGit(v.store.repo, "", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go"); err == nil {
			v.sourceFiles = strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
		}
	}
	if len(tokens) == 0 {
		return
	}
	// Listing matches short-circuits each file; grep -o repeats every pattern at every matching position.
	out, err := verifyGit(v.store.repo, strings.Join(tokens, "\n")+"\n", "grep", "--threads=4",
		"--no-color", "--full-name", "-I", "-w", "-l", "-z", "-F", "-f", "-", "--")
	if err != nil {
		return
	}
	for file := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		path, ok := v.scopedPath(file)
		if !ok {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, word := range verifyWordRE.FindAll(data, -1) {
			if result, ok := v.symbols[string(word)]; ok && result.status != "ok" {
				v.symbols[string(word)] = verifyResult{"ok", "git grep -w: " + file}
			}
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
		rel, err := filepath.Rel(memstore.ResolvePath(v.store.repo), path)
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
	files := []string{c.scope}
	if !strings.Contains(c.scope, "/") {
		for _, file := range v.sourceFiles {
			if file != c.scope && filepath.Base(file) == c.scope {
				files = append(files, file)
			}
		}
	}
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(c.value) + `\b`)
	checked, uncertain := "", false
	for _, file := range files {
		path, ok := v.scopedPath(file)
		if !ok {
			uncertain = true
			continue
		}
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !fi.Mode().IsRegular() {
			uncertain = true
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			uncertain = true
			continue
		}
		if word.Match(data) {
			return verifyResult{"ok", "symbol " + c.value + " in " + path}
		}
		checked = path
	}
	if checked != "" && !uncertain {
		return verifyResult{"stale", "no " + c.value + " in existing file scope " + c.scope + " in " + v.store.repo}
	}
	return verifyResult{"unverifiable", "symbol file is missing, outside the repository, or unreadable: " + c.scope}
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
		branch := v.defaultBranch(repo)
		if branch == "" {
			if result.status != "stale" {
				result = verifyResult{"unverifiable", "origin/HEAD and origin/main unavailable locally in " + repo + "; no fetch attempted"}
			}
			continue
		}
		_, err := verifyGit(repo, "", "merge-base", "--is-ancestor", sha, branch)
		evidence := sha + " is an ancestor of local " + strings.TrimPrefix(branch, "refs/remotes/") + " in " + repo
		if err == nil {
			result = verifyResult{"ok", evidence}
			break
		}
		var exit *exec.ExitError
		if remotes, _ := verifyGit(repo, "", "branch", "-r", "--contains", sha); strings.TrimSpace(remotes) != "" {
			result = verifyResult{"ok", sha + " is on remote branch " + strings.Fields(remotes)[0] + " in " + repo}
			break
		}
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			result = verifyResult{"stale", "pushed claim: " + sha + " is not an ancestor of local " + strings.TrimPrefix(branch, "refs/remotes/") + " in " + repo}
		} else if result.status != "stale" {
			result = verifyResult{"unverifiable", "cannot check pushed ancestry in " + repo}
		}
	}
	v.pushedResults[token] = result
	return result
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
