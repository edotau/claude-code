package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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
