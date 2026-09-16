package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/memory"
)

const memoryUsage = "usage: claude-code memory init [--repo R] | search [--repo R] [--k 8] [--json] <terms> | index [--repo R] | path [--repo R] | update --transcript T --cwd C --session S"

func cmdMemory(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, memoryUsage)
		return 2
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
		rotated, err := memory.SettleBank(root, time.Now())
		if err != nil {
			return fail("memory index: %v", err)
		}
		fmt.Printf("indexed %s", memory.Dir(root))
		if rotated > 0 {
			fmt.Printf(" (rotated %d sessionHistory entries to archive/)", rotated)
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
	}
	return fail("memory: unknown subcommand %q (%s)", args[0], memoryUsage)
}

// memoryRoot resolves --repo → $CLAUDE_PROJECT_DIR → git toplevel → cwd.
func memoryRoot(repo string) string {
	if repo != "" {
		abs, _ := filepath.Abs(repo)
		return abs
	}
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		return d
	}
	cwd, _ := os.Getwd()
	if top := memory.GitToplevel(cwd); top != "" {
		return top
	}
	return cwd
}
