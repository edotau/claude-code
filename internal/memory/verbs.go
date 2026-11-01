package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memory/recall"
	embedding "github.com/edotau/claude-code/internal/memory/recall/embed"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/providers"
)

// idRef is a block reference: a persisted 8-hex id (or id-alias) or a z-prefixed ContentRef.
var idRef = regexp.MustCompile(`^(z[0-9a-f]{8}|[0-9a-f]{8}(-[a-z0-9-]+)?)$`)

// SearchOptions is one `memory search`; Expand lets a lone block reference resolve to its block.
type SearchOptions struct {
	Repo   string
	All    bool
	Query  string
	K      int
	Expand bool
}

// SearchResult is the expanded block (Path, Block) when the query was a reference, else the ranked Hits.
type SearchResult struct {
	Path, Block string
	Hits        []bank.SearchHit
}

// Search ranks blocks for the query and logs the access; a block reference expands in place when allowed.
func Search(o SearchOptions) SearchResult {
	now := time.Now()
	if o.Expand && idRef.MatchString(o.Query) {
		if path, block, err := bank.ShowBlock(o.Query); err == nil {
			recall.LogShown(path, o.Query, recall.AccessSearch, now)
			return SearchResult{Path: path, Block: block}
		}
	}
	hits, root := recall.SearchRepo(o.Repo, o.All, o.Query, o.K)
	recall.LogHits(root, hits, recall.AccessSearch, now)
	return SearchResult{Hits: hits}
}

// Show returns one block by id and logs the access.
func Show(id string) (path, block string, err error) {
	path, block, err = bank.ShowBlock(id)
	if err != nil {
		return "", "", err
	}
	recall.LogShown(path, id, recall.AccessShow, time.Now())
	return path, block, nil
}

// InitBank creates the repo's bank and reports where it landed, with the init procedure on a fresh template.
func InitBank(w io.Writer, root string) error {
	dir, err := Init(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "bank:    %s\nhome:    %s\nsession-start injects it; `claude-code memory update` (or the background update) writes it.\n", dir, memstore.HomeBankDir())
	if IsProjectContextTemplate(root) {
		ref, err := bank.Reference("init")
		if err != nil {
			return err
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, ref)
	}
	return nil
}

// IndexOptions is one `memory index`.
type IndexOptions struct {
	Repo     string
	Reembed  bool
	MintOnly bool
}

// Index settles the bank (unless MintOnly) and refreshes the embedding cache; errors carry their verb prefix.
func Index(out, errw io.Writer, o IndexOptions) error {
	root := memstore.Root(o.Repo)
	profile := cmp.Or(embedding.EmbedProfile(), providers.SessionProfile())
	if o.Reembed {
		if err := reembedGate(profile, time.Now()); err != nil {
			return err
		}
	}
	if o.MintOnly {
		embedIndex(out, errw, root, profile, o.Reembed)
		return nil
	}
	s, err := bank.SettleBank(root, time.Now())
	if err != nil {
		return fmt.Errorf("memory index: %w", err)
	}
	fmt.Fprintf(out, "indexed %s", memstore.Dir(root))
	if s.Rotated > 0 {
		fmt.Fprintf(out, " (rotated %d sessionHistory entries to archive/)", s.Rotated)
	}
	if s.Promoted > 0 {
		fmt.Fprintf(out, " (promoted %d block(s) to home)", s.Promoted)
	}
	if s.Archived > 0 {
		fmt.Fprintf(out, " (archived %d block(s))", s.Archived)
	}
	fmt.Fprintln(out)
	embedIndex(out, errw, root, profile, o.Reembed)
	return nil
}

// reembedGate refuses --reembed before the override window opens.
func reembedGate(profile string, now time.Time) error {
	if profile == "" {
		return nil
	}
	at, open, err := embedding.ReembedWindow(profile, now)
	if err != nil {
		return fmt.Errorf("memory index --reembed: %w", err)
	}
	if !open {
		return fmt.Errorf("memory index --reembed: embeddings are valid; the override opens at %s", at.Format("15:04"))
	}
	return nil
}

// embedIndex refreshes the entry-vector cache after an index; a missing endpoint or credential is a stderr note only.
func embedIndex(out, errw io.Writer, root, profile string, reembed bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := embedding.OpenEmbedClient(ctx, profile, "")
	if err == nil {
		var n, total int
		embed := embedding.EmbedEntries
		if reembed {
			embed = embedding.ReembedEntries
		}
		if n, total, err = embed(ctx, root, c); err == nil && n > 0 {
			fmt.Fprintf(out, "embedded %d of %d entries (%s)\n", n, total, c.Model)
		}
	}
	if err != nil && !errors.Is(err, embedding.ErrEmbedValid) { // a valid cache is the normal case, not a skip
		fmt.Fprintf(errw, "memory index: embeddings skipped: %v\n", err)
	}
}

// UpdateSessionOptions is one `memory update`; ClaudeBin is only called once an update will really run.
type UpdateSessionOptions struct {
	Transcript, CWD, Session string
	DryRun                   bool
	ClaudeBin                func() (string, error)
	Run                      AgentFunc
	Out, Err                 io.Writer
}

// UpdateSession resolves the live session or transcript, then updates the bank (or prints the procedure / dry run).
func UpdateSession(o UpdateSessionOptions) error {
	root := o.CWD
	if root == "" {
		var err error
		if root, err = os.Getwd(); err != nil {
			return err
		}
	}
	session, transcript := o.Session, o.Transcript
	if transcript == "" && session == "" { // a live session updates itself
		session = os.Getenv(SessionIDEnv)
	}
	if transcript == "" {
		transcript = LiveTranscript(root, session)
	}
	if transcript == "" { // no resolvable session: print the narrative procedure instead of refusing outright
		ref, err := bank.Reference("update")
		if err != nil {
			return err
		}
		fmt.Fprintln(o.Out, ref)
		return nil
	}
	if o.DryRun {
		fmt.Fprintf(o.Out, "memory update (dry run): session %s, transcript %s, bank %s\n", session, transcript, memstore.Dir(root))
		return nil
	}
	claude, err := o.ClaudeBin() // "" is fine here: Run handles the update, Claude is only its fallback
	if err != nil {
		fmt.Fprintln(o.Err, "claude-code: memory update:", err)
	}
	return Update(UpdateOptions{Claude: claude, Run: o.Run, Transcript: transcript, CWD: root, Session: session, Out: o.Out})
}

// DreamRunOptions is one `memory dream`.
type DreamRunOptions struct {
	CWD           string
	Force, DryRun bool
	Run           AgentFunc
	Out           io.Writer
}

// RunDream previews (DryRun), skips when not due, or consolidates the bank.
func RunDream(o DreamRunOptions) error {
	root := o.CWD
	if root == "" {
		var err error
		if root, err = os.Getwd(); err != nil {
			return err
		}
	}
	if o.DryRun {
		fmt.Fprint(o.Out, PreviewDream(root, time.Now()).Report())
		return nil
	}
	if !o.Force && !DreamDue(root, time.Now()) {
		fmt.Fprintln(o.Out, "memory dream: not due")
		return nil
	}
	return Dream(DreamOptions{CWD: root, Run: o.Run, Out: o.Out})
}

// DigestRepo writes the repo bank's digest and returns its path.
func DigestRepo(repo, claude string) (string, error) {
	return Digest(memstore.Dir(memstore.Root(repo)), claude)
}

// FormatReport wraps the bank files (or only lists them under check) and reports whether check found changes.
func FormatReport(w io.Writer, check bool) (dirty bool, err error) {
	changed, err := bank.FormatBank(check)
	if err != nil {
		return false, err
	}
	for _, f := range changed {
		fmt.Fprintln(w, f)
	}
	if check {
		if len(changed) > 0 {
			fmt.Fprintf(w, "%d file(s) would change (run `claude-code memory fmt`)\n", len(changed))
			return true, nil
		}
		fmt.Fprintln(w, "memory fmt --check: clean")
		return false, nil
	}
	fmt.Fprintf(w, "wrapped %d file(s)\n", len(changed))
	return false, nil
}

// ReviewOptions is one `memory review`.
type ReviewOptions struct {
	Repo                      string
	StaleDays                 int
	Brief, Notice, Candidates bool
}

// ReviewReport writes doctor + stats (Notice refreshes the cached session-start notice and prints nothing).
func ReviewReport(w io.Writer, o ReviewOptions) error {
	root := memstore.Root(o.Repo)
	if o.Notice {
		return WriteDoctorNotice(root, o.StaleDays, time.Now())
	}
	problems := Doctor(root, o.StaleDays, time.Now())
	switch {
	case len(problems) == 0:
		fmt.Fprintln(w, "memory doctor: no problems found")
	case o.Brief:
		fmt.Fprintf(w, "memory doctor: %d problem(s) found\n", len(problems))
	default:
		for _, p := range problems {
			fmt.Fprintln(w, "- "+p)
		}
	}
	fmt.Fprint(w, CollectBankStats().Report())
	if o.Candidates {
		ref, err := bank.Reference("review")
		if err != nil {
			return err
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, ref)
	}
	return nil
}

// TimelineReport writes the n most recent bank entries, then the sessionHistory lines.
func TimelineReport(w io.Writer, repo string, n int) {
	entries, sessions := Timeline(memstore.Root(repo), n)
	for _, e := range entries {
		fmt.Fprintf(w, "%s  %-8s %s\n", e.Time.Format("2006-01-02 15:04"), e.Kind, e.Line)
	}
	if len(sessions) > 0 {
		fmt.Fprintln(w, "-- sessionHistory --")
		for _, s := range sessions {
			fmt.Fprintln(w, "- "+s)
		}
	}
}
