package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFeedNDJSON writes entries to entries/<fh>/current.ndjson in a data dir.
func writeFeedNDJSON(t *testing.T, dir, fh string, entries []Entry) {
	t.Helper()
	entDir := filepath.Join(dir, "entries", fh)
	if err := os.MkdirAll(entDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		line, err := jsonMarshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(entDir, "current.ndjson"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateIdentityStateRemapAndAccounting exercises the full bar: collapse a
// URI-guid slug-drift twin, carry read+starred state to the survivor, keep the
// survivor's fetched_at at the EARLIEST copy's, and preserve exact star + read
// counts. Then assert idempotence (2nd run no-op).
func TestMigrateIdentityStateRemapAndAccounting(t *testing.T) {
	dir := t.TempDir()
	fh := FeedHash("https://blog.example/feed")
	guid := "https://blog.example/?p=42"
	pub := time.Unix(1700000000, 0).UTC()
	early := time.Unix(1700000000, 0).UTC()
	late := time.Unix(1700100000, 0).UTC()

	hA := EntryHashLegacyForTest(guid, "https://blog.example/istorii/x")
	hB := EntryHashLegacyForTest(guid, "https://blog.example/novini/x")
	// A distinct, never-duplicated starred item to prove star count is exact.
	hSolo := EntryHash("https://blog.example/?p=99", "https://blog.example/solo", "Solo", pub)

	writeFeedNDJSON(t, dir, fh, []Entry{
		{Hash: hA, FeedHash: fh, GUID: guid, Link: "https://blog.example/istorii/x", Title: "T", Published: pub, FetchedAt: late},
		{Hash: hB, FeedHash: fh, GUID: guid, Link: "https://blog.example/novini/x", Title: "T", Published: pub, FetchedAt: early},
		{Hash: hSolo, FeedHash: fh, GUID: "https://blog.example/?p=99", Link: "https://blog.example/solo", Title: "Solo", Published: pub, FetchedAt: early},
	})
	// State: hA read, hB starred, hSolo starred.
	if err := os.WriteFile(filepath.Join(dir, "read.log"),
		[]byte("2024-01-01T00:00:00Z r "+hA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "starred.log"),
		[]byte("2024-01-01T00:00:00Z s "+hB+"\n2024-01-01T00:00:00Z s "+hSolo+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Dry-run first: must NOT modify anything.
	before, _ := os.ReadFile(filepath.Join(dir, "entries", fh, "current.ndjson"))
	rep, err := MigrateIdentity(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "entries", fh, "current.ndjson"))
	if string(before) != string(after) {
		t.Fatal("dry-run modified entry file")
	}
	if rep.Totals.OldHashCount != 3 || rep.Totals.SurvivorCount != 2 || rep.Totals.IntendedCollapses != 1 {
		t.Fatalf("dry-run totals wrong: %+v", rep.Totals)
	}
	if rep.Totals.StarBefore != 2 || rep.Totals.StarAfter != 2 {
		t.Fatalf("star count must be exact (2→2): %+v", rep.Totals)
	}

	// Real run.
	rep, err = MigrateIdentity(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped {
		t.Fatal("real run unexpectedly skipped")
	}
	survivor := EntryHash(guid, "https://blog.example/istorii/x", "T", pub)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListEntries(fh)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("survivors=%d want 2", len(listed))
	}
	// Survivor state: read (from hA) AND starred (from hB).
	st := s.EntryState(survivor)
	if !st.Read || !st.Starred {
		t.Fatalf("survivor must merge read+starred: %+v", st)
	}
	// Survivor fetched_at = earliest.
	for _, e := range listed {
		if e.Hash == survivor && !e.FetchedAt.Equal(early) {
			t.Fatalf("survivor fetched_at=%v want earliest %v", e.FetchedAt, early)
		}
	}
	// Exact star count preserved (survivor + solo = 2).
	if s.CountStarred() != 2 {
		t.Fatalf("starred count=%d want 2", s.CountStarred())
	}
	if s.CountRead() != 1 {
		t.Fatalf("read count=%d want 1", s.CountRead())
	}
	// No legacy hash left anywhere.
	for _, p := range []string{
		filepath.Join(dir, "entries", fh, "current.ndjson"),
		filepath.Join(dir, "read.log"), filepath.Join(dir, "starred.log"),
	} {
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), hA) || strings.Contains(string(data), hB) {
			t.Fatalf("%s still contains a legacy twin hash", p)
		}
	}

	// Idempotence: a second real run is a no-op (version-gated).
	rep2, err := MigrateIdentity(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.Skipped {
		t.Fatal("second real run must skip (version marker)")
	}
	// And structurally idempotent: removing the marker and dry-running again
	// finds zero collapses / zero new sticky markings.
	if err := os.Remove(filepath.Join(dir, identityMarkerName)); err != nil {
		t.Fatal(err)
	}
	rep3, err := MigrateIdentity(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep3.Totals.IntendedCollapses != 0 || rep3.Totals.StickyMarkings != 0 {
		t.Fatalf("migration not a fixed point: %+v", rep3.Totals)
	}
}

// TestMigrateIdentityMarkerAndEmptyDir covers the empty-data-dir path and the
// marker no-op branch on a fresh dir.
func TestMigrateIdentityEmptyDir(t *testing.T) {
	dir := t.TempDir()
	rep, err := MigrateIdentity(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.OldHashCount != 0 || len(rep.Feeds) != 0 {
		t.Fatalf("empty dir must yield empty report: %+v", rep.Totals)
	}
	// Marker now present → second run skips.
	rep2, err := MigrateIdentity(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.Skipped {
		t.Fatal("second run on empty dir must skip")
	}
}

// TestMigrateIdentityORStateSemantics proves OR-semantics on collapse: a
// survivor must be read/starred if ANY collapsed member was, even when a losing
// member's FINAL op is a later unread/unstar (which last-op-wins folding would
// otherwise honour).
func TestMigrateIdentityORStateSemantics(t *testing.T) {
	dir := t.TempDir()
	fh := FeedHash("https://blog.example/feed")
	guid := "https://blog.example/?p=1" // URI guid → collapses
	pub := time.Unix(1700000000, 0).UTC()
	hA := EntryHashLegacyForTest(guid, "https://blog.example/a")
	hB := EntryHashLegacyForTest(guid, "https://blog.example/b")

	writeFeedNDJSON(t, dir, fh, []Entry{
		{Hash: hA, FeedHash: fh, GUID: guid, Link: "https://blog.example/a", Title: "T", Published: pub, FetchedAt: time.Unix(1, 0)},
		{Hash: hB, FeedHash: fh, GUID: guid, Link: "https://blog.example/b", Title: "T", Published: pub, FetchedAt: time.Unix(2, 0)},
	})
	// read.log: A read; B read then LATER unread (B's final = unread).
	if err := os.WriteFile(filepath.Join(dir, "read.log"), []byte(
		"2024-01-01T00:00:00Z r "+hA+"\n"+
			"2024-01-02T00:00:00Z r "+hB+"\n"+
			"2024-01-03T00:00:00Z u "+hB+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// starred.log: A starred; B starred then LATER unstarred.
	if err := os.WriteFile(filepath.Join(dir, "starred.log"), []byte(
		"2024-01-01T00:00:00Z s "+hA+"\n"+
			"2024-01-02T00:00:00Z s "+hB+"\n"+
			"2024-01-03T00:00:00Z S "+hB+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := MigrateIdentity(dir, false); err != nil {
		t.Fatal(err)
	}
	survivor := EntryHash(guid, "https://blog.example/a", "T", pub)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := s.EntryState(survivor)
	if !st.Read {
		t.Fatal("OR-read: survivor must be read (member A was), despite B's later unread")
	}
	if !st.Starred {
		t.Fatal("OR-starred: survivor must be starred (member A was), despite B's later unstar")
	}
	if s.CountRead() != 1 || s.CountStarred() != 1 {
		t.Fatalf("counts read=%d starred=%d want 1/1", s.CountRead(), s.CountStarred())
	}
}
