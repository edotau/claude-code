package assets

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"text/template"
)

type BenchFixInput struct {
	OwnedLine, Verify, Benchmark, Package, Workdir, Cause string
	Numbers, BeforeFile, BenchCommand, IdentityCommand    string
	CodeMode                                              bool // set: print bare commands, no rtk
}

type BenchReviewInput struct {
	OwnedLine, Verify, Benchmark, Package, Workdir, Numbers string
	BeforeFile, AfterFile, Diff, IdentityCommand            string
	CodeMode                                                bool // set: print bare commands, no rtk
}

type AutofixFinding struct {
	Class, Path, Rule, Message string
	Line                       int
}

type AutofixInput struct {
	Lane, Sha, Live, Gate, Owned, Fence string
	Policy                              bool
	Judgment                            []AutofixFinding
}

type briefCache struct {
	name, label string
	shell       bool
	once        sync.Once
	tmpl        *template.Template
	err         error
}

var (
	benchFixCache    = briefCache{name: "fix", label: "bench brief fix", shell: true}
	benchReviewCache = briefCache{name: "review", label: "bench brief review", shell: true}
	autofixCache     = briefCache{name: "fix", label: "autofix brief"}
)

func renderBrief(cache *briefCache, source string, data any) (string, error) {
	cache.once.Do(func() {
		t := template.New(cache.name).Option("missingkey=error")
		if cache.shell {
			t = t.Funcs(template.FuncMap{"sh": func(s string) string {
				return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
			}})
		}
		cache.tmpl, cache.err = t.Parse(source)
	})
	if cache.err != nil {
		return "", fmt.Errorf("parse %s: %w", cache.label, cache.err)
	}
	var out bytes.Buffer
	if err := cache.tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render %s: %w", cache.label, err)
	}
	return out.String(), nil
}

func BenchFixBrief(in BenchFixInput) (string, error) {
	return renderBrief(&benchFixCache, string(read("review/bench/briefs/fix.tmpl")), in)
}

func BenchReviewBrief(in BenchReviewInput) (string, error) {
	return renderBrief(&benchReviewCache, string(read("review/bench/briefs/review.tmpl")), in)
}

func AutofixBrief(in AutofixInput) (string, error) {
	return renderBrief(&autofixCache, string(read("review/autofix/briefs/fix.tmpl")), in)
}

func text(b []byte) string { return strings.TrimSuffix(string(b), "\n") }

func BenchNoDiff() string                    { return text(read("review/bench/no-diff.txt")) }
func WebSearchDescription() string           { return text(read("tools/search-description.txt")) }
func WebExtractDescription() string          { return text(read("tools/extract-description.txt")) }
func WebSearchQueryDescription() string      { return text(read("tools/search-query.txt")) }
func WebSearchMaxResultsDescription() string { return text(read("tools/search-max-results.txt")) }
func WebExtractURLsDescription() string      { return text(read("tools/extract-urls.txt")) }
func ProxySearchDescription() string         { return text(read("tools/proxy-search-description.txt")) }
func ProxySearchSchema() string              { return text(read("tools/proxy-search-schema.json")) }
func DatabricksSmokePrompt() string          { return text(read("tools/databricks-smoke.txt")) }
func DatabricksProbePrompt() string          { return text(read("tools/databricks-probe.txt")) }
func DatabricksToolDescription() string      { return text(read("tools/databricks-tool.txt")) }
func PreflightPongPrompt() string            { return text(read("tools/preflight-pong.txt")) }
