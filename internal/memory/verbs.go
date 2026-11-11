package memory

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// idRef is a block reference: a persisted 8-hex id (or id-alias) or a z-prefixed contentRef.
var idRef = regexp.MustCompile(`^(z[0-9a-f]{8}|[0-9a-f]{8}(-[a-z0-9-]+)?)$`)

// SearchOptions is one `memory search`; Expand lets a lone block reference resolve to its block.
type SearchOptions struct {
	Repo   string
	All    bool
	Query  string
	K      int
	Expand bool
}

// searchResult is the expanded block (Path, Block) when the query was a reference, else the ranked Hits.
type searchResult struct {
	Path, Block string
	Hits        []SearchHit
}

// Search ranks blocks for the query and logs the access; a block reference expands in place when allowed.
func Search(o SearchOptions) searchResult {
	now := time.Now()
	if o.Expand && idRef.MatchString(o.Query) {
		if path, block, err := ShowBlock(o.Query); err == nil {
			logShown(path, o.Query, AccessSearch, now)
			return searchResult{Path: path, Block: block}
		}
	}
	hits, root := SearchRepo(o.Repo, o.All, o.Query, o.K)
	logHits(root, hits, AccessSearch, now)
	return searchResult{Hits: hits}
}

// Show returns one block by id and logs the access.
func Show(id string) (path, block string, err error) {
	path, block, err = ShowBlock(id)
	if err != nil {
		return "", "", err
	}
	logShown(path, id, AccessShow, time.Now())
	return path, block, nil
}

// ReportRepo appends a report to repo's store, its origin registered under the bank lock so later calls find it.
func ReportRepo(repo, label string, in ReportInput) ([]string, error) {
	dir, err := EnsureRepoDir(RepoRoot(repo))
	if err != nil {
		return nil, err
	}
	return AppendReport(dir, label, in)
}

// InitBank creates the repo's bank and reports where it landed, with the init procedure on a fresh template.
func InitBank(w io.Writer, root string) error {
	dir, err := Init(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "bank:    %s\nhome:    %s\nsession-start injects it; `claude-code memory update` (or the background update) writes it.\n", dir, HomeBankDir())
	if isProjectContextTemplate(root) {
		ref, err := Reference("init")
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
	root, err := ResolveRepo(o.Repo)
	if err != nil {
		return fmt.Errorf("memory index: %w", err)
	}
	profile := cmp.Or(EmbedProfile(), providers.SessionProfile())
	if o.Reembed {
		if err := reembedGate(profile, time.Now()); err != nil {
			return err
		}
	}
	if o.MintOnly {
		embedIndex(out, errw, root, profile, o.Reembed)
		return nil
	}
	rotated, err := settleBank(root, time.Now())
	if err != nil {
		return fmt.Errorf("memory index: %w", err)
	}
	fmt.Fprintf(out, "indexed %s", Dir(root))
	if rotated > 0 {
		fmt.Fprintf(out, " (rotated %d sessionHistory entries to archive/)", rotated)
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
	at, open, err := reembedWindow(profile, now)
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
	c, err := OpenEmbedClient(ctx, profile, "")
	if err == nil {
		var n, total int
		embed := EmbedEntries
		if reembed {
			embed = reembedEntries
		}
		if n, total, err = embed(ctx, root, c); err == nil && n > 0 {
			fmt.Fprintf(out, "embedded %d of %d entries (%s)\n", n, total, c.Model)
		}
	}
	if err != nil && !errors.Is(err, ErrEmbedValid) { // a valid cache is the normal case, not a skip
		fmt.Fprintf(errw, "memory index: embeddings skipped: %v\n", err)
	}
}

// UpdateSessionOptions is one `memory update`; Run is only called once an update will really run.
type UpdateSessionOptions struct {
	Transcript, CWD, Session string
	DryRun, Historical       bool
	Run                      AgentFunc
	Out                      io.Writer
}

// resolveUpdateTarget resolves the bank root, session id and transcript an update acts on (transcript may be empty).
func resolveUpdateTarget(o UpdateSessionOptions) (root, session, transcript string, err error) {
	root = o.CWD
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return "", "", "", err
		}
	}
	session, transcript = o.Session, o.Transcript
	explicit := session != "" || transcript != ""
	if transcript == "" && session == "" { // a live session updates itself
		session = os.Getenv(SessionIDEnv)
	}
	if transcript == "" {
		transcript = LiveTranscript(root, session)
	}
	if explicit && o.CWD == "" { // an explicit session's bank follows its project, not our cwd
		root = cmp.Or(sessionRoot(transcript), root)
	}
	return root, session, transcript, nil
}

// UpdateSession resolves the live session or transcript, then updates the bank (or prints the procedure / dry run).
func UpdateSession(o UpdateSessionOptions) error {
	root, session, transcript, err := resolveUpdateTarget(o)
	if err != nil {
		return err
	}
	if transcript == "" { // no resolvable session: print the narrative procedure instead of refusing outright
		ref, err := Reference("update")
		if err != nil {
			return err
		}
		fmt.Fprintln(o.Out, ref)
		return nil
	}
	if o.DryRun {
		fmt.Fprintf(o.Out, "memory update (dry run): session %s, transcript %s, bank %s\n", session, transcript, Dir(root))
		return nil
	}
	return update(updateOptions{Run: o.Run, Transcript: transcript, CWD: root, Session: session, Historical: o.Historical, Out: o.Out})
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
		fmt.Fprint(o.Out, previewDream(root).Report())
		return nil
	}
	if !o.Force && !DreamDue(root, time.Now()) {
		fmt.Fprintln(o.Out, "memory dream: not due")
		return nil
	}
	return dream(dreamOptions{CWD: root, Run: o.Run, Out: o.Out})
}

// DigestRepo writes the repo bank's digest and returns its path.
func DigestRepo(ctx context.Context, repo, claude string) (string, error) {
	root, err := ResolveRepo(repo)
	if err != nil {
		return "", err
	}
	return Digest(ctx, Dir(root), claude)
}

// FormatReport wraps the bank files (or only lists them under check) and reports whether check found changes.
func FormatReport(w io.Writer, check bool) (dirty bool, err error) {
	changed, err := formatBank(check)
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
	root, err := ResolveRepo(o.Repo)
	if err != nil {
		return err
	}
	if o.Notice {
		return writeDoctorNotice(root, o.StaleDays, time.Now())
	}
	problems := doctor(root, o.StaleDays, time.Now())
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
	fmt.Fprint(w, collectBankStats().Report())
	if o.Candidates {
		ref, err := Reference("review")
		if err != nil {
			return err
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, ref)
	}
	return nil
}

// TimelineReport writes the n most recent bank entries, then the sessionHistory lines.
func TimelineReport(w io.Writer, repo string, n int) error {
	root, err := ResolveRepo(repo)
	if err != nil {
		return err
	}
	entries, sessions, err := timeline(root, n)
	if err != nil {
		return fmt.Errorf("sessionHistory: %w", err)
	}
	for _, e := range entries {
		fmt.Fprintf(w, "%s  update   %s\n", e.Time.Format("2006-01-02 15:04"), e.Line)
	}
	if len(sessions) > 0 {
		fmt.Fprintln(w, "-- sessionHistory --")
		for _, s := range sessions {
			fmt.Fprintln(w, "- "+s)
		}
	}
	return nil
}

//go:embed prompts/*.md
var promptsFS embed.FS

// Reference loads the model-residue prompt for verb from the embedded prompts/<verb>.md — the one
// copy of each procedure; a background job and its CLI verb print the same text.
// A missing or empty file is an error: a job must never run on a silently empty prompt.
func Reference(verb string) (string, error) {
	data, err := promptsFS.ReadFile("prompts/" + verb + ".md")
	if err != nil {
		return "", fmt.Errorf("memory reference %s: %w", verb, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", fmt.Errorf("memory reference %s: prompts/%s.md is empty", verb, verb)
	}
	return text, nil
}
