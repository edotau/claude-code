package opencode

import (
	"cmp"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

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
