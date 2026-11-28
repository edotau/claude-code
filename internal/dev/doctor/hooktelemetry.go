package doctor

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/hooktelemetry"
	"github.com/edotau/claude-code/internal/platform/paths"
)

func hookTelemetryChecks(now time.Time) []check {
	report := hooktelemetry.Read(now, hooktelemetry.Activity)
	modules := report.Modules()
	detail := "module | fires | denies | overruns | p50 ms | p95 ms"
	if len(modules) == 0 {
		detail = "no runs recorded"
	}
	if report.Probes > 0 {
		detail += fmt.Sprintf(" (+%d probe rows skipped)", report.Probes)
	}
	if report.Dropped > 0 {
		detail += fmt.Sprintf("; incomplete (%d cap markers)", report.Dropped)
	}
	if report.Truncated {
		detail += "; read limit reached"
	}
	rows := []check{{Name: "hook telemetry (7 days, since " + report.Start + ")", OK: true, Detail: detail}}
	for _, m := range modules {
		p95 := "- | -"
		if m.HasDuration {
			p95 = fmt.Sprintf("%.2f | %.2f", m.P50, m.P95)
		}
		rows = append(rows, check{Name: "hook " + m.Name, OK: true,
			Detail: fmt.Sprintf("%d | %d | %d | %s", m.Fires, m.Denies, m.Overruns, p95)})
	}
	return rows
}

const benchCoverageCommits = 12

// benchCoverageCheck is the post-commit bench row: the R22 trigger's reach; a failure or regression is a soft ✗.
func benchCoverageCheck(ctx context.Context) check {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	detail, ok := docs.BenchCoverage(ctx, paths.ConfigDir(), benchCoverageCommits)
	return check{Name: "post-commit bench", OK: ok, Detail: detail}
}

// fixNowCheck is the soft ✗ for failures whose fix-now directive went unclaimed DirectiveMax times.
func fixNowCheck(ctx context.Context) check {
	open, err := worktree.UnclaimedDirectives(ctx, worktree.FailuresDir())
	if err != nil {
		return check{Name: "failure directives", Detail: "failure log unreadable: " + err.Error()}
	}
	if len(open) == 0 {
		return check{Name: "failure directives", OK: true, Detail: "none unclaimed"}
	}
	f := open[0]
	return check{Name: "failure directives", Detail: fmt.Sprintf("%d unclaimed after %d deliveries; newest %s %s: claude-code failures claim %s",
		len(open), worktree.DirectiveMax, f.Kind, f.Subject, f.ID)}
}

// hookCtxStat tracks injected hook_additional_context bytes and duplicate session emissions by category.
type hookCtxStat struct {
	count, bytes, dupes int
}

// hookCtxAudit holds aggregated transcript context injection stats for `doctor --hooks`.
type hookCtxAudit struct {
	sessions, totalBytes, recallIDs, recallDupes, recallUsed int
	cats                                                     map[string]*hookCtxStat
}

// runHooks implements `claude-code doctor --hooks [--days N] [--format text|json]`.
func runHooks(o *doctorOpts) int {
	if o.days < 1 {
		return helptext.FailUsage("doctor: --days must be at least 1 (try --days 7)")
	}
	now := time.Now()
	rep := hooktelemetry.Read(now, hooktelemetry.Activity)
	ctxDir := filepath.Join(paths.ConfigDir(), "projects")
	win := time.Duration(o.days) * 24 * time.Hour
	ca := scanHookContext(ctxDir, now, win, usageBudget)
	checks := buildHookAuditChecks(o.days, rep, ca)
	return printChecks(o, checks, true)
}

func buildHookAuditChecks(days int, rep hooktelemetry.Report, ca hookCtxAudit) []check {
	mods := rep.Modules()
	var fires, denies, overruns int
	for _, m := range mods {
		fires += m.Fires
		denies += m.Denies
		overruns += m.Overruns
	}
	slices.SortFunc(mods, func(a, b hooktelemetry.Module) int {
		return cmp.Or(cmp.Compare(b.Fires, a.Fires), cmp.Compare(a.Name, b.Name))
	})
	out := []check{{
		Name: "hook effectiveness",
		OK:   overruns == 0,
		Detail: fmt.Sprintf("since %s: fires=%d blocks=%d (%.2f%%) overruns=%d",
			rep.Start, fires, denies, pct(denies, fires), overruns),
	}}
	for _, m := range mods[:min(len(mods), 10)] {
		out = append(out, check{
			Name:   "hook " + m.Name,
			OK:     m.Overruns == 0,
			Detail: fmt.Sprintf("fires=%d blocks=%d (%.2f%%) overruns=%d p50=%.1fms p95=%.1fms", m.Fires, m.Denies, pct(m.Denies, m.Fires), m.Overruns, m.P50, m.P95),
		})
	}
	out = append(out, hookContextChecks(days, ca)...)
	return append(out, hookOptimizationChecks(mods, ca)...)
}

func hookContextChecks(days int, ca hookCtxAudit) []check {
	out := []check{{
		Name: "hook context tokens",
		OK:   true,
		Detail: fmt.Sprintf("%dd (%d sessions): %d B (~%d tok) injected across %d categories",
			days, ca.sessions, ca.totalBytes, ca.totalBytes/4, len(ca.cats)),
	}}
	var names []string
	for k := range ca.cats {
		names = append(names, k)
	}
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(ca.cats[b].bytes, ca.cats[a].bytes), cmp.Compare(a, b))
	})
	for _, k := range names[:min(len(names), 8)] {
		st := ca.cats[k]
		detail := fmt.Sprintf("%d B (~%d tok, %.1f%%) across %d injections", st.bytes, st.bytes/4, pct(st.bytes, ca.totalBytes), st.count)
		if st.dupes > 0 {
			detail += fmt.Sprintf(" (%d duplicate session repeats)", st.dupes)
		}
		out = append(out, check{Name: "context " + k, OK: true, Detail: detail})
	}
	return out
}

func hookOptimizationChecks(mods []hooktelemetry.Module, ca hookCtxAudit) []check {
	var out []check
	if st := ca.cats["memory-recall"]; st != nil && (pct(st.bytes, ca.totalBytes) > 25 || ca.recallDupes > 0 || pct(ca.recallUsed, ca.recallIDs) < 5) {
		out = append(out, check{
			Name: "optimize memory-recall",
			OK:   true,
			Detail: fmt.Sprintf("%.1f%% of hook tokens (~%d tok), %d/%d IDs used (%.2f%%), %d duplicate IDs -> dedup per session, raise score bar, and set Mode: review / GoldfishContext on cold-read briefs",
				pct(st.bytes, ca.totalBytes), st.bytes/4, ca.recallUsed, ca.recallIDs, pct(ca.recallUsed, ca.recallIDs), ca.recallDupes),
		})
	}
	if st := ca.cats["lane-land-queue"]; st != nil && (st.dupes > 0 || pct(st.bytes, ca.totalBytes) > 10) {
		out = append(out, check{
			Name:   "optimize lane-land-queue",
			OK:     true,
			Detail: fmt.Sprintf("~%d tok (%d duplicate repeats) -> emit land-queue/stop-job/claim-peer notices only when state changes", st.bytes/4, st.dupes),
		})
	}
	out = append(out, hookSignalSuggestions(mods)...)
	if len(out) == 0 {
		out = append(out, check{Name: "optimize hooks", OK: true, Detail: "no high-token or low-signal hook anomalies detected in window"})
	}
	return out
}

var blockingHooks = map[string]bool{
	"dispatch-handback": true, "docs-placement": true, "safety-scan": true, "worktree-busy": true, "worktree-claim-check": true,
}

func hookSignalSuggestions(mods []hooktelemetry.Module) []check {
	var out []check
	for _, m := range mods {
		if blockingHooks[m.Name] && m.Fires >= 100 && m.Denies*1000 < m.Fires {
			out = append(out, check{
				Name:   "optimize " + m.Name,
				OK:     true,
				Detail: fmt.Sprintf("%d fires with %d blocks (%.3f%% hit rate) -> skip read-only Bash/tool calls or fold into pre-filter", m.Fires, m.Denies, pct(m.Denies, m.Fires)),
			})
		}
		if m.Overruns > 0 || m.P95 >= 1000 {
			out = append(out, check{
				Name:   "optimize " + m.Name + " latency",
				OK:     m.Overruns == 0,
				Detail: fmt.Sprintf("overruns=%d p95=%.1fms -> detach background cleanup or batch per-lane process scans", m.Overruns, m.P95),
			})
		}
	}
	slices.SortFunc(out, func(a, b check) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

func scanHookContext(dir string, now time.Time, window, budget time.Duration) hookCtxAudit {
	ca := hookCtxAudit{cats: map[string]*hookCtxStat{}}
	deadline := time.Now().Add(budget)
	for _, p := range recentSessions(dir, now, window, usageMaxSessions) {
		if time.Now().After(deadline) {
			break
		}
		if scanOneSessionHookCtx(p, &ca) == nil {
			ca.sessions++
		}
	}
	return ca
}

func scanOneSessionHookCtx(path string, ca *hookCtxAudit) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	seenText, recallIDs, usedIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), usageLineCap)
	for sc.Scan() {
		b := sc.Bytes()
		if bytes.Contains(b, []byte(`"hook_additional_context"`)) {
			recordHookAttachment(b, ca, seenText, recallIDs)
		} else if len(recallIDs) > 0 && bytes.Contains(b, []byte(`"tool_use"`)) {
			recordRecallIDUse(b, recallIDs, usedIDs)
		}
	}
	ca.recallUsed += len(usedIDs)
	return nil
}

func recordHookAttachment(line []byte, ca *hookCtxAudit, seenText, recallIDs map[string]bool) {
	var row struct {
		Attachment struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &row) != nil || row.Attachment.Type != "hook_additional_context" {
		return
	}
	txt := decodeAttachmentText(row.Attachment.Content)
	if txt == "" {
		return
	}
	cat := classifyHookContext(txt)
	st := ca.cats[cat]
	if st == nil {
		st = &hookCtxStat{}
		ca.cats[cat] = st
	}
	st.count++
	st.bytes += len(txt)
	ca.totalBytes += len(txt)
	if seenText[cat+"\x00"+txt] {
		st.dupes++
	}
	seenText[cat+"\x00"+txt] = true
	if cat == "memory-recall" {
		countRecallIDs(txt, ca, recallIDs)
	}
}

func decodeAttachmentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var ss []string
	if json.Unmarshal(raw, &ss) == nil {
		return strings.Join(ss, "\n")
	}
	return ""
}

func countRecallIDs(txt string, ca *hookCtxAudit, recallIDs map[string]bool) {
	for l := range strings.SplitSeq(txt, "\n") {
		ptr, ok := strings.CutPrefix(strings.TrimSpace(l), "- ")
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(strings.Trim(ptr, "[]"), " ")
		if id, _, _ = strings.Cut(id, "]"); id != "" {
			ca.recallIDs++
			if recallIDs[id] {
				ca.recallDupes++
			}
			recallIDs[id] = true
		}
	}
}

func recordRecallIDUse(line []byte, recallIDs, usedIDs map[string]bool) {
	for id := range recallIDs {
		if !usedIDs[id] && bytes.Contains(line, []byte(id)) {
			usedIDs[id] = true
		}
	}
}

var hookCtxRules = []struct{ cat, needle string }{
	{"memory-recall", "<memory-recall>"},
	{"memory-recall", "memory-recall for this dispatch"},
	{"lane-land-queue", "lane(s) awaiting land"},
	{"lane-land-queue", "worktree land"},
	{"memory-start", "<memory-start>"},
	{"memory-start", "Session memory bank"},
	{"paths-resolve", "paths-resolve"},
	{"stop-job-lines", "background job"},
	{"claim-peer", "claim-peer"},
	{"plan-tick-notice", "active plan"},
	{"review-simplicity", "Skill(simplify)"},
}

func classifyHookContext(txt string) string {
	for _, r := range hookCtxRules {
		if strings.Contains(txt, r.needle) {
			return r.cat
		}
	}
	return "other"
}

func pct(num, den int) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) * 100 / float64(den)
}
