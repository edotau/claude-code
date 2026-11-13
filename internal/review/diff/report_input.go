package diff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"

	"github.com/edotau/claude-code/internal/review/source"
)

// riskItem is one PR-analysis risk; Count is 1 when the input omitted it.
type riskItem struct {
	Name, Message, File string
	Count               int
}

// prInput is the part of a PR analysis the report reads, from a fresh run or a lenient --pr-analysis file.
type prInput struct {
	Status      string
	Complexity  int
	Risks       map[string][]riskItem // by severity
	Summary     *PRSummary
	summaryErr  error // a strict-decode failure, raised only when the status is "analyzed"
	ReviewOrder []string
}

// qualityFile is one analyzed file's smells and SOLID violations.
type qualityFile struct {
	File       string
	Smells     []source.Smell
	Violations []source.SolidViolation
}

// qualityInput is a quality analysis: its per-file results and the summary block derived from it.
type qualityInput struct {
	Files   []qualityFile
	Summary *QualitySummary
}

// qualityIssue is one smell or SOLID violation flattened for the findings list.
type qualityIssue struct{ Severity, Type, Message, File, Line string }

func prInputOf(r PRReport) *prInput {
	in := &prInput{Status: r.Status, Summary: r.Summary, ReviewOrder: r.ReviewOrder, Risks: map[string][]riskItem{}}
	if r.Summary != nil {
		in.Complexity = r.Summary.ComplexityScore
	}
	if r.Risks != nil {
		for _, sev := range severityOrder {
			for _, k := range *r.Risks.bucket(sev) {
				in.Risks[sev] = append(in.Risks[sev], riskItem{k.Name, k.Message, k.File, k.Count})
			}
		}
	}
	return in
}

func qualityInputOf(reports []source.QualityReport) *qualityInput {
	q := &qualityInput{}
	avg, smells, violations := 100.0, 0, 0
	sum := 0.0
	for _, r := range reports {
		q.Files = append(q.Files, qualityFile{r.File, r.Smells, r.SolidViolations})
		sum += float64(r.QualityScore)
		smells += len(r.Smells)
		violations += len(r.SolidViolations)
	}
	if len(reports) > 0 {
		avg = source.Round1(sum / float64(len(reports)))
	}
	q.Summary = &QualitySummary{len(reports), source.PyFloat(avg), smells, violations}
	return q
}

// readJSONObject reads a JSON object from path; its fields stay raw so each reader applies its own leniency.
func readJSONObject(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Struct == "" {
			return nil, &json.UnmarshalTypeError{Value: typeErr.Value, Type: reflect.TypeFor[map[string]any](), Offset: typeErr.Offset}
		}
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	return m, nil
}

// loadPRAnalysisFile reads and validates a --pr-analysis file.
func loadPRAnalysisFile(path string) (*prInput, error) {
	m, err := readJSONObject(path)
	if err != nil {
		return nil, err
	}
	if err := validatePRAnalysis(m); err != nil {
		return nil, err
	}
	return decodePRAnalysis(m), nil
}

// loadQualityAnalysisFile reads and validates a --quality-analysis file.
func loadQualityAnalysisFile(path string) (*qualityInput, error) {
	m, err := readJSONObject(path)
	if err != nil {
		return nil, err
	}
	if err := validateQualityAnalysis(m); err != nil {
		return nil, err
	}
	return decodeQualityAnalysis(m), nil
}

// validatePRAnalysis accepts pr_analyzer output: status no_changes, or analyzed with object summary and risks.
func validatePRAnalysis(m map[string]json.RawMessage) error {
	switch status := rawString(m["status"]); status {
	case "no_changes":
		return nil
	case "analyzed":
	default:
		return fmt.Errorf(`not PR analysis output: "status" is %q, want "analyzed" or "no_changes"`, status)
	}
	for _, key := range []string{"summary", "risks"} {
		if v, ok := m[key]; ok && !isJSONKind(v, '{') {
			return fmt.Errorf("not PR analysis output: %q must be an object", key)
		}
	}
	risks, _ := rawObject(m["risks"])
	for _, sev := range severityOrder {
		if v, ok := risks[sev]; ok && !isJSONKind(v, '[') {
			return fmt.Errorf("not PR analysis output: risks.%s must be an array", sev)
		}
	}
	return nil
}

// validateQualityAnalysis accepts quality output: "files" an array of objects, or "file" a string (single file).
func validateQualityAnalysis(m map[string]json.RawMessage) error {
	files, ok := m["files"]
	if !ok {
		if isJSONKind(m["file"], '"') {
			return nil
		}
		return errors.New(`not quality analysis output: neither "files" nor "file" is present`)
	}
	list, isList := rawArray(files)
	if !isList {
		return errors.New(`not quality analysis output: "files" must be an array`)
	}
	for _, f := range list {
		if !isJSONKind(f, '{') {
			return errors.New(`not quality analysis output: every "files" entry must be an object`)
		}
	}
	return nil
}

// decodePRAnalysis reads the fields the report uses; wrong-typed ones fall back to zero, a missing count to 1.
func decodePRAnalysis(m map[string]json.RawMessage) *prInput {
	in := &prInput{Status: rawString(m["status"]), Risks: map[string][]riskItem{}}
	if summary, ok := rawObject(m["summary"]); ok {
		in.Complexity = rawInt(summary["complexity_score"])
		in.Summary, in.summaryErr = decodePRSummary(summary)
		in.ReviewOrder = []string{}
		items, _ := rawArray(m["review_order"])
		for _, v := range items {
			if isJSONKind(v, '"') {
				in.ReviewOrder = append(in.ReviewOrder, rawString(v))
			}
		}
	}
	risks, _ := rawObject(m["risks"])
	for _, sev := range severityOrder {
		items, _ := rawArray(risks[sev])
		for _, item := range items {
			in.Risks[sev] = append(in.Risks[sev], decodeRisk(item))
		}
	}
	return in
}

func decodeRisk(raw json.RawMessage) riskItem {
	obj, _ := rawObject(raw)
	k := riskItem{Name: rawString(obj["name"]), Message: rawString(obj["message"]), File: rawString(obj["file"]), Count: 1}
	if c, has := obj["count"]; has {
		k.Count = rawInt(c)
	}
	return k
}

// decodePRSummary decodes strictly, field by field in key order, naming the first type mismatch.
func decodePRSummary(obj map[string]json.RawMessage) (*PRSummary, error) {
	var ps PRSummary
	steps := []struct {
		key  string
		into any
	}{
		{"commits", &ps.Commits}, {"complexity_label", &ps.ComplexityLabel}, {"complexity_score", &ps.ComplexityScore},
		{"files_changed", &ps.FilesChanged}, {"total_additions", &ps.TotalAdditions}, {"total_deletions", &ps.TotalDeletions},
	}
	for _, s := range steps {
		if err := decodeStrict(obj[s.key], s.into); err != nil {
			err.Struct, err.Field = "PRSummary", s.key
			return nil, fmt.Errorf("decode PR analysis summary: %w", err)
		}
	}
	return &ps, nil
}

// decodeStrict stores raw into *int or *string like encoding/json would: null and missing leave it, a mismatch errors.
func decodeStrict(raw json.RawMessage, into any) *json.UnmarshalTypeError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	switch dst := into.(type) {
	case *string:
		if !isJSONKind(raw, '"') {
			return &json.UnmarshalTypeError{Value: jsonKindName(raw), Type: reflect.TypeFor[string]()}
		}
		*dst = rawString(raw)
	case *int:
		kind := jsonKindName(raw)
		f := rawFloat(raw)
		if kind != "number" {
			return &json.UnmarshalTypeError{Value: kind, Type: reflect.TypeFor[int]()}
		}
		if f != math.Trunc(f) || math.Abs(f) >= 1<<63 {
			lit, _ := json.Marshal(f)
			return &json.UnmarshalTypeError{Value: "number " + string(lit), Type: reflect.TypeFor[int]()}
		}
		*dst = int(f)
	}
	return nil
}

// decodeQualityAnalysis reads a validated directory ("files") or single-file ("file") analysis leniently.
func decodeQualityAnalysis(m map[string]json.RawMessage) *qualityInput {
	if raw, ok := m["files"]; ok {
		q := &qualityInput{Summary: &QualitySummary{
			FilesAnalyzed: rawInt(m["files_analyzed"]), AverageScore: source.PyFloat(rawFloat(m["average_score"])),
			TotalCodeSmells: rawInt(m["total_code_smells"]), TotalSolidViolations: rawInt(m["total_solid_violations"]),
		}}
		files, _ := rawArray(raw)
		for _, f := range files {
			obj, _ := rawObject(f)
			q.Files = append(q.Files, decodeQualityFile(obj))
		}
		return q
	}
	f := decodeQualityFile(m)
	return &qualityInput{Files: []qualityFile{f}, Summary: &QualitySummary{
		FilesAnalyzed: 1, AverageScore: source.PyFloat(rawFloat(m["quality_score"])),
		TotalCodeSmells: len(f.Smells), TotalSolidViolations: len(f.Violations),
	}}
}

// decodeQualityFile keeps every smells/solid_violations entry, a non-object one as an all-default entry.
func decodeQualityFile(obj map[string]json.RawMessage) qualityFile {
	f := qualityFile{File: rawString(obj["file"])}
	smells, _ := rawArray(obj["smells"])
	for _, s := range smells {
		o, _ := rawObject(s)
		f.Smells = append(f.Smells, source.Smell{
			Type: rawString(o["type"]), Severity: rawString(o["severity"]),
			Message: rawString(o["message"]), Location: rawString(o["location"]),
		})
	}
	violations, _ := rawArray(obj["solid_violations"])
	for _, v := range violations {
		o, _ := rawObject(v)
		f.Violations = append(f.Violations, source.SolidViolation{
			Principle: rawString(o["principle"]), Severity: rawString(o["severity"]), Message: rawString(o["message"]),
		})
	}
	return f
}

// extractQualityIssues flattens smells and solid_violations into one list, defaulting severity and type.
func extractQualityIssues(q *qualityInput) []qualityIssue {
	var issues []qualityIssue
	for _, fr := range q.Files {
		for _, s := range fr.Smells {
			issues = append(issues, qualityIssue{
				orDefault(s.Severity, "medium"), orDefault(s.Type, "smell"), s.Message, fr.File, s.Location,
			})
		}
		for _, v := range fr.Violations {
			issues = append(issues, qualityIssue{
				orDefault(v.Severity, "medium"), orDefault(v.Principle, "solid"), v.Message, fr.File, "",
			})
		}
	}
	return issues
}

// The raw* readers are deliberately lenient: a missing or wrong-typed value yields the zero value.

func rawString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func rawFloat(raw json.RawMessage) float64 {
	var f float64
	_ = json.Unmarshal(raw, &f)
	return f
}

func rawInt(raw json.RawMessage) int { return int(rawFloat(raw)) }

func isJSONKind(raw json.RawMessage, first byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == first
}

func jsonKindName(raw json.RawMessage) string {
	switch bytes.TrimSpace(raw)[0] {
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case '[':
		return "array"
	case '{':
		return "object"
	}
	return "number"
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if !isJSONKind(raw, '{') || json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

func rawArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	var a []json.RawMessage
	if !isJSONKind(raw, '[') || json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	return a, true
}
