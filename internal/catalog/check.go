package catalog

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Targets resolves the probe set: every served chat endpoint (all), else ids (default: defaults) against served.
func Targets(ids []string, all bool, served map[string]providers.ModelInfo, p *providers.Provider, defaults []string,
) []serving.CheckTarget {
	var out []serving.CheckTarget
	if all {
		for _, name := range slices.Sorted(maps.Keys(served)) {
			if served[name].Task != "llm/v1/chat" {
				continue
			}
			id := ID(name)
			out = append(out, serving.CheckTarget{ID: id, Wire: name, Responses: providers.ResponsesID(id)})
		}
		return out
	}
	if len(ids) == 0 {
		ids = defaults
	}
	for _, raw := range ids {
		id := ID(raw)
		t := serving.CheckTarget{ID: id, Responses: providers.ResponsesID(id)}
		if _, ok := served[p.ModelPrefix+id]; ok {
			t.Wire = p.ModelPrefix + id
		} else if _, ok := served[id]; ok {
			t.Wire = id
		} else {
			t.Missing = true
		}
		out = append(out, t)
	}
	return out
}

// Check probes targets on p, sorts the results and merges them into the cache; cacheErr is a cache write failure.
func Check(p *providers.Provider, targets []serving.CheckTarget, o serving.CheckOptions,
) (results []serving.CheckResult, cacheErr, err error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results, err = serving.Probe(ctx, p, targets, o)
	if err != nil { // an interrupted run is neither cached nor reported as success
		return nil, nil, err
	}
	SortCheckResults(results)
	return results, SaveCheckCache(CheckCache{CheckedAt: time.Now().UTC(), Provider: p.Name, Results: merged(p.Name, results)}), nil
}

// merged is the profile's cached results with results laid over them by id, sanitized: never raw error bodies.
func merged(profile string, results []serving.CheckResult) []serving.CheckResult {
	byID, _ := LoadCheckCache(profile)
	if byID == nil {
		byID = map[string]serving.CheckResult{}
	}
	for _, r := range results {
		byID[r.ID] = r
	}
	out := serving.SanitizeChecks(slices.Collect(maps.Values(byID)))
	SortCheckResults(out)
	return out
}

// WriteCheck renders probe results as the ID / STATUS / HTTP / LATENCY / TOOLS table, or JSON.
func WriteCheck(w io.Writer, results []serving.CheckResult, format string) error {
	if format == helptext.JSON {
		return helptext.WriteJSON(w, results)
	}
	idW := ColWidth("ID", len(results), func(i int) string { return results[i].ID })
	fmt.Fprintf(w, "%-*s  %-9s  %-4s  %-10s  %s\n", idW, "ID", "STATUS", "HTTP", "LATENCY", "TOOLS")
	for _, r := range results {
		code, lat := "-", "-"
		if r.HTTP > 0 {
			code = strconv.Itoa(r.HTTP)
		}
		if r.Status != "missing" {
			lat = fmt.Sprintf("%dms", r.LatencyMS)
		}
		fmt.Fprintf(w, "%-*s  %-9s  %-4s  %-10s  %s", idW, r.ID, r.Status, code, lat, cmp.Or(r.Tools, "-"))
		if (r.Status == "fail" || r.Status == serving.StatusTransient) && r.Error != "" {
			fmt.Fprint(w, "  "+r.Error)
		}
		fmt.Fprintln(w)
	}
	return nil
}

// ColWidth is the widest of the head and the n cells.
func ColWidth(head string, n int, cell func(int) string) int {
	w := len(head)
	for i := range n {
		w = max(w, len(cell(i)))
	}
	return w
}

// CheckCell renders a cached probe as "ok tools", "ok no-tools", "fail 400", "missing" or "-".
func CheckCell(cache map[string]serving.CheckResult, id string) string {
	r, ok := cache[id]
	switch {
	case !ok:
		return "-"
	case r.Status == "ok" && r.Tools == "no":
		return "ok no-tools"
	case r.Status == "ok":
		return "ok tools"
	case (r.Status == "fail" || r.Status == serving.StatusTransient) && r.HTTP > 0:
		return fmt.Sprintf("%s %d", r.Status, r.HTTP)
	}
	return r.Status
}

// CheckFamily is the id's leading letters, e.g. claude, gpt, gemini, glm, qwen.
func CheckFamily(id string) string {
	id = stripSpellingPrefix(id)
	if i := strings.IndexFunc(id, func(r rune) bool { return !unicode.IsLetter(r) }); i > 0 {
		return id[:i]
	}
	return id
}

// SortCheckResults orders by family, then latency (non-ok last in family), then id.
func SortCheckResults(rs []serving.CheckResult) {
	slices.SortStableFunc(rs, func(a, b serving.CheckResult) int {
		if c := cmp.Compare(CheckFamily(a.ID), CheckFamily(b.ID)); c != 0 {
			return c
		}
		la, lb := a.LatencyMS, b.LatencyMS
		if a.Status != "ok" {
			la = 1 << 60
		}
		if b.Status != "ok" {
			lb = 1 << 60
		}
		return cmp.Or(cmp.Compare(la, lb), cmp.Compare(a.ID, b.ID))
	})
}

// CheckUsable probes ids, else every chat endpoint (all), else those usable on dialects, each on its client route.
func CheckUsable(p *providers.Provider, dialects, ids []string, all bool, timeout time.Duration,
) (results []serving.CheckResult, cacheErr, err error) {
	served, err := Served(p)
	if err != nil {
		return nil, nil, err
	}
	var usable []string
	for _, info := range Usable(p, served, dialects) {
		usable = append(usable, info.ID)
	}
	return Check(p, Targets(ids, all, served, p, usable),
		serving.CheckOptions{ClientRoutes: true, Concurrency: 4, Timeout: timeout})
}
