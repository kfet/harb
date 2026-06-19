package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIndexBuildAndLookup covers buildIndex with the variety of
// filesystem shapes it must skip past — a stray non-dir entry under
// entries/, a non-ndjson file inside a feed dir, and an ndjson record
// whose feedHash field is empty (legacy data; should be filled in from
// the parent dir name). Also exercises IndexedEntries copy semantics
// and the EntryByHash hit / miss branches.
func TestIndexBuildAndLookup(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "entries")
	if err := os.MkdirAll(filepath.Join(root, "feedA"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stray file at entries/ root → buildIndex should !IsDir-skip it.
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("ignore"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-ndjson file inside a feed dir → skipped.
	if err := os.WriteFile(filepath.Join(root, "feedA", "README"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Legacy-shaped record with empty feedHash field — buildIndex should
	// backfill from the parent dir name. We seed two records so the
	// sort.Slice inside buildIndex actually invokes its comparator.
	rec := `{"hash":"deadbeef00000000","guid":"g","published":"2025-01-01T00:00:00Z","fetched_at":"2025-01-01T00:00:00Z"}` + "\n" +
		`{"hash":"cafebabe00000000","guid":"g2","published":"2025-02-01T00:00:00Z","fetched_at":"2025-02-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "feedA", "current.ndjson"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	got := s.IndexedEntries("feedA")
	if len(got) != 2 || got[0].FeedHash != "feedA" {
		t.Fatalf("indexed=%+v", got)
	}
	// Verify desc-by-published order from buildIndex's own sort.
	if !got[0].Published.After(got[1].Published) {
		t.Fatalf("buildIndex did not sort desc: %+v", got)
	}
	// Mutating the returned slice must not corrupt the cache.
	got[0].Title = "MUT"
	again := s.IndexedEntries("feedA")
	if again[0].Title == "MUT" {
		t.Fatal("IndexedEntries did not return a defensive copy")
	}
	// Empty/missing feed → empty slice.
	if len(s.IndexedEntries("nope")) != 0 {
		t.Fatal("expected empty slice for unknown feed")
	}
	// EntryByHash hit + miss. The seeded hashes are recomputed to the
	// guid-only identity on migration, so look up by that id.
	idG := EntryHash("g", "", "", time.Time{})
	if e, ok := s.EntryByHash(idG); !ok || e.GUID != "g" {
		t.Fatalf("byHash hit=%v entry=%+v", ok, e)
	}
	if _, ok := s.EntryByHash("nonexistenthash00"); ok {
		t.Fatal("byHash should miss")
	}

	// AppendEntries must keep idx + byHash in sync and re-sort by
	// Published desc.
	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.AppendEntries("feedA", []Entry{
		{GUID: "older", Published: older, FetchedAt: older},
		{GUID: "newer", Published: newer, FetchedAt: newer},
	}); err != nil {
		t.Fatal(err)
	}
	got = s.IndexedEntries("feedA")
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}
	if !got[0].Published.After(got[1].Published) || !got[1].Published.After(got[2].Published) {
		t.Fatalf("not sorted desc: %+v", got)
	}
	if _, ok := s.EntryByHash(got[0].Hash); !ok {
		t.Fatal("freshly appended entry missing from byHash")
	}
}

// TestBuildIndexFeedReadDirError exercises the inner os.ReadDir error
// branch: a feed directory we own at the top level but cannot read.
//
// We invoke buildIndex directly so the entry-hash migration step in
// Open (which uses filepath.WalkDir and would also fail on the
// chmod-blocked dir) doesn't preempt the path we want to cover.
func TestBuildIndexFeedReadDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "entries", "badfeed")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o755) })
	s := &Store{Dir: dir, state: map[string]EntryState{}, idx: map[string][]Entry{}, byHash: map[string]Entry{}}
	if err := s.buildIndex(); err == nil {
		t.Fatal("expected ReadDir error from buildIndex")
	}
}

// TestBuildIndexScanError exercises the scanEntries error path from
// inside buildIndex by planting a malformed ndjson record. Invoked
// directly because Open's migration step would otherwise fail on the
// same file first.
func TestBuildIndexScanError(t *testing.T) {
	dir := t.TempDir()
	fd := filepath.Join(dir, "entries", "feedX")
	if err := os.MkdirAll(fd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fd, "current.ndjson"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Store{Dir: dir, state: map[string]EntryState{}, idx: map[string][]Entry{}, byHash: map[string]Entry{}}
	if err := s.buildIndex(); err == nil {
		t.Fatal("expected scanEntries error from buildIndex")
	}
}
