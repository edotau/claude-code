// Package source ports quality_core.py / code_quality_checker.py / complexity_checker.py to Go stdlib.
package source

import (
	"strconv"
	"strings"
)

// PyFloat marshals like Python's json.dumps(float): always keeps a decimal point ("100.0", not "100").
type PyFloat float64

// MarshalJSON implements json.Marshaler.
func (v PyFloat) MarshalJSON() ([]byte, error) {
	s := strconv.FormatFloat(float64(v), 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return []byte(s), nil
}

// Extensions mirrors quality_core.LANGUAGE_EXTENSIONS; order here has no meaning (Go maps are unordered).
var Extensions = map[string][]string{
	"python":     {".py"},
	"typescript": {".ts", ".tsx"},
	"javascript": {".js", ".jsx", ".mjs"},
	"go":         {".go"},
	"swift":      {".swift"},
	"kotlin":     {".kt", ".kts"},
}

// languageOrder fixes the iteration order quality_core's dict literal gives Python (insertion order).
var languageOrder = []string{"python", "typescript", "javascript", "go", "swift", "kotlin"}

// LineMetrics is quality_core.count_lines's return shape.
type LineMetrics struct {
	Total   int `json:"total"`
	Code    int `json:"code"`
	Blank   int `json:"blank"`
	Comment int `json:"comment"`
}

// FunctionInfo is one entry from quality_core.find_functions.
type FunctionInfo struct {
	Name       string `json:"name"`
	Parameters int    `json:"parameters"`
	Lines      int    `json:"lines"`
	Complexity int    `json:"complexity"`
}

// ClassInfo is one entry from quality_core.find_classes.
type ClassInfo struct {
	Name    string `json:"name"`
	Methods int    `json:"methods"`
	Lines   int    `json:"lines"`
}

// Smell is one entry from quality_core.check_code_smells.
type Smell struct {
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Location string `json:"location"`
}

// SolidViolation is one entry from quality_core.check_solid_violations.
type SolidViolation struct {
	Principle string `json:"principle"`
	Name      string `json:"name"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
}

// Metrics is the "metrics" object inside a QualityReport.
type Metrics struct {
	Lines         LineMetrics `json:"lines"`
	Functions     int         `json:"functions"`
	Classes       int         `json:"classes"`
	AvgComplexity PyFloat     `json:"avg_complexity"`
}

// QualityReport is code_quality_checker.analyze_file's dict shape (field order = JSON key order).
type QualityReport struct {
	File            string           `json:"file"`
	Language        string           `json:"language"`
	Metrics         Metrics          `json:"metrics"`
	QualityScore    int              `json:"quality_score"`
	Grade           string           `json:"grade"`
	Smells          []Smell          `json:"smells"`
	SolidViolations []SolidViolation `json:"solid_violations"`
	FunctionDetails []FunctionInfo   `json:"function_details"`
	ClassDetails    []ClassInfo      `json:"class_details"`
}

// errorResult is the {"error": "..."} shape both scripts return on a per-file/dir failure.
type errorResult struct {
	Error string `json:"error"`
}

// directoryReport is code_quality_checker.analyze_directory's dict shape.
type directoryReport struct {
	Directory            string          `json:"directory"`
	FilesAnalyzed        int             `json:"files_analyzed"`
	AverageScore         PyFloat         `json:"average_score"`
	OverallGrade         string          `json:"overall_grade"`
	TotalCodeSmells      int             `json:"total_code_smells"`
	TotalSolidViolations int             `json:"total_solid_violations"`
	Files                []QualityReport `json:"files"`
}
