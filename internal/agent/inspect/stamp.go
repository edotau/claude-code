package inspect

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

const GoStampGates = "build,test,vet"
const DefaultStampGates = "build,complexity,gofmt,golangci-lint,quality,test,vet"
const StampTTL = 30 * time.Minute

// StampMeta describes a successful check, never a promise to check later.
type StampMeta struct {
	Passed   bool      `json:"passed"`
	Created  time.Time `json:"created"`
	Gates    string    `json:"gates"`
	Packages []string  `json:"packages"`
}

type stampTree struct {
	names   []string
	read    func(string) ([]byte, error)
	byDir   map[string][]string
	modules map[string]bool
	env     map[string]string
}

func (t *stampTree) index() {
	t.byDir, t.modules = map[string][]string{}, map[string]bool{}
	for _, name := range t.names {
		dir := path.Dir(name)
		t.byDir[dir] = append(t.byDir[dir], name)
		if path.Base(name) == "go.mod" {
			t.modules[dir] = true
		}
	}
}

// ComputeStampKey hashes contents, not HEAD, index state, timestamps or absolute checkout paths.
func ComputeStampKey(dir string, pkgs []string, gates string) (string, error) {
	env, err := effectiveStampEnv(dir)
	if err != nil {
		return "", err
	}
	tree := stampTree{env: env, read: func(p string) ([]byte, error) {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("stamp: not a regular file: %s", p)
		}
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
	}}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && stampSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		tree.names = append(tree.names, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		return "", err
	}
	return tree.key(pkgs, gates)
}

func stampSkipDir(name string) bool {
	return name == ".git"
}

// ComputeCommitStampKey reads committed blobs through one cat-file process without acquiring a worktree.
func ComputeCommitStampKey(ctx context.Context, dir, sha string, pkgs []string, gates string) (string, error) {
	if len(sha) < 7 || len(sha) > 64 || strings.Trim(sha, "0123456789abcdefABCDEF") != "" {
		return "", errors.New("stamp: expected a commit object id")
	}
	raw, err := proc.RunGit(ctx, dir, "ls-tree", "-rz", "--full-tree", sha)
	if err != nil {
		return "", err
	}
	objects, names := map[string]string{}, []string{}
	for row := range strings.SplitSeq(raw, "\x00") {
		head, name, ok := strings.Cut(row, "\t")
		fields := strings.Fields(head)
		if ok && len(fields) == 3 {
			objects[name] = fields[0] + " " + fields[2]
			names = append(names, name)
		}
	}
	read, closeReader, err := stampBlobReader(ctx, dir, objects)
	if err != nil {
		return "", err
	}
	defer closeReader()
	env, err := effectiveStampEnv(dir)
	if err != nil {
		return "", err
	}
	return (stampTree{names: names, read: read, env: env}).key(pkgs, gates)
}

func stampBlobReader(ctx context.Context, dir string, objects map[string]string) (func(string) ([]byte, error), func(), error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir, cmd.Env = dir, proc.GitEnv()
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, nil, err
	}
	r := bufio.NewReader(out)
	read := func(p string) ([]byte, error) {
		obj, ok := objects[p]
		if !ok {
			return nil, fs.ErrNotExist
		}
		mode, oid, _ := strings.Cut(obj, " ")
		if mode != "100644" && mode != "100755" {
			return nil, fmt.Errorf("stamp: not a regular blob: %s", p)
		}
		if _, err := fmt.Fprintln(in, oid); err != nil {
			return nil, err
		}
		return readStampBlob(r)
	}
	return read, func() { _ = in.Close(); _ = out.Close(); _ = cmd.Wait() }, nil
}

func readStampBlob(r *bufio.Reader) ([]byte, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	f := strings.Fields(header)
	if len(f) != 3 || f[1] != "blob" {
		return nil, fmt.Errorf("stamp: invalid blob header %q", header)
	}
	n, err := strconv.Atoi(f[2])
	if err != nil || n < 0 {
		return nil, errors.New("stamp: invalid blob size")
	}
	b := make([]byte, n+1)
	_, err = io.ReadFull(r, b)
	return b[:n], err
}

func (t stampTree) key(pkgs []string, gates string) (string, error) {
	t.index()
	mod, err := t.read("go.mod")
	if err != nil {
		return "", err
	}
	module, err := stampModule(mod)
	if err != nil {
		return "", err
	}
	selected, err := t.packages(pkgs)
	if err != nil || len(selected) == 0 {
		return "", errors.Join(err, errors.New("stamp: no Go packages"))
	}
	files := map[string][]byte{"go.mod": mod}
	if err := t.collectConfig(files); err != nil {
		return "", err
	}
	if err := t.collectValidation(selected, module, gates, files); err != nil {
		return "", err
	}
	return stampDigest(files, selected, gates, t.env), nil
}

// A diff proof also waives its whole-module build, so hash that scope while retaining the tested package identity.
func (t stampTree) collectValidation(selected []string, module, gates string, files map[string][]byte) error {
	pkgs := selected
	if strings.Contains(gates, "\x00diff-v1\x00") {
		var err error
		pkgs, err = t.packages([]string{"./..."})
		if err != nil {
			return err
		}
	}
	return t.collect(pkgs, module, files)
}

var stampEnvNames = []string{
	"GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED", "CGO_CFLAGS", "CGO_LDFLAGS", "GOEXPERIMENT", "GOWORK",
	"CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_FFLAGS", "CC", "CXX", "GOAMD64", "GOARM", "GOARM64", "GOMIPS", "GOMIPS64",
	"GOPPC64", "GORISCV64", "GOWASM", "GO386", "RUNNER_GOFLAGS",
}

var stampEnvs sync.Map

// Cache the effective Go environment per executable and checkout, not the compiler that built this harness.
func effectiveStampEnv(dir string) (map[string]string, error) {
	bin, err := exec.LookPath("go")
	if err != nil {
		return nil, err
	}
	key := struct{ bin, dir, env, revision string }{
		bin, paths.CanonDeep(dir), strings.Join(os.Environ(), "\x00"), stampEnvRevision(bin, dir),
	}
	load := sync.OnceValues(func() (map[string]string, error) { return readStampEnv(bin, dir) })
	cached, _ := stampEnvs.LoadOrStore(key, load)
	return cached.(func() (map[string]string, error))()
}

// Invalidate the memo when Go's persisted settings, tool binary or workspace discovery inputs change.
func stampEnvRevision(bin, dir string) string {
	h := sha256.New()
	if info, err := os.Stat(bin); err == nil {
		fmt.Fprintf(h, "%d:%d\x00", info.Size(), info.ModTime().UnixNano())
	}
	config := os.Getenv("GOENV")
	if config == "" {
		base, _ := os.UserConfigDir()
		config = filepath.Join(base, "go", "env")
	}
	b, _ := os.ReadFile(config)
	_, _ = h.Write(b)
	b, _ = os.ReadFile(filepath.Join(dir, "go.mod"))
	_, _ = h.Write(b)
	for d := paths.CanonDeep(dir); ; d = filepath.Dir(d) {
		b, _ := os.ReadFile(filepath.Join(d, "go.work"))
		fmt.Fprintf(h, "%s\x00%x\x00", d, sha256.Sum256(b))
		if filepath.Dir(d) == d {
			break
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readStampEnv(bin, dir string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "env", "-json")
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("stamp: go env: %w", err)
	}
	var env map[string]string
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	if env["GOWORK"] != "" && env["GOWORK"] != "off" {
		return nil, errors.New("stamp: workspace mode requires independent validation")
	}
	if env["GOVERSION"] == "" {
		return nil, errors.New("stamp: missing effective Go toolchain version")
	}
	env["GOWORK"], env["GOFLAGS"] = "", stampGoFlags(env["GOFLAGS"])
	// Runners add GOFLAGS explicitly, so persisted flags and process flags must not share a proof.
	env["RUNNER_GOFLAGS"] = stampGoFlags(os.Getenv("GOFLAGS"))
	return env, nil
}

func (t stampTree) collectConfig(files map[string][]byte) error {
	for _, p := range []string{"go.sum", "vendor/modules.txt", ".golangci.yml", ".golangci.yaml",
		".golangci.toml", ".golangci.json", "Makefile"} {
		b, err := t.read(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		files[p] = b
	}
	return nil
}

// Diff stamps bind package contents to the exact checked files and the comparison tree, unlike Go command proofs.
func ComputeDiffStampKey(ctx context.Context, dir, base string, pkgs, files []string) (string, error) {
	gates, err := diffStampGates(ctx, dir, base, files)
	if err != nil {
		return "", err
	}
	return ComputeStampKey(dir, pkgs, gates)
}

func ComputeCommitDiffStampKey(ctx context.Context, dir, sha string, pkgs, files []string) (string, error) {
	gates, err := diffStampGates(ctx, dir, sha+"^", files)
	if err != nil {
		return "", err
	}
	return ComputeCommitStampKey(ctx, dir, sha, pkgs, gates)
}

func diffStampGates(ctx context.Context, dir, base string, files []string) (string, error) {
	if base == "" || strings.HasPrefix(base, "-") {
		return "", errors.New("stamp: missing comparison base")
	}
	tree, err := proc.RunGit(ctx, dir, "rev-parse", "--verify", base+"^{tree}")
	if err != nil {
		return "", err
	}
	names := slices.Clone(files)
	slices.Sort(names)
	return fmt.Sprintf("%s\x00diff-v1\x00%s\x00%q", DefaultStampGates, tree, slices.Compact(names)), nil
}

func stampDigest(files map[string][]byte, selected []string, gates string, env map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "verify-v2\x00%s\x00%q\x00", gates, selected)
	for _, name := range stampEnvNames {
		fmt.Fprintf(h, "%s=%s\x00", name, env[name])
	}
	names := make([]string, 0, len(files))
	for p := range files {
		names = append(names, p)
	}
	slices.Sort(names)
	for _, p := range names {
		fmt.Fprintf(h, "%s\x00%x\x00", p, sha256.Sum256(files[p]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// The runners add these cache-stabilizing flags themselves; they do not distinguish a validation pass.
func stampGoFlags(value string) string {
	flags := slices.DeleteFunc(strings.Fields(value), func(f string) bool { return f == "-trimpath" || f == "-buildvcs=false" })
	return strings.Join(flags, " ")
}

func stampModule(mod []byte) (string, error) {
	module := ""
	for line := range strings.SplitSeq(string(mod), "\n") {
		f := strings.Fields(strings.SplitN(line, "//", 2)[0])
		if len(f) >= 2 && f[0] == "module" {
			module = strings.Trim(f[1], "\"")
		}
		for i, word := range f {
			if word != "=>" || i+1 == len(f) {
				continue
			}
			target := strings.Trim(f[i+1], "\"")
			if strings.HasPrefix(target, ".") || filepath.IsAbs(target) {
				return "", errors.New("stamp: local replacements require independent validation")
			}
		}
	}
	if module == "" {
		return "", errors.New("stamp: missing module directive")
	}
	return module, nil
}

func (t stampTree) packages(pkgs []string) ([]string, error) {
	var out []string
	for _, p := range pkgs {
		matches, err := t.packageMatches(p)
		if err != nil {
			return nil, err
		}
		out = append(out, matches...)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func (t stampTree) packageMatches(pattern string) ([]string, error) {
	p := strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	recursive := p == "..." || strings.HasSuffix(p, "/...")
	p = path.Clean(strings.TrimSuffix(strings.TrimSuffix(p, "..."), "/"))
	if !fs.ValidPath(p) {
		return nil, fmt.Errorf("stamp: invalid package %q", p)
	}
	var out []string
	for d := range t.byDir {
		if t.matchesPackage(d, p, recursive) {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("stamp: package %s has no Go files", p)
	}
	return out, nil
}

func (t stampTree) matchesPackage(dir, prefix string, recursive bool) bool {
	if recursive && !t.walkPackage(dir) {
		return false
	}
	within := dir == prefix || recursive && (prefix == "." || strings.HasPrefix(dir, prefix+"/"))
	return within && t.hasPackage(dir)
}

func (t stampTree) walkPackage(dir string) bool {
	if dir == "." {
		return true
	}
	for part := range strings.SplitSeq(dir, "/") {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") || part == "testdata" || part == "vendor" {
			return false
		}
	}
	for d := dir; d != "."; d = path.Dir(d) {
		if t.modules[d] {
			return false
		}
	}
	return true
}

func (t stampTree) collect(pkgs []string, module string, files map[string][]byte) error {
	seen := map[string]bool{}
	queue := slices.Clone(pkgs)
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if seen[d] {
			continue
		}
		seen[d] = true
		deps, err := t.collectPackage(d, module, files)
		if err != nil {
			return err
		}
		queue = append(queue, deps...)
	}
	return nil
}

func (t stampTree) collectPackage(dir, module string, files map[string][]byte) ([]string, error) {
	if !t.hasPackage(dir) {
		return nil, fmt.Errorf("stamp: local dependency %s has no Go files", dir)
	}
	var deps, embeds []string
	for _, name := range t.byDir[dir] {
		if !stampBuildFile(name) {
			continue
		}
		b, err := t.read(name)
		if err != nil {
			return nil, err
		}
		files[name] = b
		imports, patterns, err := t.sourceInputs(name, module, b)
		if err != nil {
			return nil, err
		}
		deps, embeds = append(deps, imports...), append(embeds, patterns...)
	}
	embeds = append(embeds, "testdata")
	return deps, t.collectEmbedded(dir, embeds, files)
}

func (t stampTree) sourceInputs(name, module string, b []byte) ([]string, []string, error) {
	if !strings.HasSuffix(name, ".go") {
		return nil, nil, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), name, b, parser.ImportsOnly)
	if err != nil {
		return nil, nil, err
	}
	var deps []string
	for _, imp := range f.Imports {
		i, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, nil, err
		}
		if i == module || strings.HasPrefix(i, module+"/") {
			deps = append(deps, path.Clean(strings.TrimPrefix(strings.TrimPrefix(i, module), "/")))
		} else if t.hasPackage("vendor/" + i) {
			deps = append(deps, "vendor/"+i)
		}
	}
	patterns, err := stampEmbeds(string(b))
	return deps, patterns, err
}

func (t stampTree) hasPackage(dir string) bool {
	return slices.ContainsFunc(t.byDir[dir], func(p string) bool { return strings.HasSuffix(p, ".go") })
}

func stampBuildFile(p string) bool {
	return slices.Contains([]string{".go", ".s", ".S", ".c", ".h", ".cc", ".cpp", ".cxx", ".m", ".mm", ".f", ".F", ".syso"}, path.Ext(p))
}

func stampEmbeds(body string) ([]string, error) {
	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		rest, ok := strings.CutPrefix(line, "//go:embed")
		if !ok || rest == "" || rest[0] != ' ' && rest[0] != '\t' {
			continue
		}
		for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimSpace(rest) {
			pattern, tail, err := stampEmbedPattern(rest)
			if err != nil {
				return nil, err
			}
			out, rest = append(out, pattern), tail
		}
	}
	return out, nil
}

func stampEmbedPattern(rest string) (string, string, error) {
	pattern := strings.Fields(rest)[0]
	n := len(pattern)
	if rest[0] == '"' || rest[0] == '`' {
		quoted, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return "", "", err
		}
		pattern, err = strconv.Unquote(quoted)
		if err != nil {
			return "", "", err
		}
		n = len(quoted)
	}
	pattern = strings.TrimPrefix(pattern, "all:")
	if !fs.ValidPath(pattern) {
		return "", "", fmt.Errorf("stamp: invalid embed pattern %q", pattern)
	}
	return pattern, rest[n:], nil
}

// Hash directory embeds recursively, conservatively including hidden files as well as all: matches.
func (t stampTree) collectEmbedded(dir string, patterns []string, files map[string][]byte) error {
	for _, p := range t.names {
		rel := p
		if dir != "." {
			var within bool
			rel, within = strings.CutPrefix(p, dir+"/")
			if !within {
				continue
			}
		}
		match, err := stampEmbedMatch(rel, patterns)
		if err != nil {
			return err
		}
		if match {
			b, err := t.read(p)
			if err != nil {
				return err
			}
			files[p] = b
		}
	}
	return nil
}

func stampEmbedMatch(rel string, patterns []string) (bool, error) {
	for _, pattern := range patterns {
		for name := rel; name != "."; name = path.Dir(name) {
			match, err := path.Match(pattern, name)
			if match || err != nil {
				return match, err
			}
		}
	}
	return false, nil
}

func stampPath(key string) string {
	b, err := hex.DecodeString(key)
	if err != nil || len(b) != sha256.Size {
		return ""
	}
	return filepath.Join(paths.StateDir(), "verify", "stamps", key+".json")
}

func WriteStamp(key string, meta StampMeta) error {
	p := stampPath(key)
	if p == "" || !meta.Passed {
		return errors.New("stamp: invalid key or unsuccessful check")
	}
	if meta.Created.IsZero() {
		meta.Created = time.Now()
	}
	if err := paths.WriteJSONNoSync(p, meta); err != nil {
		return err
	}
	pruneStamps(filepath.Dir(p))
	return nil
}

func HasFreshStamp(key string, maxAge time.Duration) (StampMeta, bool) {
	var meta StampMeta
	p := stampPath(key)
	if p == "" {
		return meta, false
	}
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &meta) != nil || !meta.Passed {
		return meta, false
	}
	if maxAge <= 0 {
		maxAge = StampTTL
	}
	age := time.Since(meta.Created)
	return meta, age >= 0 && age <= maxAge
}

// FreshPackageStamps accepts an exact union stamp or independently checked stamps for every requested package.
func FreshPackageStamps(pkgs []string, compute func([]string) (string, error)) bool {
	fresh := func(scope []string) bool {
		key, err := compute(scope)
		if err != nil {
			return false
		}
		meta, ok := HasFreshStamp(key, StampTTL)
		return ok && meta.Gates == GoStampGates
	}
	if len(pkgs) == 0 {
		return false
	}
	if fresh(pkgs) {
		return true
	}
	if len(pkgs) == 1 {
		return false
	}
	for _, p := range pkgs {
		if !fresh([]string{p}) {
			return false
		}
	}
	return true
}

func pruneStamps(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// PendingTask records the checkout as well as the paths; a batch must never validate another lane's contents.
type PendingCheck struct {
	Command   string `json:"command"`
	Generated bool   `json:"generated,omitempty"`
}

type PendingTask struct {
	Revision uint64         `json:"revision"`
	Checks   []PendingCheck `json:"checks,omitempty"`
	Dir      string         `json:"dir"`
	Task     string         `json:"task"`
	Files    []string       `json:"files"`
}

type PendingVerify struct {
	Revision uint64        `json:"revision"`
	Tasks    []PendingTask `json:"tasks"`
}

func pendingPath(session string) (string, error) {
	if session == "" || session == "." || session == ".." || strings.ContainsAny(session, "/\\\x00") {
		return "", errors.New("batch verification needs a valid session id")
	}
	return filepath.Join(paths.StateDir(), "verify", "pending", session+".json"), nil
}

func ReadPending(session string) (PendingVerify, error) {
	p, err := pendingPath(session)
	if err != nil {
		return PendingVerify{}, err
	}
	return readPendingFile(p)
}

func readPendingFile(p string) (q PendingVerify, err error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return q, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &q)
	}
	return q, err
}

func QueuePending(session string, task PendingTask) error {
	p, err := pendingPath(session)
	if err != nil {
		return err
	}
	if root, err := repoRoot(task.Dir); err == nil {
		task.Dir = root
	}
	task.Dir = paths.CanonDeep(task.Dir)
	task.Files = slices.Clone(task.Files)
	task.Checks = slices.Clone(task.Checks)
	return paths.WithFileLockWithin(p, 3*time.Millisecond, func() error {
		q, err := readPendingFile(p)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(q.Tasks, func(t PendingTask) bool { return t.Dir == task.Dir && t.Task == task.Task })
		if i < 0 {
			q.Tasks = append(q.Tasks, task)
			i = len(q.Tasks) - 1
		} else {
			q.Tasks[i].Files = append(q.Tasks[i].Files, task.Files...)
			q.Tasks[i].Checks = append(q.Tasks[i].Checks, task.Checks...)
		}
		slices.Sort(q.Tasks[i].Files)
		q.Tasks[i].Files = slices.Compact(q.Tasks[i].Files)
		q.Revision = max(q.Revision+1, uint64(time.Now().UnixNano()))
		q.Tasks[i].Revision = q.Revision
		return paths.WriteJSONNoSync(p, q)
	})
}

// ClearPending preserves arrivals during validation, including a second handback of the same paths.
func ClearPending(session string, checked PendingVerify) error {
	p, err := pendingPath(session)
	if err != nil {
		return err
	}
	return paths.WithFileLockWithin(p, 3*time.Millisecond, func() error {
		q, err := readPendingFile(p)
		if err != nil {
			return err
		}
		q.Tasks = slices.DeleteFunc(q.Tasks, func(task PendingTask) bool {
			return slices.ContainsFunc(checked.Tasks, func(done PendingTask) bool {
				return task.Dir == done.Dir && task.Task == done.Task && task.Revision == done.Revision
			})
		})
		if len(q.Tasks) > 0 {
			return paths.WriteJSONNoSync(p, q)
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}
