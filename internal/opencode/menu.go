package opencode

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// MenuRow is one `opencode catalog list` line; Live, Ready and Check are set only for a live listing.
type MenuRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Responses bool   `json:"responses"`
	Source    string `json:"source"`
	Live      string `json:"live,omitempty"`
	Ready     string `json:"ready,omitempty"`
	Check     string `json:"check,omitempty"`
}

// MenuRows lists the shared menu; live marks each entry served or missing on provider's workspace.
func MenuRows(live bool, provider string) ([]MenuRow, error) {
	src := "default"
	if providers.OpencodeMenuFromVault() {
		src = "vault"
	}
	var served map[string]providers.ModelInfo
	var p *providers.Provider
	var cache map[string]serving.CheckResult
	if live {
		var err error
		if served, p, err = OpencodeServed(provider); err != nil {
			return nil, err
		}
		cache, _ = LoadCheckCache(p.Name)
	}
	var rows []MenuRow
	for _, m := range providers.OpencodeMenu() {
		r := MenuRow{ID: m.ID, Label: m.Label, Responses: m.Responses, Source: src}
		if live {
			r.Live = "missing"
			if info, ok := ServedInfo(served, p, m.ID); ok {
				r.Live, r.Ready = "served", info.Ready
			}
			r.Check = CheckCell(cache, m.ID)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// MenuAddServed is the menu with arg (`id[=Label]`) added; unless force, the workspace must serve the id.
func MenuAddServed(arg string, responses, responsesSet, force bool, provider string) (id string, next []providers.OpencodeModel, err error) {
	id, label, _ := strings.Cut(arg, "=")
	id = MenuID(id)
	if id == "" {
		return "", nil, errors.New("empty model id")
	}
	if !force {
		served, p, err := OpencodeServed(provider)
		if err != nil {
			return "", nil, fmt.Errorf("cannot check %s against the workspace (--force skips): %v", id, err)
		}
		if _, ok := ServedInfo(served, p, id); !ok {
			return "", nil, fmt.Errorf("%s%s is not served by %s (--force skips the check)", p.ModelPrefix, id, p.Name)
		}
	}
	var r *bool
	if responsesSet {
		r = &responses
	}
	return id, MenuAdd(providers.OpencodeMenu(), id, strings.TrimSpace(label), r), nil
}

// MenuImportDiff parses file and returns the menu it describes plus what changes against the current one.
func MenuImportDiff(file string) (next []providers.OpencodeModel, changes []MenuChange, err error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	next, err = ParseMenuImport(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", file, err)
	}
	return next, DiffMenu(providers.OpencodeMenu(), next), nil
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

// MenuCheck probes the chosen endpoints and caches the results; cacheErr is a cache write failure, not a probe one.
func MenuCheck(ids []string, all bool, provider string, timeout time.Duration) (results []serving.CheckResult, cacheErr, err error) {
	served, p, err := OpencodeServed(provider)
	if err != nil {
		return nil, nil, err
	}
	targets := OpencodeCheckTargets(ids, all, served, p)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results, err = serving.Probe(ctx, p, targets, serving.CheckOptions{Concurrency: 4, Timeout: timeout})
	if err != nil { // an interrupted run is neither cached nor reported as success
		return nil, nil, err
	}
	SortCheckResults(results)
	cacheErr = SaveCheckCache(CheckCache{CheckedAt: time.Now().UTC(), Provider: p.Name, Results: results})
	return results, cacheErr, nil
}

// SyncOptions are `opencode catalog sync`'s switches.
type SyncOptions struct {
	Apply   bool
	Refresh bool
	Verify  bool
}

// SyncPlan is a discovery run merged into the shared menu; nothing is written until Seal and the caller's menu write.
type SyncPlan struct {
	Provider *providers.Provider
	Snapshot serving.Snapshot
	IDs      []string // eligible serving ids
	Menu     []providers.OpencodeModel
	Next     []providers.OpencodeModel
	Skipped  int // additions dropped because their probe failed
	Changes  []MenuChange
	Verified bool
}

// MenuSync discovers profile's serving models and merges absent ids in; existing order, labels and Responses stay.
func MenuSync(profile string, opts SyncOptions) (SyncPlan, error) {
	p, snapshot, err := OpencodeSyncModels(profile, opts.Refresh)
	if err != nil {
		return SyncPlan{}, err
	}
	ids := OpencodeSyncIDs(snapshot.Models)
	menu := providers.OpencodeMenu()
	if opts.Verify {
		if err := OpencodeSyncVerify(p, &snapshot, ids, menu); err != nil {
			return SyncPlan{}, err
		}
	}
	next, skipped := OpencodeSyncMerge(menu, ids, snapshot.Checks)
	return SyncPlan{Provider: p, Snapshot: snapshot, IDs: ids, Menu: menu, Next: next, Skipped: skipped,
		Changes: DiffMenu(menu, next), Verified: opts.Verify}, nil
}

// Seal stores the discovery snapshot in the vault when it is new or freshly verified; sealed is false when skipped.
func (pl SyncPlan) Seal() (sealed bool, err error) {
	if pl.Snapshot.CacheHit && !pl.Verified {
		return false, nil
	}
	if err := serving.Save(pl.Provider, pl.Snapshot); err != nil {
		return false, err
	}
	return true, nil
}

// SyncSummaryLines are the discovery and verification lines that head a sync report.
func SyncSummaryLines(snapshot serving.Snapshot, verified bool) []string {
	source := "live discovery"
	if snapshot.CacheHit {
		source = "encrypted cache hit"
	}
	lines := []string{fmt.Sprintf("Valid identity (not endpoint permissions); %s, discovered %s",
		source, snapshot.DiscoveredAt.Format(time.RFC3339))}
	label := "historical probes"
	if verified {
		label = "fresh probes"
	}
	ok, failed, tools := 0, 0, 0
	for _, c := range snapshot.Checks {
		if c.Status == "ok" {
			ok++
		} else {
			failed++
		}
		if c.Tools == "yes" {
			tools++
		}
	}
	if snapshot.CheckedAt.IsZero() {
		return append(lines, "Verification: listing only; models are discovered/unverified")
	}
	return append(lines,
		fmt.Sprintf("Verification (%s, %s): %d HTTP accepted, %d failed, %d tool definitions accepted",
			label, snapshot.CheckedAt.Format(time.RFC3339), ok, failed, tools),
		"Probes test HTTP acceptance, not generated answers or actual tool calls; untested models remain unverified.")
}

// WriteMenu stores next; launches rebuild their config from the menu, so nothing else is refreshed.
func WriteMenu(w io.Writer, next []providers.OpencodeModel, msg string) error {
	if err := providers.SetOpencodeMenu(next); err != nil {
		return err
	}
	fmt.Fprintf(w, "opencode catalog: %s; relaunch `claude-code opencode` to apply\n", msg)
	return nil
}

func ListMenu(w io.Writer, live bool, provider, format string) error {
	rows, err := MenuRows(live, provider)
	if err != nil {
		return err
	}
	if format == helptext.JSON {
		return helptext.WriteJSON(w, rows)
	}
	labW := len("LABEL")
	for _, r := range rows {
		labW = max(labW, len(r.Label))
	}
	idW := idWidth(len(rows), func(i int) string { return rows[i].ID })
	head := fmt.Sprintf("%-*s  %-*s  %-9s  %s", idW, "ID", labW, "LABEL", "RESPONSES", "SOURCE")
	if live {
		head += "  LIVE                  CHECK"
	}
	fmt.Fprintln(w, head)
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-9v  %s", idW, r.ID, labW, r.Label, r.Responses, r.Source)
		if live {
			line += fmt.Sprintf("  %-20s  %s", strings.TrimSpace(r.Live+" "+r.Ready), r.Check)
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

// idWidth is the widest of the "ID" header and the n ids.
func idWidth(n int, id func(int) string) int {
	w := len("ID")
	for i := range n {
		w = max(w, len(id(i)))
	}
	return w
}

func AddMenu(w io.Writer, arg string, responses, responsesSet, force bool, provider string) error {
	id, next, err := MenuAddServed(arg, responses, responsesSet, force, provider)
	if err != nil {
		return err
	}
	return WriteMenu(w, next, "added "+id)
}

func ImportMenu(w io.Writer, file string, apply bool) error {
	next, changes, err := MenuImportDiff(file)
	if err != nil {
		return err
	}
	for _, c := range changes {
		line := fmt.Sprintf("%s %s  %s", c.Op, c.Model.ID, c.Model.Label)
		if c.Op == "~" {
			line += fmt.Sprintf("  (was %q responses=%v; now responses=%v)", c.Was.Label, c.Was.Responses, c.Model.Responses)
		}
		fmt.Fprintln(w, line)
	}
	if len(changes) == 0 {
		fmt.Fprintln(w, "opencode catalog: no change")
		return nil
	}
	if !apply {
		fmt.Fprintf(w, "opencode catalog: dry run, %d change(s); pass --apply to write\n", len(changes))
		return nil
	}
	return WriteMenu(w, next, fmt.Sprintf("imported %d model(s)", len(next)))
}

func CheckMenu(w, errw io.Writer, ids []string, all bool, provider string, timeout time.Duration, format string) error {
	results, cacheErr, err := MenuCheck(ids, all, provider, timeout)
	if err != nil {
		return err
	}
	if cacheErr != nil {
		fmt.Fprintf(errw, "opencode catalog check: cache not written: %v\n", cacheErr)
	}
	if format == helptext.JSON {
		return helptext.WriteJSON(w, results)
	}
	idW := idWidth(len(results), func(i int) string { return results[i].ID })
	fmt.Fprintf(w, "%-*s  %-9s  %-4s  %-10s  %s\n", idW, "ID", "STATUS", "HTTP", "LATENCY", "TOOLS")
	for _, r := range results {
		code, lat := "-", "-"
		if r.HTTP > 0 {
			code = strconv.Itoa(r.HTTP)
		}
		if r.Status != "missing" {
			lat = fmt.Sprintf("%dms", r.LatencyMS)
		}
		tools := cmp.Or(r.Tools, "-")
		fmt.Fprintf(w, "%-*s  %-9s  %-4s  %-10s  %s", idW, r.ID, r.Status, code, lat, tools)
		if (r.Status == "fail" || r.Status == "transient") && r.Error != "" {
			fmt.Fprint(w, "  "+r.Error)
		}
		fmt.Fprintln(w)
	}
	return nil
}

func SyncMenu(w io.Writer, profile string, opts SyncOptions) error {
	pl, err := MenuSync(profile, opts)
	if err != nil {
		return err
	}
	for _, line := range SyncSummaryLines(pl.Snapshot, opts.Verify) {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "opencode catalog sync: %d listed, %d eligible, %d additions, %d failed additions skipped, %d retained, %d total\n",
		len(pl.Snapshot.Models), len(pl.IDs), len(pl.Changes), pl.Skipped, len(pl.Menu), len(pl.Next))
	for _, c := range pl.Changes {
		fmt.Fprintf(w, "%s %s  %s  responses=%v\n", c.Op, c.Model.ID, c.Model.Label, c.Model.Responses)
	}
	fmt.Fprintln(w, "Shared menu: OpenCode uses all entries; Codex needs Responses + bundled metadata.")
	fmt.Fprintln(w, "Discovery is not inference proof; use --verify or the existing check command:")
	fmt.Fprintf(w, "  claude-code opencode catalog check --all --profile %s\n", profile)
	if !opts.Apply {
		fmt.Fprintln(w, "opencode catalog: dry run; no discovery/menu writes; pass --apply to write")
		return nil
	}
	sealed, err := pl.Seal()
	if err != nil {
		return err
	}
	if sealed {
		fmt.Fprintln(w, "opencode catalog: discovery snapshot sealed in vault")
	}
	if len(pl.Changes) == 0 {
		fmt.Fprintln(w, "opencode catalog: no menu change")
		return nil
	}
	if err := WriteMenu(w, pl.Next, fmt.Sprintf("synced %d addition(s); relaunch Codex too if used", len(pl.Changes))); err != nil {
		return fmt.Errorf("cannot persist the shared model menu (check vault access): %w", err)
	}
	return nil
}
