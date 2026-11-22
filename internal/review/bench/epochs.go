package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

type benchmarkEpoch struct {
	File string `json:"file"`
	Hash string `json:"hash"`
}

const epochPrefix = "# bench-epochs: "

func benchmarkFunc(name string) string {
	root, _, _ := strings.Cut(name, "/")
	return procSuffix.ReplaceAllString(root, "")
}

func benchmarkEpochs(top, pkg string) (map[string]benchmarkEpoch, error) {
	dir := filepath.Join(top, pkg)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]benchmarkEpoch{}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		epochs, err := fileEpochs(top, filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, err
		}
		maps.Copy(out, epochs)
	}
	return out, nil
}

func fileEpochs(top, file string) (map[string]benchmarkEpoch, error) {
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
	hash := sha256.Sum256(body)
	out := map[string]benchmarkEpoch{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Benchmark") {
			out[fn.Name.Name] = benchmarkEpoch{File: filepath.ToSlash(rel), Hash: hex.EncodeToString(hash[:])}
		}
	}
	return out, nil
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
