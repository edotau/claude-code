package memory

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// leaseClient embeds each text as a 3-dim vector and counts the texts sent.
func leaseClient(calls *int, hook func()) *EmbedClient {
	return &EmbedClient{Profile: "fake", Model: "fake-gte", Namespace: "fake|h|fake-gte", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
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
func backdate(t *testing.T, age time.Duration) embedCacheFile {
	t.Helper()
	f := loadEmbedCache("fake")
	f.MintedAt = time.Now().Add(-age).UnixNano()
	if err := saveEmbedCache("fake", f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The 1h lease end to end: mint, no-op inside it, re-mint after it, the 52m override window, the race, partial run, recall.
func TestEmbedLeaseEndToEnd(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"),
		"# D\n\n## 2026-10-01 — gamma <!-- id:aaaa0001 -->\n\nb1\n\n## 2026-10-02 — delta <!-- id:aaaa0002 -->\n\nb2\n")
	ctx := context.Background()
	var calls int
	c := leaseClient(&calls, nil)

	// (a) the first mint writes the record
	n, total, err := EmbedEntries(ctx, root, c)
	rec := loadEmbedCache("fake")
	if err != nil || n != 2 || total != 2 || calls != 2 || rec.MintedAt <= 0 || len(rec.Rows) != 2 {
		t.Fatalf("first mint: n=%d total=%d calls=%d minted=%d rows=%d err=%v", n, total, calls, rec.MintedAt, len(rec.Rows), err)
	}

	// (b) inside validity: no endpoint call, ErrEmbedValid, record unchanged
	if _, _, err = EmbedEntries(ctx, root, c); !errors.Is(err, ErrEmbedValid) || calls != 2 {
		t.Fatalf("valid record must not mint: calls=%d err=%v", calls, err)
	}
	if got := loadEmbedCache("fake"); got.MintedAt != rec.MintedAt || len(got.Rows) != 2 {
		t.Fatalf("record changed inside validity: %+v", got)
	}

	// (c) just under 1h is still valid; past 1h re-mints and resets MintedAt
	backdate(t, embedValidity-time.Minute)
	if _, _, err = EmbedEntries(ctx, root, c); !errors.Is(err, ErrEmbedValid) {
		t.Fatalf("59m old record must be valid: %v", err)
	}
	old := backdate(t, embedValidity+time.Second)
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"),
		"# D\n\n## 2026-10-01 — gamma <!-- id:aaaa0001 -->\n\nb1\n\n## 2026-10-02 — delta <!-- id:aaaa0002 -->\n\nb2\n\n## 2026-10-03 — eps <!-- id:aaaa0003 -->\n\nb3\n")
	before := calls
	if n, _, err = EmbedEntries(ctx, root, c); err != nil || n != 1 || calls != before+1 {
		t.Fatalf("expired record must re-mint the changed rows: n=%d calls=%d err=%v", n, calls-before, err)
	}
	if got := loadEmbedCache("fake"); got.MintedAt <= old.MintedAt {
		t.Fatalf("re-mint did not reset MintedAt: %d <= %d", got.MintedAt, old.MintedAt)
	}

	// (d) override refused at 51m, allowed at 52m and re-embeds every row
	backdate(t, 51*time.Minute)
	before = calls
	if _, _, err = ReembedEntries(ctx, root, c); !errors.Is(err, ErrReembedEarly) || calls != before {
		t.Fatalf("51m override must be refused without a call: calls=%d err=%v", calls-before, err)
	}
	if _, open, _ := ReembedWindow("fake", time.Now()); open {
		t.Error("ReembedWindow open at 51m")
	}
	backdate(t, 52*time.Minute+time.Second)
	if _, open, _ := ReembedWindow("fake", time.Now()); !open {
		t.Error("ReembedWindow closed at 52m")
	}
	if n, total, err = ReembedEntries(ctx, root, c); err != nil || n != 3 || total != 3 || calls != before+3 {
		t.Fatalf("52m override must re-embed all rows: n=%d total=%d calls=%d err=%v", n, total, calls-before, err)
	}

	// (e) race: a newer valid record lands between the network call and the commit and survives it
	backdate(t, 2*time.Hour)
	var winner int64
	racer := leaseClient(&calls, func() {
		f := loadEmbedCache("fake")
		f.MintedAt = time.Now().UnixNano()
		winner = f.MintedAt
		f.Rows = map[string]embedRow{"winner": {Hash: "w", Vec: encodeVec([]float32{1, 0, 0})}}
		if err := saveEmbedCache("fake", f); err != nil {
			t.Error(err)
		}
	})
	_, _, _ = ReembedEntries(ctx, root, racer)
	got := loadEmbedCache("fake")
	if got.MintedAt != winner || len(got.Rows) != 1 || got.Rows["winner"].Hash != "w" {
		t.Fatalf("the other process's record was overwritten: minted=%d want %d rows=%d", got.MintedAt, winner, len(got.Rows))
	}

	// (f) a partial run (embed error) leaves MintedAt unchanged
	old = backdate(t, 2*time.Hour)
	failing := &EmbedClient{Profile: "fake", Model: c.Model, Namespace: c.Namespace, Embed: func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("boom")
	}}
	if _, _, err = EmbedEntries(ctx, root, failing); err == nil {
		t.Fatal("want the embed error")
	}
	if got = loadEmbedCache("fake"); got.MintedAt != old.MintedAt {
		t.Fatalf("partial run moved MintedAt: %d -> %d", old.MintedAt, got.MintedAt)
	}

	// (g) NewSemantic still reads an expired record
	if _, _, err = EmbedEntries(ctx, root, c); err != nil {
		t.Fatal(err)
	}
	backdate(t, 3*time.Hour)
	prev := OpenEmbedClient
	t.Cleanup(func() { OpenEmbedClient = prev })
	OpenEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return c, nil }
	t.Setenv("CLAUDE_PROFILE", "fake")
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	if _, err = NewSemantic("gamma"); err != nil {
		t.Fatalf("an expired record must still serve recall: %v", err)
	}
}

// leaseFixture seeds one decisionLog entry and returns the bank root.
func leaseFixture(t *testing.T) string {
	t.Helper()
	isolatedConfig1(t)
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"), "# D\n\n## 2026-10-01 — gamma <!-- id:aaaa0001 -->\n\nb1\n")
	return t.TempDir()
}

// readClaim returns the live claim's expiry and owner for profile "fake".
func readClaim(t *testing.T) (exp time.Time, owner int64, ok bool) {
	t.Helper()
	db, err := bbolt.Open(EmbedDBPath(), 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket(embedClaimBucket); b != nil {
			if v := b.Get([]byte("fake")); len(v) == 16 {
				exp, owner, ok = time.Unix(0, int64(binary.BigEndian.Uint64(v))), int64(binary.BigEndian.Uint64(v[8:])), true
			}
		}
		return nil
	})
	return exp, owner, ok
}

// A live claim held by someone else yields ErrEmbedBusy with no endpoint call and no commit, even with nothing pending.
func TestEmbedBusyClaimBlocksEveryRun(t *testing.T) {
	root := leaseFixture(t)
	ctx := context.Background()
	var calls int
	c := leaseClient(&calls, nil)
	if _, _, err := EmbedEntries(ctx, root, c); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // a second root with no entries of its own: nothing is pending for it
	old := backdate(t, embedValidity+time.Minute)
	now := time.Now()
	if _, ok, err := claimEmbedMint("fake", 42, now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	before := calls
	for name, r := range map[string]string{"pending root": root, "empty-pending root": other} {
		if _, _, err := EmbedEntries(ctx, r, c); !errors.Is(err, ErrEmbedBusy) {
			t.Fatalf("%s: want ErrEmbedBusy, got %v", name, err)
		}
	}
	if calls != before {
		t.Fatalf("a busy run called the endpoint %d times", calls-before)
	}
	if got := loadEmbedCache("fake"); got.MintedAt != old.MintedAt {
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
	if _, _, err := EmbedEntries(ctx, root, c); err != nil {
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
		f := loadEmbedCache("fake")
		f.MintedAt = time.Now().UnixNano()
		f.Rows = map[string]embedRow{"winner": {Hash: "w", Vec: encodeVec([]float32{1, 0, 0})}}
		if err := saveEmbedCache("fake", f); err != nil {
			t.Error(err)
		}
	})
	n, _, err := EmbedEntries(context.Background(), root, c)
	if !errors.Is(err, ErrEmbedSuperseded) || n != 0 {
		t.Fatalf("want ErrEmbedSuperseded and 0 embedded, got n=%d err=%v", n, err)
	}
	if _, _, ok := readClaim(t); ok {
		t.Error("the claim outlived a discarded mint")
	}
}

// A commit that errors (write lock timeout) releases our claim instead of leaving it to the TTL.
func TestEmbedCommitErrorReleasesClaim(t *testing.T) {
	root := leaseFixture(t)
	prev := embedWriteTimeout
	embedWriteTimeout = 400 * time.Millisecond // bbolt gives up 50ms early: commit fails near 350ms, release near 700ms
	t.Cleanup(func() { embedWriteTimeout = prev })
	var calls int
	freed := make(chan struct{})
	c := leaseClient(&calls, func() {
		db, err := bbolt.Open(EmbedDBPath(), 0o600, &bbolt.Options{Timeout: time.Second})
		if err != nil {
			t.Error(err)
			close(freed)
			return
		}
		go func() { // hold the write lock past the commit's timeout, free it before the release gives up
			time.Sleep(500 * time.Millisecond)
			_ = db.Close()
			close(freed)
		}()
		_ = db.Update(func(*bbolt.Tx) error { return nil })
	})
	if _, _, err := EmbedEntries(context.Background(), root, c); err == nil {
		t.Fatal("want the commit error")
	}
	<-freed
	if _, _, ok := readClaim(t); ok {
		t.Fatal("a failed commit left our claim behind")
	}
}

// A MintedAt in the future (clock skew) is not valid: it mints again and the override is open.
func TestEmbedFutureMintedAtIsNotValid(t *testing.T) {
	root := leaseFixture(t)
	var calls int
	c := leaseClient(&calls, nil)
	if _, _, err := EmbedEntries(context.Background(), root, c); err != nil {
		t.Fatal(err)
	}
	backdate(t, -time.Hour)
	if _, open, err := ReembedWindow("fake", time.Now()); err != nil || !open {
		t.Fatalf("future record must open the override: open=%v err=%v", open, err)
	}
	if _, _, err := EmbedEntries(context.Background(), root, c); errors.Is(err, ErrEmbedValid) || err != nil {
		t.Fatalf("future record must mint, got %v", err)
	}
	if got := loadEmbedCache("fake"); got.MintedAt > time.Now().UnixNano() {
		t.Fatal("MintedAt still in the future after the re-mint")
	}
}

func TestReembedWindowRejectsUnsafeProfile(t *testing.T) {
	isolatedConfig1(t)
	if _, open, err := ReembedWindow("../escape", time.Now()); err == nil || open || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe profile: open=%v err=%v", open, err)
	}
}

// A read error on the session profile is a miss: the pinned profile still serves; all-unreadable reports it.
func TestNewSemanticReadErrorFallsBackToPin(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	t.Setenv("CLAUDE_PROFILE", "qa")
	vaulttest.SetRecord(t, "claude", "profile", "dev")
	f := embedCacheFile{Namespace: "dev|h|gte", Model: "gte", Rows: map[string]embedRow{"aaaa0001": {Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}}}
	if err := saveEmbedCache("dev", f); err != nil {
		t.Fatal(err)
	}
	prevRead, prevOpen := readProfileCache, OpenEmbedClient
	t.Cleanup(func() { readProfileCache, OpenEmbedClient = prevRead, prevOpen })
	readProfileCache = func(profile string) (embedCacheFile, error) {
		if profile == "qa" {
			return embedCacheFile{}, errors.New("timeout")
		}
		return prevRead(profile)
	}
	var opened string
	OpenEmbedClient = func(_ context.Context, profile, model string) (*EmbedClient, error) {
		opened = profile
		return &EmbedClient{Profile: profile, Model: model, Namespace: "dev|h|gte", Embed: func(context.Context, []string) ([][]float32, error) {
			return [][]float32{{1, 0, 0}}, nil
		}}, nil
	}
	if _, err := NewSemantic("q"); err != nil || opened != "dev" {
		t.Fatalf("qa unreadable: opened %q err %v, want the pinned dev profile", opened, err)
	}
	readProfileCache = func(string) (embedCacheFile, error) { return embedCacheFile{}, errors.New("timeout") }
	if _, err := NewSemantic("q"); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("every profile unreadable: err %v, want 'unreadable'", err)
	}
}

// A success removes only a backoff marker older than its own attempt; one written mid-query by a timeout stays.
func TestNewSemanticSuccessKeepsFreshBackoffMarker(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	t.Setenv("CLAUDE_PROFILE", "fake")
	f := embedCacheFile{Namespace: "fake|h|gte", Model: "gte", Rows: map[string]embedRow{"aaaa0001": {Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}}}
	if err := saveEmbedCache("fake", f); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(StateDir(), embedCachePre+"fake.backoff")
	write(t, marker, "")
	stale := time.Now().Add(-2 * embedBackoff)
	if err := os.Chtimes(marker, stale, stale); err != nil {
		t.Fatal(err)
	}
	var midQuery func()
	prev := OpenEmbedClient
	t.Cleanup(func() { OpenEmbedClient = prev })
	OpenEmbedClient = func(_ context.Context, profile, model string) (*EmbedClient, error) {
		return &EmbedClient{Profile: profile, Model: model, Namespace: "fake|h|gte", Embed: func(context.Context, []string) ([][]float32, error) {
			if midQuery != nil {
				midQuery()
			}
			return [][]float32{{1, 0, 0}}, nil
		}}, nil
	}
	if _, err := NewSemantic("q"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a stale marker must be removed on success: %v", err)
	}
	midQuery = func() { time.Sleep(30 * time.Millisecond); write(t, marker, "") } // file mtimes tick coarsely
	if _, err := NewSemantic("q"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a marker written during the query was deleted: %v", err)
	}
}
