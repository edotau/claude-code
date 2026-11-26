package memory

import (
	"context"
	"errors"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// leaseClient embeds each text as a 3-dim vector and counts the texts sent.
func leaseClient(calls *int, hook func()) *EmbedClient {
	return &EmbedClient{Model: "fake-gte", EmbedFunc: func(_ context.Context, texts []string) ([][]float32, error) {
		*calls += len(texts)
		if hook != nil {
			hook()
		}
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0.1}
		}
		return out, nil
	}}
}

// backdate rewrites the record's MintedAt to age ago.
func backdate(t *testing.T, age time.Duration) vectorRecord {
	t.Helper()
	f := loadEmbedCache()
	f.MintedAt = time.Now().Add(-age).UnixNano()
	if err := saveEmbedCache(f); err != nil {
		t.Fatal(err)
	}
	return f
}

// leaseEnv is the shared state of the 1h-lease walkthrough: one bank root, one client, one call counter.
type leaseEnv struct {
	t     *testing.T
	ctx   context.Context
	root  string
	c     *EmbedClient
	calls int
}

const lease3Bank = "# D\n\n## 2026-10-01 — gamma <!-- id:aaaa0001 -->\n\nb1\n\n## 2026-10-02 — delta <!-- id:aaaa0002 -->\n\nb2\n\n## 2026-10-03 — eps <!-- id:aaaa0003 -->\n\nb3\n"

// The 1h lease end to end: mint, no-op inside it, re-mint after it, the 52m override, the race, partial run, recall.
func TestEmbedLeaseEndToEnd(t *testing.T) {
	isolatedConfig1(t)
	write(t, filepath.Join(paths.MemoryStoreDir(), "decisionLog.md"),
		"# D\n\n## 2026-10-01 — gamma <!-- id:aaaa0001 -->\n\nb1\n\n## 2026-10-02 — delta <!-- id:aaaa0002 -->\n\nb2\n")
	e := &leaseEnv{t: t, ctx: context.Background(), root: t.TempDir()}
	e.c = leaseClient(&e.calls, nil)
	e.firstMint()
	e.validInside()
	e.remintAfterExpiry()
	e.overrideWindow()
	e.raceKeepsWinner()
	e.partialKeepsMintedAt()
	e.recallReadsExpired()
}

// (a) the first mint writes the record
func (e *leaseEnv) firstMint() {
	e.t.Helper()
	n, total, err := embedChanged(e.ctx, e.root, e.c)
	rec := loadEmbedCache()
	if err != nil || n != 2 || total != 2 || e.calls != 2 || rec.MintedAt <= 0 || len(rec.Rows) != 2 {
		e.t.Fatalf("first mint: n=%d total=%d calls=%d minted=%d rows=%d err=%v", n, total, e.calls, rec.MintedAt, len(rec.Rows), err)
	}
}

// (b) inside validity: no endpoint call, errEmbedValid, record unchanged
func (e *leaseEnv) validInside() {
	e.t.Helper()
	rec := loadEmbedCache()
	if _, _, err := embedChanged(e.ctx, e.root, e.c); !errors.Is(err, errEmbedValid) || e.calls != 2 {
		e.t.Fatalf("valid record must not mint: calls=%d err=%v", e.calls, err)
	}
	if got := loadEmbedCache(); got.MintedAt != rec.MintedAt || len(got.Rows) != 2 {
		e.t.Fatalf("record changed inside validity: %+v", got)
	}
}

// (c) just under 1h is still valid; past 1h re-mints and resets MintedAt
func (e *leaseEnv) remintAfterExpiry() {
	e.t.Helper()
	backdate(e.t, embedValidity-time.Minute)
	if _, _, err := embedChanged(e.ctx, e.root, e.c); !errors.Is(err, errEmbedValid) {
		e.t.Fatalf("59m old record must be valid: %v", err)
	}
	old := backdate(e.t, embedValidity+time.Second)
	write(e.t, filepath.Join(paths.MemoryStoreDir(), "decisionLog.md"), lease3Bank)
	before := e.calls
	if n, _, err := embedChanged(e.ctx, e.root, e.c); err != nil || n != 1 || e.calls != before+1 {
		e.t.Fatalf("expired record must re-mint the changed rows: n=%d calls=%d err=%v", n, e.calls-before, err)
	}
	if got := loadEmbedCache(); got.MintedAt <= old.MintedAt {
		e.t.Fatalf("re-mint did not reset MintedAt: %d <= %d", got.MintedAt, old.MintedAt)
	}
}

// (d) override refused at 51m, allowed at 52m and re-embeds every row
func (e *leaseEnv) overrideWindow() {
	e.t.Helper()
	backdate(e.t, 51*time.Minute)
	before := e.calls
	if _, _, err := reembedEntries(e.ctx, e.root, e.c); !errors.Is(err, errReembedEarly) || e.calls != before {
		e.t.Fatalf("51m override must be refused without a call: calls=%d err=%v", e.calls-before, err)
	}
	if _, open, _ := reembedWindow(time.Now()); open {
		e.t.Error("reembedWindow open at 51m")
	}
	backdate(e.t, 52*time.Minute+time.Second)
	if _, open, _ := reembedWindow(time.Now()); !open {
		e.t.Error("reembedWindow closed at 52m")
	}
	if n, total, err := reembedEntries(e.ctx, e.root, e.c); err != nil || n != 3 || total != 3 || e.calls != before+3 {
		e.t.Fatalf("52m override must re-embed all rows: n=%d total=%d calls=%d err=%v", n, total, e.calls-before, err)
	}
}

// (e) race: a newer valid record lands between the network call and the commit and survives it
func (e *leaseEnv) raceKeepsWinner() {
	e.t.Helper()
	backdate(e.t, 2*time.Hour)
	var winner int64
	racer := leaseClient(&e.calls, func() {
		f := loadEmbedCache()
		f.MintedAt = time.Now().UnixNano()
		winner = f.MintedAt
		f.Rows = map[string]vectorRow{"winner": {Hash: "w", Vec: encodeVec([]float32{1, 0, 0})}}
		if err := saveEmbedCache(f); err != nil {
			e.t.Error(err)
		}
	})
	_, _, _ = reembedEntries(e.ctx, e.root, racer)
	got := loadEmbedCache()
	if got.MintedAt != winner || len(got.Rows) != 1 || got.Rows["winner"].Hash != "w" {
		e.t.Fatalf("the other process's record was overwritten: minted=%d want %d rows=%d", got.MintedAt, winner, len(got.Rows))
	}
}

// (f) a partial run (embed error) leaves MintedAt unchanged
func (e *leaseEnv) partialKeepsMintedAt() {
	e.t.Helper()
	old := backdate(e.t, 2*time.Hour)
	failing := &EmbedClient{Model: e.c.Model, EmbedFunc: func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("boom")
	}}
	if _, _, err := embedChanged(e.ctx, e.root, failing); err == nil {
		e.t.Fatal("want the embed error")
	}
	if got := loadEmbedCache(); got.MintedAt != old.MintedAt {
		e.t.Fatalf("partial run moved MintedAt: %d -> %d", old.MintedAt, got.MintedAt)
	}
}

// (g) embedQuery still reads an expired record
func (e *leaseEnv) recallReadsExpired() {
	e.t.Helper()
	if _, _, err := embedChanged(e.ctx, e.root, e.c); err != nil {
		e.t.Fatal(err)
	}
	backdate(e.t, 3*time.Hour)
	prev := openEmbedClient
	e.t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return e.c, nil }
	e.t.Setenv("CLAUDE_PROFILE", "fake")
	e.t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	if _, err := embedQuery(context.Background(), "gamma"); err != nil {
		e.t.Fatalf("an expired record must still serve recall: %v", err)
	}
}

// leaseFixture seeds one decisionLog entry and returns the bank root.
func leaseFixture(t *testing.T) string {
	t.Helper()
	isolatedConfig1(t)
	write(t, filepath.Join(paths.MemoryStoreDir(), "decisionLog.md"), "# D\n\n## 2026-10-01 — gamma <!-- id:aaaa0001 -->\n\nb1\n")
	return t.TempDir()
}

// readClaim returns the live mint claim's expiry and owner.
func readClaim(t *testing.T) (exp time.Time, owner int64, ok bool) {
	t.Helper()
	f := vectorStore().(*fakeVectors)
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Unix(0, f.claimExp), f.owner, f.claim
}

// A live claim held by someone else yields errEmbedBusy with no endpoint call and no commit, even with nothing pending.
func TestEmbedBusyClaimBlocksEveryRun(t *testing.T) {
	root := leaseFixture(t)
	ctx := context.Background()
	var calls int
	c := leaseClient(&calls, nil)
	if _, _, err := embedChanged(ctx, root, c); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // a second root with no entries of its own: nothing is pending for it
	old := backdate(t, embedValidity+time.Minute)
	now := time.Now()
	if _, ok, err := vectorStore().Claim(42, now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	before := calls
	for name, r := range map[string]string{"pending root": root, "empty-pending root": other} {
		if _, _, err := embedChanged(ctx, r, c); !errors.Is(err, errEmbedBusy) {
			t.Fatalf("%s: want errEmbedBusy, got %v", name, err)
		}
	}
	assertBusyClaimUnchanged(t, calls, before, old)
}

func assertBusyClaimUnchanged(t *testing.T, calls, before int, old vectorRecord) {
	t.Helper()
	if calls != before {
		t.Fatalf("a busy run called the endpoint %d times", calls-before)
	}
	if got := loadEmbedCache(); got.MintedAt != old.MintedAt {
		t.Fatalf("a busy run bumped MintedAt %d -> %d, which would discard the in-flight mint", old.MintedAt, got.MintedAt)
	}
	if _, owner, ok := readClaim(t); !ok || owner != 42 {
		t.Fatalf("the other owner's claim must survive: ok=%v owner=%d", ok, owner)
	}
}

// The claim owner is a random token apart from the start time, and its expiry follows the ctx deadline.
func TestEmbedClaimOwnerAndExpiry(t *testing.T) {
	root := leaseFixture(t)
	var calls int
	var exp time.Time
	var owner int64
	c := leaseClient(&calls, func() {
		var ok bool
		if exp, owner, ok = readClaim(t); !ok {
			t.Error("no claim held during the mint")
		}
	})
	deadline := time.Now().Add(10 * time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	start := time.Now().UnixNano()
	if _, _, err := embedChanged(ctx, root, c); err != nil {
		t.Fatal(err)
	}
	if owner == 0 || owner >= start && owner <= time.Now().UnixNano() {
		t.Errorf("owner %d looks like a timestamp, not a random token", owner)
	}
	if want := deadline.Add(embedClaimSlack); !exp.Equal(want) {
		t.Errorf("claim expiry %s, want ctx deadline + slack %s", exp, want)
	}
	if _, _, ok := readClaim(t); ok {
		t.Error("the claim must be dropped by the commit")
	}
}

// A newer record landing mid-mint discards this run and says so; the claim is still released.
func TestEmbedSupersededIsReported(t *testing.T) {
	root := leaseFixture(t)
	var calls int
	c := leaseClient(&calls, func() {
		f := loadEmbedCache()
		f.MintedAt = time.Now().UnixNano()
		f.Rows = map[string]vectorRow{"winner": {Hash: "w", Vec: encodeVec([]float32{1, 0, 0})}}
		if err := saveEmbedCache(f); err != nil {
			t.Error(err)
		}
	})
	n, _, err := embedChanged(context.Background(), root, c)
	if !errors.Is(err, errEmbedSuperseded) || n != 0 {
		t.Fatalf("want errEmbedSuperseded and 0 embedded, got n=%d err=%v", n, err)
	}
	if _, _, ok := readClaim(t); ok {
		t.Error("the claim outlived a discarded mint")
	}
}

// A commit that errors releases our claim instead of leaving it to the TTL.
func TestEmbedCommitErrorReleasesClaim(t *testing.T) {
	root := leaseFixture(t)
	var calls int
	c := leaseClient(&calls, func() { vectorStore().(*fakeVectors).putErr = errors.New("write lock timeout") })
	if _, _, err := embedChanged(context.Background(), root, c); err == nil {
		t.Fatal("want the commit error")
	}
	if _, _, ok := readClaim(t); ok {
		t.Fatal("a failed commit left our claim behind")
	}
}

// A MintedAt in the future (clock skew) is not valid: it mints again and the override is open.
func TestEmbedFutureMintedAtIsNotValid(t *testing.T) {
	root := leaseFixture(t)
	var calls int
	c := leaseClient(&calls, nil)
	if _, _, err := embedChanged(context.Background(), root, c); err != nil {
		t.Fatal(err)
	}
	backdate(t, -time.Hour)
	if _, open, err := reembedWindow(time.Now()); err != nil || !open {
		t.Fatalf("future record must open the override: open=%v err=%v", open, err)
	}
	if _, _, err := embedChanged(context.Background(), root, c); errors.Is(err, errEmbedValid) || err != nil {
		t.Fatalf("future record must mint, got %v", err)
	}
	if got := loadEmbedCache(); got.MintedAt > time.Now().UnixNano() {
		t.Fatal("MintedAt still in the future after the re-mint")
	}
}

// Another model's cache is ignored even inside its lease: the new model re-embeds every row and owns the record.
func TestEmbedModelChangeIgnoresForeignLease(t *testing.T) {
	root := leaseFixture(t)
	var calls int
	c := leaseClient(&calls, nil)
	if _, _, err := embedChanged(context.Background(), root, c); err != nil {
		t.Fatal(err)
	}
	other := &EmbedClient{Model: "other-model", EmbedFunc: c.EmbedFunc}
	n, _, err := embedChanged(context.Background(), root, other)
	if err != nil || n != 1 || calls != 2 {
		t.Fatalf("model change inside lease: n=%d calls=%d err=%v", n, calls, err)
	}
	if rec := loadEmbedCache(); rec.Model != other.Model {
		t.Fatalf("foreign cache survived the model change: %+v", rec)
	}
}

func TestEmbedDimensionMigrationLeavesRepairLeaseOpen(t *testing.T) {
	root := leaseFixture(t)
	bankFile := filepath.Join(paths.MemoryStoreDir(), "decisionLog.md")
	write(t, bankFile, lease3Bank[:strings.LastIndex(lease3Bank, "\n\n##")]+"\n")
	ctx := context.Background()
	if _, _, err := embedChanged(ctx, root, dimEmbedClient(3, nil)); err != nil {
		t.Fatal(err)
	}
	old := backdate(t, 2*time.Hour)
	write(t, bankFile, lease3Bank)
	c := dimEmbedClient(2, nil)
	n, total, err := embedChanged(ctx, root, c)
	if err != nil || n != 1 || total != 3 {
		t.Fatalf("dimension migration: n=%d total=%d err=%v", n, total, err)
	}
	rec := loadEmbedCache()
	if rec.MintedAt != old.MintedAt {
		t.Fatalf("incomplete migration renewed lease: minted=%d want=%d", rec.MintedAt, old.MintedAt)
	}
	n, _, err = embedChanged(ctx, root, c)
	if err != nil || n != 2 {
		t.Fatalf("immediate repair must embed the two dropped rows: n=%d err=%v", n, err)
	}
	if _, _, err := embedChanged(ctx, root, c); !errors.Is(err, errEmbedValid) {
		t.Fatalf("complete repair must renew the lease: %v", err)
	}
}

func TestEmbedPutRejectsTakenOverClaim(t *testing.T) {
	isolatedConfig1(t)
	now := time.Now()
	if _, ok, err := vectorStore().Claim(11, now.Add(-2*time.Minute), now.Add(-time.Minute)); err != nil || !ok {
		t.Fatalf("seed expired claim: ok=%v err=%v", ok, err)
	}
	if _, ok, err := vectorStore().Claim(22, now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("take over expired claim: ok=%v err=%v", ok, err)
	}
	merged := false
	stale := vectorWrite{Owner: 11, Started: now.UnixNano(), Merge: func(vectorRecord) vectorRecord {
		merged = true
		return vectorRecord{MintedAt: time.Now().UnixNano(), Rows: map[string]vectorRow{"stale": {Hash: "old"}}}
	}}
	stored, err := vectorStore().Put(stale)
	if err != nil || stored || merged {
		t.Fatalf("displaced owner committed: stored=%v merged=%v err=%v", stored, merged, err)
	}
	assertTakenOverClaimCommits(t, now)
}

func assertTakenOverClaimCommits(t *testing.T, started time.Time) {
	t.Helper()
	if _, owner, ok := readClaim(t); !ok || owner != 22 {
		t.Fatalf("displaced owner changed successor's claim: ok=%v owner=%d", ok, owner)
	}
	if rec := loadEmbedCache(); rec.MintedAt != 0 || len(rec.Rows) != 0 {
		t.Fatalf("displaced owner wrote a cache record: %+v", rec)
	}
	stored, err := vectorStore().Put(vectorWrite{Owner: 22, Started: started.UnixNano(), Merge: func(vectorRecord) vectorRecord {
		return vectorRecord{MintedAt: time.Now().UnixNano(), Rows: map[string]vectorRow{"winner": {Hash: "new"}}}
	}})
	if err != nil || !stored {
		t.Fatalf("successor's commit was discarded: stored=%v err=%v", stored, err)
	}
	if rec := loadEmbedCache(); rec.Rows["winner"].Hash != "new" {
		t.Fatalf("successor's row missing: %+v", rec)
	}
}

// A read error on the shared cache is reported as unreadable, not as an empty cache.
func TestNewSemanticReadErrorIsReported(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	t.Setenv("CLAUDE_PROFILE", "dev")
	if err := saveEmbedCache(vectorRecord{Model: "gte", Rows: map[string]vectorRow{"aaaa0001": {Hash: "h"}}}); err != nil {
		t.Fatal(err)
	}
	prevRead, prevOpen := readEmbedCache, openEmbedClient
	t.Cleanup(func() { readEmbedCache, openEmbedClient = prevRead, prevOpen })
	readEmbedCache = func() (vectorRecord, error) { return vectorRecord{}, errors.New("timeout") }
	openEmbedClient = func(_ context.Context, _, model string) (*EmbedClient, error) {
		return &EmbedClient{Model: model, EmbedFunc: func(context.Context, []string) ([][]float32, error) { return [][]float32{{1}}, nil }}, nil
	}
	if _, err := embedQuery(context.Background(), "q"); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unreadable cache: err %v, want 'unreadable'", err)
	}
}

// Given a rows load that waits for the query request, When embedQuery runs, Then the request left before the rows came.
func TestEmbedQueryRequestLeavesBeforeRowsLoad(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	t.Setenv("CLAUDE_PROFILE", "fake")
	rows := map[string]vectorRow{"aaaa0001": {Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}}
	if err := saveEmbedCache(vectorRecord{Model: "gte", Rows: rows}); err != nil {
		t.Fatal(err)
	}
	sent := make(chan struct{})
	prevOpen, prevRead := openEmbedClient, readEmbedCache
	t.Cleanup(func() { openEmbedClient, readEmbedCache = prevOpen, prevRead })
	openEmbedClient = func(_ context.Context, _, model string) (*EmbedClient, error) {
		return &EmbedClient{Model: model, EmbedFunc: func(context.Context, []string) ([][]float32, error) {
			close(sent)
			return [][]float32{{1, 0, 0}}, nil
		}}, nil
	}
	readEmbedCache = func() (vectorRecord, error) {
		select {
		case <-sent:
			return vectorRecord{Model: "gte", Rows: rows}, nil
		case <-time.After(300 * time.Millisecond):
			return vectorRecord{}, errors.New("the query request had not left before the rows load")
		}
	}
	if _, err := embedQuery(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
}

// A success removes only a backoff marker older than its own attempt; one written mid-query by a timeout stays.
func TestNewSemanticSuccessKeepsFreshBackoffMarker(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	t.Setenv("CLAUDE_PROFILE", "fake")
	f := vectorRecord{Model: "gte", Rows: map[string]vectorRow{"aaaa0001": {Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}}}
	if err := saveEmbedCache(f); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(paths.MemoryStateDir(), embedCachePre+"fake.backoff")
	write(t, marker, "")
	stale := time.Now().Add(-2 * embedBackoff)
	if err := os.Chtimes(marker, stale, stale); err != nil {
		t.Fatal(err)
	}
	var midQuery func()
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(_ context.Context, profile, model string) (*EmbedClient, error) {
		return &EmbedClient{Model: model, EmbedFunc: func(context.Context, []string) ([][]float32, error) {
			if midQuery != nil {
				midQuery()
			}
			return [][]float32{{1, 0, 0}}, nil
		}}, nil
	}
	if _, err := embedQuery(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a stale marker must be removed on success: %v", err)
	}
	midQuery = func() { time.Sleep(30 * time.Millisecond); write(t, marker, "") } // file mtimes tick coarsely
	if _, err := embedQuery(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a marker written during the query was deleted: %v", err)
	}
}

func assertNoVectors(t *testing.T, s VectorStore) {
	t.Helper()
	rec, err := s.Vectors()
	h, herr := s.Head()
	if err != nil || herr != nil || rec.Model != "" || len(rec.Rows) != 0 || h != (vectorHead{}) {
		t.Fatalf("an unlinked store must read empty: %+v %+v %v %v", rec, h, err, herr)
	}
	if s.Present() || s.RefreshDue(time.Now()) || s.Release(1) != nil {
		t.Fatal("a missing store must be absent, never due, and release without error")
	}
}

// With no store linked, nothing is stored, probed or minted and a mint reports why.
func TestVectorSeamNilFallsBack(t *testing.T) {
	root := leaseFixture(t)
	prev := OpenVectorStore
	t.Cleanup(func() { OpenVectorStore = prev })
	OpenVectorStore = nil
	assertNoVectors(t, vectorStore())
	var calls int
	if _, _, err := embedChanged(context.Background(), root, leaseClient(&calls, nil)); !errors.Is(err, errNoVectorStore) || calls != 0 {
		t.Fatalf("mint with no store: calls=%d err=%v", calls, err)
	}
}

func TestEmbedCorpusReadFailureReleasesClaim(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	root := leaseFixture(t)
	bankFile := filepath.Join(paths.MemoryStoreDir(), "decisionLog.md")
	backup := bankFile + ".saved"
	var calls int
	c := leaseClient(&calls, func() {
		if err := os.Rename(bankFile, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(bankFile, 0o700); err != nil {
			t.Fatal(err)
		}
	})
	n, total, err := embedChanged(context.Background(), root, c)
	if !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("post-embedding corpus read must fail with EISDIR: %v", err)
	}
	wantEmbedded(t, "failed corpus read", n, calls, nil, 1, 1)
	if total != 1 {
		t.Fatalf("live entries = %d, want 1", total)
	}
	if err := os.Remove(bankFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, bankFile); err != nil {
		t.Fatal(err)
	}
	n, _, err = embedChanged(context.Background(), root, leaseClient(&calls, nil))
	if err != nil {
		t.Fatalf("immediate retry after corpus repair must not retain the minting claim: %v", err)
	}
	wantEmbedded(t, "immediate retry", n, calls, err, 1, 2)
}

// Given a cache of another model, When the query is refused locally, Then no endpoint backoff marker is written.
func TestNewSemanticMismatchWritesNoBackoff(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	t.Setenv("CLAUDE_PROFILE", "fake")
	f := vectorRecord{Model: "gte", Rows: map[string]vectorRow{"aaaa0001": {Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}}}
	if err := saveEmbedCache(f); err != nil {
		t.Fatal(err)
	}
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(_ context.Context, profile, model string) (*EmbedClient, error) {
		return &EmbedClient{Model: "other-gte"}, nil
	}
	_, err := embedQuery(context.Background(), "q")
	if _, statErr := os.Stat(filepath.Join(paths.MemoryStateDir(), embedCachePre+"fake.backoff")); !os.IsNotExist(statErr) {
		t.Errorf("a local model mismatch wrote the endpoint backoff marker: %v", statErr)
	}
	if !errors.Is(err, errEmbedMismatch) {
		t.Errorf("embedQuery = %v, want the model mismatch", err)
	}
}

func synthHits(n, buckets int, seed int64) []scoredHit {
	r := rand.New(rand.NewSource(seed))
	all := make([]scoredHit, n)
	for i := range all {
		all[i] = scoredHit{cos: float64(r.Intn(buckets)) / float64(buckets), idx: i}
	}
	return all
}

func stableTopK(all []scoredHit, k int) []scoredHit {
	c := append([]scoredHit(nil), all...)
	sort.SliceStable(c, func(i, j int) bool { return c[i].cos > c[j].cos })
	var out []scoredHit
	out = append(out, c[:min(max(k, 0), len(c))]...)
	return out
}

func TestTopKMatchesStableSort(t *testing.T) {
	all := synthHits(300, 10, 1)
	for _, k := range []int{0, 1, 8, len(all), len(all) + 5} {
		got, want := topK(all, k), stableTopK(all, k)
		if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
			t.Fatalf("k=%d mismatch", k)
		}
	}
}

func TestTopKEmpty(t *testing.T) {
	if got := topK(nil, 8); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func BenchmarkCandidatesTopK(b *testing.B) {
	all := synthHits(20000, 1000, 2)
	b.Run("sort", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = stableTopK(all, 8)
		}
	})
	b.Run("heap", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = topK(all, 8)
		}
	})
}

// fakeVectors is an in-memory VectorStore keyed by the state dir, so every test's temp dir has its own.
type fakeVectors struct {
	mu       sync.Mutex
	rec      vectorRecord
	has      bool
	claim    bool
	claimExp int64
	owner    int64
	putErr   error
}

var fakeVecs sync.Map

func init() {
	OpenVectorStore = func() VectorStore {
		v, _ := fakeVecs.LoadOrStore(paths.MemoryStateDir(), &fakeVectors{})
		return v.(*fakeVectors)
	}
}

func (f *fakeVectors) Present() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.has || f.claim }

func (f *fakeVectors) Vectors() (vectorRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.copyRec(), nil
}

func (f *fakeVectors) copyRec() vectorRecord {
	out := f.rec
	out.Rows = maps.Clone(f.rec.Rows)
	if out.Rows == nil {
		out.Rows = map[string]vectorRow{}
	}
	return out
}

func (f *fakeVectors) Head() (vectorHead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return vectorHead{Model: f.rec.Model, MintedAt: f.rec.MintedAt, Rows: len(f.rec.Rows) > 0}, nil
}

func (f *fakeVectors) Claim(owner int64, now, expiry time.Time) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claim && time.Unix(0, f.claimExp).After(now) {
		return time.Unix(0, f.claimExp), false, nil
	}
	f.claim, f.claimExp, f.owner = true, expiry.UnixNano(), owner
	return time.Time{}, true, nil
}

func (f *fakeVectors) Release(owner int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claim && f.owner == owner {
		f.claim = false
	}
	return nil
}

func (f *fakeVectors) Put(w vectorWrite) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		err := f.putErr
		f.putErr = nil
		return false, err
	}
	if !f.claim || f.owner != w.Owner {
		return false, nil
	}
	f.claim = false
	if f.rec.MintedAt >= w.Started && f.rec.MintedAt <= time.Now().UnixNano() {
		return false, nil
	}
	f.put(w.Merge(f.copyRec()))
	time.Sleep(time.Millisecond) // a real commit outlasts the clock tick; the next run must start after MintedAt
	return true, nil
}

func (f *fakeVectors) put(r vectorRecord) { f.rec, f.has = r, true }

func (f *fakeVectors) RefreshDue(now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	live := f.claim && time.Unix(0, f.claimExp).After(now)
	return !embedValid(f.rec, now) && !live
}
