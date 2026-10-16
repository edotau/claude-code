package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

const memoryUsage = `usage: claude-code memory init [--repo R]
       claude-code memory search [--repo R] [--k 8] [--json] <terms>
       claude-code memory index [--repo R]
       claude-code memory path [--repo R]
       claude-code memory update --transcript T --cwd C --session S
       claude-code memory watch --session S --transcript T --cwd C [--owner PID] [--idle 10m]
       claude-code memory doctor [--stale-days 90] [--notice]
       claude-code memory stats
       claude-code memory timeline [--repo R] [-n 20]
       claude-code memory promote [--repo R]
       claude-code memory archive [--dupes] [--apply] [--restore] [--split N] [<topic>...]`

func cmdMemory(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, memoryUsage)
		return 2
	}
	if args[0] == "archive" {
		return cmdMemoryArchive(args[1:])
	}
	if n, err := memory.MigrateLegacy(); err != nil {
		fmt.Fprintln(os.Stderr, "memory: legacy layout migration:", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "memory: moved %d legacy entries into %s\n", n, memory.GlobalDir())
	}
	fs := flag.NewFlagSet("memory "+args[0], flag.ContinueOnError)
	repo := fs.String("repo", "", "project root (default: $CLAUDE_PROJECT_DIR, git toplevel, cwd)")
	switch args[0] {
	case "init":
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		dir, err := memory.Init(memoryRoot(*repo))
		if err != nil {
			return fail("memory init: %v", err)
		}
		fmt.Printf("bank:    %s\nglobal:  %s\nsession-start injects it; /memory:end (or the background harvest) writes it.\n", dir, memory.GlobalDir())
		return 0
	case "search":
		k := fs.Int("k", 8, "maximum hits")
		asJSON := fs.Bool("json", false, "print hits as JSON")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		if fs.NArg() == 0 {
			return fail("memory search: no query (%s)", memoryUsage)
		}
		hits := memory.SearchBanks(memoryRoot(*repo), strings.Join(fs.Args(), " "), *k)
		if *asJSON {
			b, _ := json.Marshal(hits)
			fmt.Println(string(b))
			return 0
		}
		if len(hits) == 0 {
			fmt.Println("no matching blocks (banks empty or query all stopwords)")
		}
		for _, h := range hits {
			fmt.Printf("%5.2f  %-7s %-18s %s\n", h.Score, h.Bank, h.File, h.Title())
		}
		return 0
	case "index":
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		root := memoryRoot(*repo)
		s, err := memory.SettleBank(root, time.Now())
		if err != nil {
			return fail("memory index: %v", err)
		}
		fmt.Printf("indexed %s", memory.Dir(root))
		if s.Rotated > 0 {
			fmt.Printf(" (rotated %d sessionHistory entries to archive/)", s.Rotated)
		}
		if s.Promoted > 0 {
			fmt.Printf(" (promoted %d block(s) to global)", s.Promoted)
		}
		if s.Archived > 0 {
			fmt.Printf(" (archived %d block(s))", s.Archived)
		}
		fmt.Println()
		return 0
	case "path":
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		fmt.Println(memory.Dir(memoryRoot(*repo)))
		return 0
	case "update":
		transcript := fs.String("transcript", "", "finished session's transcript")
		cwd := fs.String("cwd", "", "project root whose bank to harvest (default: cwd)")
		session := fs.String("session", "", "finished session's id (outcome recorded under state/harvest/memory)")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		root := *cwd
		if root == "" {
			root, _ = os.Getwd()
		}
		claude, _ := launch.ClaudeBinary()
		if err := memory.Update(memory.UpdateOptions{Claude: claude, Transcript: *transcript, CWD: root, Session: *session, Out: os.Stdout}); err != nil {
			return fail("memory update: %v", err)
		}
		return 0
	case "watch":
		return hooks.IdleHarvestWatch(args[1:], os.Stderr) // spawned detached by the Stop gate; its own flag set
	case "doctor":
		staleDays := fs.Int("stale-days", memory.DefaultStaleDays, "flag a bank untouched for this many days")
		notice := fs.Bool("notice", false, "refresh the cached session-start notice and print nothing")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		if *notice {
			if err := memory.WriteDoctorNotice(memoryRoot(*repo), *staleDays, time.Now()); err != nil {
				return fail("memory doctor: %v", err)
			}
			return 0
		}
		problems := memory.Doctor(memoryRoot(*repo), *staleDays, time.Now())
		if len(problems) == 0 {
			fmt.Println("memory doctor: no problems found")
			return 0
		}
		for _, p := range problems {
			fmt.Println("- " + p)
		}
		return 0
	case "stats":
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		fmt.Print(memory.CollectBankStats().Report())
		return 0
	case "timeline":
		n := fs.Int("n", 20, "max entries")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		entries, sessions := memory.Timeline(memoryRoot(*repo), *n)
		for _, e := range entries {
			fmt.Printf("%s  %-8s %s\n", e.Time.Format("2006-01-02 15:04"), e.Kind, e.Line)
		}
		if len(sessions) > 0 {
			fmt.Println("-- sessionHistory --")
			for _, s := range sessions {
				fmt.Println("- " + s)
			}
		}
		return 0
	case "promote":
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		root := memoryRoot(*repo)
		s, err := memory.SettleBank(root, time.Now())
		if err != nil {
			return fail("memory promote: %v", err)
		}
		if !s.Any() {
			fmt.Println("nothing to settle (durable files under their caps, history under its entry cap)")
			return 0
		}
		if s.Promoted > 0 {
			fmt.Printf("promoted %d block(s) local -> global (%s), pruned local back under cap\n", s.Promoted, memory.GlobalDir())
		}
		if s.Archived > 0 {
			fmt.Printf("archived %d settled block(s) to %s, pruned back under cap\n", s.Archived, filepath.Join(memory.Dir(root), "archive"))
		}
		if s.Rotated > 0 {
			fmt.Printf("rotated %d sessionHistory entries to archive/\n", s.Rotated)
		}
		return 0
	}
	return fail("memory: unknown subcommand %q (%s)", args[0], memoryUsage)
}

// memoryRoot resolves --repo → $CLAUDE_PROJECT_DIR (only when it is a repo or the config dir) → git toplevel → cwd.
// A launcher may stamp CLAUDE_PROJECT_DIR with a non-repo parent (e.g. $HOME); honoring that attached every
// bank verb to a junk root instead of the repo the user is standing in.
func memoryRoot(repo string) string {
	if repo != "" {
		abs, _ := filepath.Abs(repo)
		return abs
	}
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" && (paths.SamePath(d, paths.ConfigDir()) || paths.SamePath(memory.GitToplevel(d), d)) {
		return d
	}
	cwd, _ := os.Getwd()
	if top := memory.GitToplevel(cwd); top != "" {
		return top
	}
	return cwd
}
