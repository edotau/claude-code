package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/outfmt"
)

const memoryMigrateUsage = `usage: claude-code memory migrate [--dry-run] [--origin SLUG=PATH]...
                                  [--supersede] [--format text|json]
       claude-code memory migrate --manifest MANIFEST [--supersede]
Inventory, write manifest, and apply (--dry-run writes the manifest only).`

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

// cmdMemoryMigrate applies by default (inventory, manifest, apply); --dry-run stops after the manifest and
// --manifest applies a reviewed one. Exit 0 only on a complete apply (or a written dry-run manifest).
func cmdMemoryMigrate(args []string) int {
	fs := flag.NewFlagSet("memory migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of memory migrate:\n")
		helptext.Defaults(fs)
	}
	dryRun := fs.Bool("dry-run", false, "inventory and write a manifest only; apply nothing")
	manifest := fs.String("manifest", "", "apply this reviewed manifest instead of taking a fresh inventory")
	supersede := fs.Bool("supersede", false, "set aside an unfinished journal as *.journal.superseded.json")
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
	if *manifest != "" && (*dryRun || len(origins) > 0) {
		return fail("memory migrate: --manifest applies a reviewed manifest; it takes no --dry-run/--origin (%s)", memoryMigrateUsage)
	}
	if *format == outfmt.JSON && !*dryRun {
		return fail("memory migrate: --format json only applies to --dry-run; an apply streams progress text (%s)", memoryMigrateUsage)
	}
	if *manifest != "" {
		return migrateApply(*manifest, *supersede)
	}
	path, rc := migrateDryRun(map[string]string(origins), *format, *dryRun)
	if rc != 0 || *dryRun {
		return rc
	}
	return migrateApply(path, *supersede)
}

// migrateDryRun takes the inventory and writes its manifest; with preview it also prints the review hint.
func migrateDryRun(origins map[string]string, format string, preview bool) (string, int) {
	m, err := memory.Inventory(memory.InventoryOptions{Origins: origins, Now: time.Now()})
	if err != nil {
		return "", fail("memory migrate: %v", err)
	}
	path, err := memory.WriteManifest(m)
	if err != nil {
		return "", fail("memory migrate: %v", err)
	}
	if format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, m)
		return path, 0
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
	fmt.Printf("manifest: %s\n", path)
	if preview {
		fmt.Printf("apply: claude-code memory migrate --manifest %s\n", path)
	}
	return path, 0
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
	if g := memory.StartupGuard(); g.Paused {
		fmt.Println("note: " + g.Notice)
		return 0
	}
	links, err := memory.EnsureNativeLinks() // the cutover: native memory now reads and writes the store
	if err != nil {
		return fail("memory migrate: native links: %v", err)
	}
	for _, l := range links {
		fmt.Printf("link %-9s %s -> %s %s\n", l.Action, l.Link, l.Target, l.Detail)
	}
	if _, err := memory.SettleAll(time.Now()); err != nil {
		return fail("memory migrate: index: %v", err)
	}
	fmt.Println("store live: " + memory.StoreDir())
	return 0
}
