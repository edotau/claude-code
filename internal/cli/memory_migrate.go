package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/outfmt"
)

const memoryMigrateUsage = `usage: claude-code memory migrate [--dry-run] [--origin SLUG=PATH]... [--format text|json]
       claude-code memory migrate --apply MANIFEST [--supersede]`

// originFlag collects repeatable --origin SLUG=PATH pins.
type originFlag map[string]string

func (o originFlag) String() string { return fmt.Sprint(map[string]string(o)) }

func (o originFlag) Set(v string) error {
	slug, path, ok := strings.Cut(v, "=")
	if !ok || slug == "" || path == "" {
		return fmt.Errorf("--origin %q: want SLUG=PATH", v)
	}
	o[slug] = path
	return nil
}

// cmdMemoryMigrate: the default dry run inventories the legacy trees and writes only a manifest; --apply consumes
// that exact manifest. Exit 0 only on a complete apply; 2 when the box is not quiesced or the manifest is stale.
func cmdMemoryMigrate(args []string) int {
	fs := flag.NewFlagSet("memory migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", true, "inventory and write a manifest only (the default)")
	apply := fs.String("apply", "", "apply this manifest (implies not --dry-run)")
	supersede := fs.Bool("supersede", false, "with --apply: set another manifest's unfinished journal aside (kept as *.journal.superseded.json)")
	origins := originFlag{}
	fs.Var(origins, "origin", "pin a legacy slug to its checkout: SLUG=PATH (repeatable)")
	format := outfmt.Flag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return fail("memory migrate: unexpected argument %q (%s)", fs.Arg(0), memoryMigrateUsage)
	}
	if err := outfmt.Check(*format); err != nil {
		fail("memory migrate: %v", err)
		return 2
	}
	if *apply != "" {
		if *format == outfmt.JSON {
			return fail("memory migrate: --format json only applies to a dry run; --apply streams progress text (%s)", memoryMigrateUsage)
		}
		return migrateApply(*apply, *supersede)
	}
	if *supersede {
		return fail("memory migrate: --supersede needs --apply MANIFEST (%s)", memoryMigrateUsage)
	}
	if !*dryRun {
		return fail("memory migrate: --dry-run=false needs --apply MANIFEST (%s)", memoryMigrateUsage)
	}
	return migrateDryRun(map[string]string(origins), *format)
}

func migrateDryRun(origins map[string]string, format string) int {
	m, err := memory.Inventory(memory.InventoryOptions{Origins: origins, Now: time.Now()})
	if err != nil {
		return fail("memory migrate: %v", err)
	}
	path, err := memory.WriteManifest(m)
	if err != nil {
		return fail("memory migrate: %v", err)
	}
	if format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, m)
		return 0
	}
	for _, it := range m.Items {
		src, dest := it.Source, it.Dest
		if src == "" {
			src = "(inline)"
		}
		if dest == "" {
			dest = "-"
		}
		fmt.Printf("%-10s %-14s %s -> %s (%d B)\n", it.Action, it.Kind, src, dest, it.Bytes)
	}
	for _, f := range m.Findings {
		fmt.Println("! " + f)
	}
	fmt.Printf("manifest: %s\napply: claude-code memory migrate --apply %s\n", path, path)
	return 0
}

// migrateApply runs the manifest; supersede is the explicit, journal-preserving way past another manifest's journal.
func migrateApply(path string, supersede bool) int {
	run := memory.Apply
	if supersede {
		run = memory.ApplySuperseding
	}
	res, err := run(path, os.Stdout)
	if err != nil {
		if errors.Is(err, memory.ErrNotQuiesced) || errors.Is(err, memory.ErrBankBusy) || errors.Is(err, memory.ErrManifestStale) {
			fmt.Fprintln(os.Stderr, "memory migrate:", err)
			return 2
		}
		return fail("memory migrate: %v", err)
	}
	fmt.Printf("copied %d, deleted %d, quarantined %d, skipped %d, resumed %d\n", res.Copied, res.Deleted, res.Quarantined, res.Skipped, res.Resumed)
	if res.Commit != "" {
		fmt.Println("commit: " + res.Commit)
	}
	for _, c := range res.Conflicts {
		fmt.Println("conflict: " + c)
	}
	for _, c := range res.Changed {
		fmt.Println("changed: " + c)
	}
	if !res.Complete {
		fmt.Printf("incomplete: journal %s — resolve the entries above and re-run --apply %s\n", res.Journal, path)
		return 1
	}
	fmt.Println("complete")
	if g := memory.StartupGuard(); g.Paused { // Phase 1: a finished import stays paused until the Phase 2 cutover
		fmt.Println("note: " + g.Notice)
	}
	return 0
}
