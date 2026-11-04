package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
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
	counts, err := v.report(w, stores, claims)
	if err != nil {
		return err
	}
	return writeVerifySummary(w, counts)
}

// report checks every store's claims and writes one line per claim, returning the status tallies.
func (v *claimVerifier) report(w io.Writer, stores []verifyStore, claims [][]verifyClaim) (map[string]int, error) {
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
				return nil, err
			}
		}
	}
	return counts, nil
}

// writeVerifySummary writes the final tally line with the share of claims that verified.
func writeVerifySummary(w io.Writer, counts map[string]int) error {
	total := counts["ok"] + counts["stale"] + counts["unverifiable"]
	share := 0.0
	if total > 0 {
		share = 100 * float64(counts["ok"]) / float64(total)
	}
	_, err := fmt.Fprintf(w, "summary: ok=%d stale=%d unverifiable=%d total=%d ok_share=%.2f%%\n",
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
	maps.Copy(out, catalogOrigins(string(data)))
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

// catalogOrigins reads the `<!-- origin: {...} -->` rows of the store catalog, keyed by store name.
func catalogOrigins(catalog string) map[string]memstore.Origin {
	out := map[string]memstore.Origin{}
	for line := range strings.SplitSeq(catalog, "\n") {
		key, rest, ok := strings.Cut(strings.TrimPrefix(line, "- "), " — ")
		_, raw, found := strings.CutLast(rest, "<!-- origin: ")
		var origin memstore.Origin
		if ok && found && bank.ValidBareName(key) == nil && json.Unmarshal([]byte(strings.TrimSuffix(raw, " -->")), &origin) == nil {
			out[key] = origin
		}
	}
	return out
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
	best, home := bestStore(stores, cwd, root, inStore)
	if best < 0 && (root == "" || inStore) {
		best = home
	}
	if best < 0 {
		return nil, fmt.Errorf("no memory store for %s", cwd)
	}
	return []verifyStore{stores[best]}, nil
}

// bestStore returns the index of the store whose dir, repo or alias is the longest match for cwd (or its checkout
// root), and the index of the home store; both are -1 when absent. Inside the memory repo only store dirs match.
func bestStore(stores []verifyStore, cwd, root string, inStore bool) (best, home int) {
	best, home = -1, -1
	length := 0
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
	return best, home
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
		found, err := fileClaims(store, path)
		claims = append(claims, found...)
		return err
	})
	return claims, err
}

// fileClaims extracts the claims of one bank or topic markdown file; other kinds yield none.
func fileClaims(store verifyStore, path string) ([]verifyClaim, error) {
	kind := bank.ClassifyMemoryFile(filepath.Dir(path), filepath.Base(path))
	if kind != bank.KindBank && kind != bank.KindTopic {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(memstore.StoreDir(), path)
	active := filepath.Dir(path) == store.dir && verifyActive(filepath.Base(path))
	var claims []verifyClaim
	for _, block := range verifyBlocks(filepath.Base(path), string(data), kind) {
		id := bank.BlockID(block)
		if id == "" {
			id = bank.ContentRef(block)
		}
		claims = append(claims, verifyExtract(filepath.ToSlash(rel), id, block, active)...)
	}
	return claims, nil
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
	tokens, needFiles := v.bareSymbols(claims)
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
		v.markSymbolsIn(file)
	}
}

// bareSymbols registers each unscoped symbol claim as unverifiable-until-grepped and reports whether any scoped
// claim names a bare file (so the repo's Go file list is needed).
func (v *claimVerifier) bareSymbols(claims []verifyClaim) (tokens []string, needFiles bool) {
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
	return tokens, needFiles
}

// markSymbolsIn marks every pending bare symbol that occurs as a whole word in the repo file as ok.
func (v *claimVerifier) markSymbolsIn(file string) {
	path, ok := v.scopedPath(file)
	if !ok {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, word := range verifyWordRE.FindAll(data, -1) {
		if result, ok := v.symbols[string(word)]; ok && result.status != "ok" {
			v.symbols[string(word)] = verifyResult{"ok", "git grep -w: " + file}
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

// verifyObjectTypes are the git object types a cat-file --batch-check reply may name.
var verifyObjectTypes = map[string]bool{"commit": true, "tree": true, "blob": true, "tag": true}

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
		if i < len(tokens) && len(fields) == 2 && verifyHexRE.MatchString(fields[0]) && verifyObjectTypes[fields[1]] {
			out[tokens[i]] = fields[0]
		}
	}
	return out
}
