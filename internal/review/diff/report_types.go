package diff

import ()

var severityWeights = map[string]int{"critical": 100, "high": 75, "medium": 50, "low": 25, "info": 10}

type verdictThreshold struct {
	name        string
	minScore    int
	maxCritical int
	maxHigh     int // -1 means unbounded (Python's float("inf"))
}

// verdictThresholds is checked in order; first match wins (mirrors VERDICT_THRESHOLDS dict order).
var verdictThresholds = []verdictThreshold{
	{"approve", 90, 0, 0},
	{"approve_with_suggestions", 75, 0, 2},
	{"request_changes", 50, 0, -1},
}

var verdictRationales = map[string]string{
	"approve":                  "Code meets quality standards",
	"approve_with_suggestions": "Minor improvements recommended",
	"request_changes":          "Several issues need to be addressed",
}

var deductionWeights = map[string]int{"critical": 15, "high": 8, "medium": 3, "low": 1}
var deductionCaps = map[string]int{"critical": 60, "high": 40, "medium": 20, "low": 10}
var severityOrder = []string{"critical", "high", "medium", "low"}

// Finding is a report line item; Count is set for pr_analysis risks, Line for quality issues
// (never both) — pointers so the omitted one matches Python's absent dict key exactly.
type Finding struct {
	Source   string  `json:"source"`
	Severity string  `json:"severity"`
	Category string  `json:"category"`
	Message  string  `json:"message"`
	File     string  `json:"file"`
	Count    *int    `json:"count,omitempty"`
	Line     *string `json:"line,omitempty"`
}

// ActionItem is a prioritized, deduped recommendation derived from findings.
type ActionItem struct {
	Priority      string   `json:"priority"`
	Action        string   `json:"action"`
	Severity      string   `json:"severity"`
	FilesAffected []string `json:"files_affected"`
}

// ReportIssueCounts buckets finding counts by severity for the summary block.
type ReportIssueCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

// ReportSummary is the review_report_generator.py "summary" block.
type ReportSummary struct {
	Score       int               `json:"score"`
	Verdict     string            `json:"verdict"`
	Rationale   string            `json:"rationale"`
	IssueCounts ReportIssueCounts `json:"issue_counts"`
}

// ReportMetadata is the review_report_generator.py "metadata" block.
type ReportMetadata struct {
	GeneratedAt string `json:"generated_at"`
	Repository  string `json:"repository"`
	Version     string `json:"version"`
}

// QualitySummary re-derives files_analyzed/average_score/etc. in a fixed key order.
type QualitySummary struct {
	FilesAnalyzed        int     `json:"files_analyzed"`
	AverageScore         pyFloat `json:"average_score"`
	TotalCodeSmells      int     `json:"total_code_smells"`
	TotalSolidViolations int     `json:"total_solid_violations"`
}

// Report is the full review_report_generator.py output shape.
type Report struct {
	Metadata       ReportMetadata  `json:"metadata"`
	Summary        ReportSummary   `json:"summary"`
	Findings       []Finding       `json:"findings"`
	ActionItems    []ActionItem    `json:"action_items"`
	PRSummary      *PRSummary      `json:"pr_summary,omitempty"`
	ReviewOrder    []string        `json:"review_order,omitempty"`
	QualitySummary *QualitySummary `json:"quality_summary,omitempty"`
}
