package diff

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/source"
)

func loadJSONFile(path string) (map[string]any, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
}

func toGenericMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

func mStr(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// mFloat handles both json.Unmarshal's float64 and native int/float64 from our own runQualityChecker map.
func mFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	default:
		return 0
	}
}

func mInt(m map[string]any, key string) int {
	return int(mFloat(m, key))
}

func mSlice(m map[string]any, key string) []any {
	if s, ok := m[key].([]any); ok {
		return s
	}
	return nil
}

func mMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// getChangedSourceFiles ports get_changed_source_files: git diff --name-only, filtered to
// existing files whose extension appears in source.Extensions.
func getChangedSourceFiles(repo, base, head string) []string {
	exts := map[string]bool{}
	for _, list := range source.Extensions {
		for _, e := range list {
			exts[strings.ToLower(e)] = true
		}
	}
	tryArgs := [][]string{{base + "..." + head}, {base, head}}
	for _, extra := range tryArgs {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		full := append([]string{"diff", "--name-only", "--relative"}, extra...)
		out, err := proc.RunGitInherit(ctx, repo, full...)
		cancel()
		if err != nil {
			continue
		}
		var files []string
		for line := range strings.SplitSeq(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			ext := extAt(line)
			if !exts[ext] {
				continue
			}
			full := repo + "/" + line
			if st, statErr := os.Stat(full); statErr == nil && !st.IsDir() {
				files = append(files, full)
			}
		}
		return files
	}
	return nil
}

func extAt(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx == -1 {
		return ""
	}
	return strings.ToLower(path[idx:])
}

func languageForExt(ext string) string {
	for lang, exts := range source.Extensions {
		for _, e := range exts {
			if strings.ToLower(e) == ext {
				return lang
			}
		}
	}
	return ""
}

// runQualityChecker ports run_quality_checker: source.Quality over each changed source file.
func runQualityChecker(repo, base, head string) map[string]any {
	var results []map[string]any
	for _, path := range getChangedSourceFiles(repo, base, head) {
		lang := languageForExt(extAt(path))
		qr, err := source.Quality(path, lang)
		if err != nil {
			continue
		}
		m := toGenericMap(qr)
		if _, isErr := m["error"]; isErr {
			continue
		}
		results = append(results, m)
	}

	avg := 100.0
	totalSmells, totalViolations := 0, 0
	if len(results) > 0 {
		sum := 0.0
		for _, r := range results {
			sum += mFloat(r, "quality_score")
			totalSmells += len(mSlice(r, "smells"))
			totalViolations += len(mSlice(r, "solid_violations"))
		}
		avg = roundTo(sum/float64(len(results)), 1)
	}
	filesAny := make([]any, len(results))
	for i, r := range results {
		filesAny[i] = r
	}
	return map[string]any{
		"files_analyzed":         len(results),
		"average_score":          avg,
		"total_code_smells":      totalSmells,
		"total_solid_violations": totalViolations,
		"files":                  filesAny,
	}
}

// extractQualityIssues flattens quality_analysis["files"] (or itself, single-file shape) smells
// and solid_violations into a uniform {severity,type,message,file,line} list.
func extractQualityIssues(qualityAnalysis map[string]any) []map[string]string {
	var issues []map[string]string
	for _, fr := range qualityFileResults(qualityAnalysis) {
		path := mStr(fr, "file")
		for _, s := range mSlice(fr, "smells") {
			sm := asMap(s)
			issues = append(issues, map[string]string{
				"severity": orDefault(mStr(sm, "severity"), "medium"), "type": orDefault(mStr(sm, "type"), "smell"),
				"message": mStr(sm, "message"), "file": path, "line": mStr(sm, "location"),
			})
		}
		for _, v := range mSlice(fr, "solid_violations") {
			vm := asMap(v)
			issues = append(issues, map[string]string{
				"severity": orDefault(mStr(vm, "severity"), "medium"), "type": orDefault(mStr(vm, "principle"), "solid"),
				"message": mStr(vm, "message"), "file": path, "line": "",
			})
		}
	}
	return issues
}

// qualityFileResults returns the per-file maps of quality_analysis["files"], or the analysis itself (single-file shape).
func qualityFileResults(qualityAnalysis map[string]any) []map[string]any {
	fileResults := mSlice(qualityAnalysis, "files")
	if fileResults == nil {
		return []map[string]any{qualityAnalysis}
	}
	var frs []map[string]any
	for _, f := range fileResults {
		if m := asMap(f); m != nil {
			frs = append(frs, m)
		}
	}
	return frs
}
