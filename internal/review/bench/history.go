package bench

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	historySchema = 1
	quietWindow   = 5
	idleAge       = 30 * 24 * time.Hour
)

type observation struct {
	Schema         int       `json:"schema"`
	At             time.Time `json:"at"`
	Pkg            string    `json:"pkg"`
	Benchmark      string    `json:"benchmark"`
	Epoch          string    `json:"epoch"`
	Env            Env       `json:"env"`
	Score          *Score    `json:"score,omitempty"`
	Compared       bool      `json:"compared,omitempty"`
	Decisive       bool      `json:"decisive,omitempty"`
	ProductionPath string    `json:"production_path,omitempty"`
	Subject        string    `json:"subject,omitempty"`
	Repo           string    `json:"repo,omitempty"`
}

// repoIdentity is the repository top-level path, symlink-resolved and cleaned; unresolvable stays as given, cleaned.
func repoIdentity(top string) string {
	if top == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(top); err == nil {
		top = real
	}
	return filepath.Clean(top)
}

var historyFormat = schemas.Register(schemas.Format{
	Name: "bench-history", Owner: "internal/review/bench", Kind: schemas.Log, Version: historySchema,
	Glob: func() string { return filepath.Join(benchRoot(), "*", "history.jsonl") },
	New:  func() any { return new(observation) },
	Keep: "benchmark evidence and epochs; deletion requires owner approval",
})

// Verdict reports evidence only; no verdict authorizes a deletion.
type Verdict struct {
	Pkg       string `json:"pkg"`
	Benchmark string `json:"benchmark"`
	Epoch     string `json:"epoch"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
}

func readHistory(file string) ([]observation, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []observation
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for line := 1; sc.Scan(); line++ {
		var row observation
		if _, err := schemas.Decode(historyFormat, sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", file, line, err)
		}
		if row.At.IsZero() || row.Pkg == "" || row.Benchmark == "" || !validEpoch(row.Epoch) {
			return nil, fmt.Errorf("%s:%d: incomplete benchmark evidence", file, line)
		}
		out = append(out, row)
	}
	return out, sc.Err()
}

func appendHistory(dir string, rows []observation) error {
	var data strings.Builder
	enc := json.NewEncoder(&data)
	for _, row := range rows {
		row.Schema = historySchema
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return paths.AppendLine(filepath.Join(dir, "history.jsonl"), []byte(data.String()))
}

// recordRun holds history.lock (not AppendLine's history.jsonl.lock) so concurrent runs see each other's rows.
func recordRun(dir string, rep Report, res PkgResult, at time.Time) (verdicts []Verdict, err error) {
	err = paths.WithFileLock(filepath.Join(dir, "history"), func() error {
		verdicts, err = recordRunLocked(dir, rep, res, at)
		return err
	})
	return verdicts, err
}

func recordRunLocked(dir string, rep Report, res PkgResult, at time.Time) ([]Verdict, error) {
	history, err := readHistory(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		return nil, err
	}
	var rows []observation
	for _, s := range res.Benchmarks {
		if !validEpoch(s.Epoch) {
			return nil, fmt.Errorf("%s: benchmark source was not found", s.Name)
		}
		rows = append(rows, observation{At: at, Pkg: res.Pkg, Benchmark: s.Name, Epoch: s.Epoch,
			Env: rep.Env, Score: &s, Compared: len(s.Deltas) > 0, Decisive: moved(s) && isQuiet(s),
			Repo: repoIdentity(cmp.Or(res.repo, res.top))})
	}
	if err := recordEpochChanges(history, rows, rep.Commit); err != nil {
		return nil, err
	}
	if err := appendHistory(dir, rows); err != nil {
		return nil, err
	}
	return evaluateHistory(slices.Concat(history, rows), at), nil
}

func recordEpochChanges(history, rows []observation, sha string) error {
	last := map[string]string{}
	files := map[string]string{}
	for _, row := range history {
		last[row.Benchmark] = row.Epoch
		if row.Score != nil {
			files[row.Score.Source] = row.Epoch
		}
	}
	for _, row := range rows {
		old := last[row.Benchmark]
		if old == "" && row.Score != nil {
			old = files[row.Score.Source]
		}
		if old == "" || old == row.Epoch {
			continue
		}
		data, err := json.Marshal(scorecardRow{At: row.At, Pkg: row.Pkg, Benchmark: row.Benchmark, Epoch: row.Epoch,
			OldEpoch: old, Verdict: "bench-change", What: "benchmark file changed", Sha: sha})
		if err != nil {
			return err
		}
		if err := paths.AppendLine(filepath.Join(benchRoot(), "scorecard.jsonl"), data); err != nil {
			return err
		}
	}
	return nil
}

func validateRecord(l loopOpts) error {
	if l.pkg == "" || l.what == "" || !slices.Contains([]string{"kept", "reverted", "bench-change"}, l.verdict) {
		return errors.New("--record needs --pkg, --what and --verdict kept|reverted|bench-change")
	}
	if !filepath.IsLocal(l.pkg) || strings.Contains(l.pkg, "\\") {
		return errors.New("--pkg must be a local package path")
	}
	return nil
}

func validateEvidence(l loopOpts) error {
	if err := validateRecord(l); err != nil {
		return err
	}
	if !slices.Contains([]string{"", "on", "off"}, l.productionPath) ||
		!slices.Contains([]string{"", "present", "gone"}, l.subject) {
		return errors.New("--production-path must be on|off; --subject must be present|gone")
	}
	if l.benchmark+l.epoch+l.oldEpoch+l.productionPath+l.subject == "" &&
		l.verdict != "bench-change" {
		return nil
	}
	if !strings.HasPrefix(l.benchmark, "Benchmark") || !validEpoch(l.epoch) {
		return errors.New("benchmark evidence needs --benchmark and a lowercase SHA-256 --epoch")
	}
	return validateEpochChange(l)
}

func validateEpochChange(l loopOpts) error {
	if l.verdict == "bench-change" && (!validEpoch(l.oldEpoch) || l.oldEpoch == l.epoch) {
		return errors.New("bench-change needs distinct --old-epoch and --epoch hashes")
	}
	if l.oldEpoch != "" && l.verdict != "bench-change" {
		return errors.New("--old-epoch requires --verdict bench-change")
	}
	return nil
}

func recordEvidence(l loopOpts, at time.Time) error {
	if l.benchmark == "" {
		return nil
	}
	return appendHistory(filepath.Join(benchRoot(), pkgDirName(l.pkg)), []observation{{
		At: at, Pkg: l.pkg, Benchmark: l.benchmark, Epoch: l.epoch,
		Decisive:       l.verdict == "kept" || l.verdict == "reverted",
		ProductionPath: l.productionPath, Subject: l.subject,
	}})
}

func runVerdicts(l loopOpts, asJSON bool, w io.Writer) error {
	files, err := filepath.Glob(filepath.Join(benchRoot(), "*", "history.jsonl"))
	if err != nil {
		return err
	}
	var rows []observation
	for _, file := range files {
		history, err := readHistory(file)
		if err != nil {
			return err
		}
		for _, row := range history {
			if l.pkg == "" || row.Pkg == l.pkg {
				rows = append(rows, row)
			}
		}
	}
	verdicts := evaluateHistory(rows, time.Now().UTC())
	if asJSON {
		return json.NewEncoder(w).Encode(verdicts)
	}
	for _, v := range verdicts {
		fmt.Fprintf(w, "%s %s: %s (%s; gates only)\n", v.Pkg, v.Benchmark, v.Verdict, v.Reason)
	}
	return nil
}

type benchmarkEpoch struct {
	File string `json:"file"`
	Hash string `json:"hash"`
}

const epochPrefix = "# bench-epochs: "

func benchmarkFunc(name string) string {
	root, _, _ := strings.Cut(name, "/")
	return procSuffix.ReplaceAllString(root, "")
}

type testFile struct {
	pkg   string
	rel   string
	body  []byte
	decls []*ast.FuncDecl
	other []ast.Decl
}

func benchmarkEpochs(top, pkg string) (map[string]benchmarkEpoch, error) {
	dir := filepath.Join(top, pkg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []*testFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		tf, err := loadTestFile(top, filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if tf != nil {
			files = append(files, tf)
		}
	}
	return epochsOf(files), nil
}

func loadTestFile(top, file string) (*testFile, error) {
	active, err := build.Default.MatchFile(filepath.Dir(file), filepath.Base(file))
	if err != nil || !active {
		return nil, err
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, body, 0)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(top, file)
	if err != nil {
		return nil, err
	}
	// CRLF folds to LF so an autocrlf checkout keeps the epoch.
	tf := &testFile{pkg: f.Name.Name, rel: filepath.ToSlash(rel), body: bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			tf.decls = append(tf.decls, fn)
		} else {
			tf.other = append(tf.other, decl)
		}
	}
	return tf, nil
}

// declNode is a top-level declaration; the empty name marks code that runs before every benchmark.
type declNode struct {
	file  *testFile
	refs  map[string]bool
	names []string
}

// namespaces lists the packages whose test files a name resolves in; p_test also sees p.
func namespaces(pkg, name string) []string {
	base, ok := strings.CutSuffix(pkg, "_test")
	switch {
	case name == "": // both test packages link into one binary, so their setup runs for every benchmark
		return []string{base, base + "_test"}
	case ok:
		return []string{pkg, base}
	}
	return []string{pkg}
}

// epochsOf hashes each benchmark's declaring file plus the test files that define what it reaches by name.
func epochsOf(files []*testFile) map[string]benchmarkEpoch {
	defs := map[string][]*declNode{}
	types := localTypes(files)
	for _, tf := range files {
		for _, node := range fileNodes(tf, types) {
			for _, name := range node.names {
				defs[tf.pkg+"\x00"+name] = append(defs[tf.pkg+"\x00"+name], node)
			}
		}
	}
	out := map[string]benchmarkEpoch{}
	for _, tf := range files {
		for _, fn := range tf.decls {
			if fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Benchmark") {
				out[fn.Name.Name] = benchmarkEpoch{File: tf.rel, Hash: reachHash(tf, fn, defs)}
			}
		}
	}
	return out
}

func fileNodes(tf *testFile, types map[string]bool) []*declNode {
	var out []*declNode
	for _, fn := range tf.decls {
		node := &declNode{file: tf, refs: identNames(fn), names: []string{fn.Name.Name}}
		if recv := recvTypeName(fn); recv != "" {
			node.names = append(node.names, recv)
		}
		if fn.Recv == nil && (fn.Name.Name == "init" || fn.Name.Name == "TestMain") {
			node.names = append(node.names, "")
		}
		out = append(out, node)
	}
	for _, decl := range tf.other {
		out = append(out, specNodes(tf, decl, types)...)
	}
	return out
}

func reachHash(decl *testFile, bench *ast.FuncDecl, defs map[string][]*declNode) string {
	seen := map[string]bool{}
	reached := map[*testFile]bool{}
	queue := append(slices.Collect(maps.Keys(identNames(bench))), "")
	for len(queue) > 0 {
		name := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, pkg := range namespaces(decl.pkg, name) {
			for _, node := range defs[pkg+"\x00"+name] {
				reached[node.file] = true
				queue = append(queue, slices.Collect(maps.Keys(node.refs))...)
			}
		}
	}
	delete(reached, decl)
	h := sha256.New()
	h.Write(decl.body)
	extra := slices.Collect(maps.Keys(reached))
	slices.SortFunc(extra, func(a, b *testFile) int { return cmp.Compare(a.rel, b.rel) })
	for _, tf := range extra {
		h.Write([]byte("\x00" + tf.rel + "\x00"))
		h.Write(tf.body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hasCall reports a function call that runs at init: not a conversion, not inside a function literal.
func hasCall(e ast.Expr, types map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			found = found || !isConversion(v.Fun, types)
		}
		return !found
	})
	return found
}

func isConversion(fun ast.Expr, types map[string]bool) bool {
	switch f := fun.(type) {
	case *ast.ParenExpr:
		return isConversion(f.X, types)
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StarExpr:
		return true
	case *ast.Ident:
		return types[f.Name]
	}
	return false
}

// localTypes holds the predeclared types and every type the test files declare, by name.
func localTypes(files []*testFile) map[string]bool {
	out := map[string]bool{}
	for name := range strings.FieldsSeq("bool byte rune string error any int int8 int16 int32 int64 uint uint8 uint16 " +
		"uint32 uint64 uintptr float32 float64 complex64 complex128") {
		out[name] = true
	}
	for _, tf := range files {
		for _, decl := range tf.other {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
				for _, spec := range gen.Specs {
					out[spec.(*ast.TypeSpec).Name.Name] = true
				}
			}
		}
	}
	return out
}

func identNames(n ast.Node) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && id.Name != "_" {
			out[id.Name] = true
		}
		return true
	})
	return out
}

func recvTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	for {
		switch v := t.(type) {
		case *ast.StarExpr:
			t = v.X
		case *ast.IndexExpr:
			t = v.X
		case *ast.IndexListExpr:
			t = v.X
		case *ast.Ident:
			return v.Name
		default:
			return ""
		}
	}
}

func valueNames(tok token.Token, sp *ast.ValueSpec, types map[string]bool) []string {
	var names []string
	for _, id := range sp.Names {
		names = append(names, id.Name)
	}
	if tok == token.VAR && slices.ContainsFunc(sp.Values, func(e ast.Expr) bool { return hasCall(e, types) }) {
		names = append(names, "")
	}
	return names
}

// specNodes splits a type/var/const group per spec; an implicit const repeat inherits the previous spec's refs.
func specNodes(tf *testFile, decl ast.Decl, types map[string]bool) []*declNode {
	gen, ok := decl.(*ast.GenDecl)
	if !ok {
		return nil
	}
	var out []*declNode
	var prev map[string]bool
	for _, spec := range gen.Specs {
		node := &declNode{file: tf, refs: identNames(spec)}
		switch sp := spec.(type) {
		case *ast.TypeSpec:
			node.names = []string{sp.Name.Name}
		case *ast.ValueSpec:
			node.names = valueNames(gen.Tok, sp, types)
			if gen.Tok == token.CONST && len(sp.Values) == 0 && sp.Type == nil {
				maps.Copy(node.refs, prev)
			} else {
				prev = node.refs
			}
		}
		out = append(out, node)
	}
	return out
}

func epochHeader(epochs map[string]benchmarkEpoch) string {
	data, _ := json.Marshal(epochs) // strings only
	return epochPrefix + string(data) + "\n"
}

func parseEpochs(text string) (map[string]benchmarkEpoch, error) {
	for line := range strings.SplitSeq(text, "\n") {
		if raw, ok := strings.CutPrefix(line, epochPrefix); ok {
			var epochs map[string]benchmarkEpoch
			if err := json.Unmarshal([]byte(raw), &epochs); err != nil || epochs == nil {
				return nil, fmt.Errorf("invalid benchmark epoch header: %s", line)
			}
			for name, epoch := range epochs {
				if name == "" || epoch.File == "" || !validEpoch(epoch.Hash) {
					return nil, fmt.Errorf("invalid benchmark epoch for %q", name)
				}
			}
			return epochs, nil
		}
	}
	return nil, nil
}

func sameEpochs(before, after string) bool {
	b, berr := parseEpochs(before)
	a, aerr := parseEpochs(after)
	return berr == nil && aerr == nil && maps.Equal(b, a) && (b == nil) == (a == nil)
}

func validEpoch(epoch string) bool {
	decoded, err := hex.DecodeString(epoch)
	return err == nil && len(decoded) == sha256.Size && epoch == strings.ToLower(epoch)
}

func evaluateHistory(history []observation, now time.Time) []Verdict {
	ordered := slices.Clone(history)
	slices.SortStableFunc(ordered, func(a, b observation) int { return a.At.Compare(b.At) })
	groups := map[string][]observation{}
	for _, row := range ordered {
		key := row.Pkg + "\x00" + row.Benchmark
		group := groups[key]
		if len(group) > 0 && group[len(group)-1].Epoch != row.Epoch {
			group = nil
		}
		groups[key] = append(group, row)
	}
	verdicts := make([]Verdict, 0, len(groups))
	for _, group := range groups {
		last := group[len(group)-1]
		verdict, reason := benchmarkVerdict(group, now)
		verdicts = append(verdicts, Verdict{Pkg: last.Pkg, Benchmark: last.Benchmark, Epoch: last.Epoch,
			Verdict: verdict, Reason: reason})
	}
	slices.SortFunc(verdicts, func(a, b Verdict) int {
		return cmp.Compare(a.Pkg+"\x00"+a.Benchmark, b.Pkg+"\x00"+b.Benchmark)
	})
	return verdicts
}

type verdictEvidence struct {
	latest             observation
	compared, decisive time.Time
	path, subject      string
}

func summarizeEvidence(history []observation) verdictEvidence {
	var e verdictEvidence
	for _, row := range history {
		if row.Score != nil {
			e.latest = row
		}
		if row.Compared {
			e.compared = row.At
		}
		if row.Decisive {
			e.decisive = row.At
		}
		if row.ProductionPath != "" {
			e.path = row.ProductionPath
		}
		if row.Subject != "" {
			e.subject = row.Subject
		}
	}
	return e
}

func benchmarkVerdict(history []observation, now time.Time) (string, string) {
	e := summarizeEvidence(history)
	switch {
	case e.subject == "gone":
		return "dead", "recorded subject is gone; owner approval required for deletion"
	case e.path == "off":
		return "off-path", "recorded evidence says production does not run the subject"
	}
	if verdict, reason := measurementVerdict(history, e.latest); verdict != "" {
		return verdict, reason
	}
	switch {
	case now.Sub(history[0].At) >= idleAge && (e.compared.IsZero() || now.Sub(e.decisive) >= idleAge):
		return "idle", "never compared or no decisive evidence in 30 days"
	case e.compared.IsZero() && e.decisive.IsZero():
		return "idle", "no comparison or decision evidence"
	case e.path != "on": // design §10: idle means never compared or decisive, not an unrecorded production path
		return "unverified", "compared, but no production-path record (--production-path on|off)"
	}
	return "working", "stable, on production path, and compared or decisive"
}

func measurementVerdict(history []observation, latest observation) (string, string) {
	switch {
	case latest.Score == nil:
		return "idle", "no measured run in this epoch"
	case !isQuiet(*latest.Score) || latest.Score.Metrics["ns/op"].CV > noiseFlagCV || crossRunCV(history, latest) > noiseFlagCV:
		return "noisy", "load or within/across-run CV exceeds the quiet threshold"
	case latest.Score.Samples > 0 && latest.Score.CostSec/float64(latest.Score.Samples) > slowSampleSec:
		return "costly", "measured cost exceeds two seconds per sample"
	case latest.Score.Samples < 2:
		return "idle", "insufficient samples"
	}
	return "", ""
}

func crossRunCV(history []observation, latest observation) float64 {
	medians := make([]float64, 0, quietWindow)
	for i := len(history) - 1; i >= 0 && len(medians) < quietWindow; i-- {
		row := history[i]
		if row.Score != nil && isQuiet(*row.Score) && sameHost(row.Env, latest.Env) {
			if metric, ok := row.Score.Metrics["ns/op"]; ok {
				medians = append(medians, metric.Median)
			}
		}
	}
	if len(medians) < 2 {
		return 0
	}
	return spread(medians).CV
}

func sameHost(a, b Env) bool {
	return a.Machine == b.Machine && a.GOOS == b.GOOS && a.GOARCH == b.GOARCH && a.Go == b.Go &&
		a.CPUs == b.CPUs && a.GOMAXPROCS == b.GOMAXPROCS
}
