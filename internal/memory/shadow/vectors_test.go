package shadow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"go.etcd.io/bbolt"
)

func vectorsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HARNESS_KEYRING", "off")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func fixtureRecord() memory.VectorRecord {
	return memory.VectorRecord{Schema: memory.EmbedCacheSchema, Model: "gte", Dim: 2, DimAt: 5, MintedAt: time.Now().Add(-time.Minute).UnixNano(),
		Rows: map[string]memory.VectorRow{
			"aaaa0001": {Root: "/r", Hash: "h1", BlockHash: "b1", Vec: "\x00\x00\x80\x3f\x00\x00\x00\x00"},
			"aaaa0002": {Hash: "h2", Vec: "\x00\x00\x00\x00\x00\x00\x80\x3f"},
		}}
}

// writeLegacy writes rec in the bbolt layout the previous store used.
func writeLegacy(t *testing.T, rec memory.VectorRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(memory.EmbedDBPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(memory.EmbedDBPath(), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucket(legacyVectorBucket)
		if err != nil {
			return err
		}
		if err := b.Put(legacyMetaKey, raw); err != nil {
			return err
		}
		rows, err := b.CreateBucket(legacyRowsBucket)
		if err != nil {
			return err
		}
		for id, r := range rec.Rows {
			if err := rows.Put([]byte(id), []byte(r.Vec)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Given a bbolt fixture, When the store opens, Then every record is imported identically and the file is kept.
func TestVectorsImportLegacyKeepsFile(t *testing.T) {
	vectorsEnv(t)
	want := fixtureRecord()
	writeLegacy(t, want)
	s := sqlVectors{}
	got, err := s.Vectors()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("imported record differs (the recall input must be byte-identical): err=%v\n got %+v\nwant %+v", err, got, want)
	}
	if _, err := os.Stat(memory.EmbedDBPath()); err != nil {
		t.Fatalf("the legacy file must be kept: %v", err)
	}
	if h, err := s.Head(); err != nil || h.Model != "gte" || h.MintedAt != want.MintedAt || !h.Rows {
		t.Fatalf("Head = %+v, %v", h, err)
	}
	putRecord(t, s, 7, memory.VectorRecord{Model: "other", MintedAt: time.Now().UnixNano(), Rows: map[string]memory.VectorRow{"x": {Hash: "x"}}})
	if got, _ := s.Vectors(); got.Model != "other" || len(got.Rows) != 1 {
		t.Fatalf("a reopen re-imported the legacy file: %+v", got)
	}
}

// putRecord claims as owner and stores rec through Put.
func putRecord(t *testing.T, s sqlVectors, owner int64, rec memory.VectorRecord) {
	t.Helper()
	now := time.Now()
	if _, ok, err := s.Claim(owner, now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	stored, err := s.Put(memory.VectorWrite{Owner: owner, Started: now.UnixNano(), Merge: func(memory.VectorRecord) memory.VectorRecord { return rec }})
	if err != nil || !stored {
		t.Fatalf("put: %v %v", stored, err)
	}
}

// Given a damaged legacy file, When the store opens, Then the record is empty and a mint can proceed.
func TestVectorsDamagedLegacyImportsEmpty(t *testing.T) {
	vectorsEnv(t)
	if err := os.MkdirAll(filepath.Dir(memory.EmbedDBPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory.EmbedDBPath(), append([]byte("not a bbolt file"), make([]byte, 16384)...), 0o600); err != nil {
		t.Fatal(err)
	}
	s := sqlVectors{}
	if rec, err := s.Vectors(); err != nil || rec.Model != "" {
		t.Fatalf("damaged legacy: %+v %v", rec, err)
	}
	if !s.RefreshDue(time.Now()) {
		t.Fatal("an empty store must be due")
	}
}

// Given no store at all, When read, Then nothing is created and the store is due.
func TestVectorsAbsentCreatesNothing(t *testing.T) {
	vectorsEnv(t)
	s := sqlVectors{}
	if rec, err := s.Vectors(); err != nil || rec.Model != "" || s.Present() || !s.RefreshDue(time.Now()) {
		t.Fatalf("absent store: %+v %v", rec, err)
	}
	if _, err := os.Stat(ShadowDBPath()); err == nil {
		t.Fatal("a read created the shadow db")
	}
}

// A live claim blocks other owners and the refresh; a non-owner's Put stores nothing.
func TestVectorsClaimIsExclusive(t *testing.T) {
	vectorsEnv(t)
	s := sqlVectors{}
	now := time.Now()
	if _, ok, err := s.Claim(1, now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if held, ok, err := s.Claim(2, now, now.Add(time.Minute)); err != nil || ok || held.IsZero() || s.RefreshDue(now) {
		t.Fatalf("a live claim must refuse others and block the refresh: %v %v %v", held, ok, err)
	}
	merge := func(memory.VectorRecord) memory.VectorRecord { return fixtureRecord() }
	if stored, err := s.Put(memory.VectorWrite{Owner: 2, Started: now.UnixNano(), Merge: merge}); err != nil || stored {
		t.Fatalf("a non-owner must not store: %v %v", stored, err)
	}
}

// Put stores the record unchanged and releases the claim.
func TestVectorsRoundTrip(t *testing.T) {
	vectorsEnv(t)
	s := sqlVectors{}
	want := fixtureRecord()
	putRecord(t, s, 1, want)
	if got, err := s.Vectors(); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	now := time.Now()
	if _, ok, _ := s.Claim(3, now, now.Add(time.Minute)); !ok {
		t.Fatal("Put must release the claim")
	}
}
