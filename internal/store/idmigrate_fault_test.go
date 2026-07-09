package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMigrateIdentityBadStickySidecar: a corrupt reused-guids.json makes
// loadStickyGUIDs (and thus MigrateIdentity) fail.
func TestMigrateIdentityBadStickySidecar(t *testing.T) {
	dir := t.TempDir()
	fh := "feedX"
	writeFeedNDJSON(t, dir, fh, []Entry{{Hash: "aaaabbbbccccdddd", GUID: "g", Link: "l"}})
	if err := os.WriteFile(filepath.Join(dir, "entries", fh, "reused-guids.json"),
		[]byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateIdentity(dir, true); err == nil {
		t.Fatal("expected error from corrupt sticky sidecar")
	}
	// Same corruption fails the runtime path.
	s := &Store{Dir: dir}
	if err := s.AssignEntryHashesForFeed(fh, []Entry{{GUID: "g", Link: "l"}}); err == nil {
		t.Fatal("expected AssignEntryHashesForFeed error from corrupt sidecar")
	}
	if _, err := s.loadStickyGUIDs(fh); err == nil {
		t.Fatal("expected loadStickyGUIDs error")
	}
}

// TestMigrateIdentityMalformedNDJSON: an unparseable entry line fails the gather
// pass.
func TestMigrateIdentityMalformedNDJSON(t *testing.T) {
	dir := t.TempDir()
	fh := "feedY"
	entDir := filepath.Join(dir, "entries", fh)
	if err := os.MkdirAll(entDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entDir, "current.ndjson"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateIdentity(dir, true); err == nil {
		t.Fatal("expected error from malformed ndjson")
	}
}

// TestMigrateIdentityUnreadableStateLog: a state log we cannot read fails the
// pre-migration fold.
func TestMigrateIdentityUnreadableStateLog(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "read.log")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if _, err := MigrateIdentity(dir, true); err == nil {
		t.Fatal("expected error from unreadable read.log")
	}
}

// TestMigrateIdentityUnreadableEntriesDir: an unreadable entries/ dir fails
// feedHashDirs.
func TestMigrateIdentityUnreadableEntriesDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	ent := filepath.Join(dir, "entries")
	if err := os.MkdirAll(ent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ent, 0o755) })
	if _, err := MigrateIdentity(dir, true); err == nil {
		t.Fatal("expected error from unreadable entries dir")
	}
}

// TestFoldStateFileUnsetOps covers the 'u' (unread) and 'S' (unstar) branches:
// an item set then cleared must NOT appear live.
func TestFoldStateFileUnsetOps(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "read.log"),
		[]byte("2024-01-01T00:00:00Z r h1\n2024-01-02T00:00:00Z u h1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "starred.log"),
		[]byte("2024-01-01T00:00:00Z s h2\n2024-01-02T00:00:00Z S h2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := foldStateFile(filepath.Join(dir, "read.log"), 'r')
	if err != nil {
		t.Fatal(err)
	}
	star, err := foldStateFile(filepath.Join(dir, "starred.log"), 's')
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 0 || len(star) != 0 {
		t.Fatalf("unset ops must clear live: read=%v star=%v", read, star)
	}
}

// TestMigrateIdentityReadOnlyDirApplyFails: a real run into a read-only data dir
// fails when it tries to write (marker / files).
func TestMigrateIdentityReadOnlyDirApplyFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	fh := "feedZ"
	// One collapsible pair so applyFileRewrites has work to do.
	guid := "https://blog/?p=1"
	writeFeedNDJSON(t, dir, fh, []Entry{
		{Hash: EntryHashLegacyForTest(guid, "https://blog/a"), GUID: guid, Link: "https://blog/a", FetchedAt: time.Unix(1, 0)},
		{Hash: EntryHashLegacyForTest(guid, "https://blog/b"), GUID: guid, Link: "https://blog/b", FetchedAt: time.Unix(2, 0)},
	})
	// Make the feed dir read-only so atomicWriteFile of current.ndjson fails.
	feedDir := filepath.Join(dir, "entries", fh)
	if err := os.Chmod(feedDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(feedDir, 0o755) })
	if _, err := MigrateIdentity(dir, false); err == nil {
		t.Fatal("expected apply error writing into read-only feed dir")
	}
}

// TestMigrateIdentityWriteMarkerFails: a real run on a dir that has no
// collapsible entries but is read-only at the top level fails writing the
// version marker.
func TestMigrateIdentityWriteMarkerFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := MigrateIdentity(dir, false); err == nil {
		t.Fatal("expected marker-write error on read-only dir")
	}
}

// TestMigrateIdentityUnreadableStarredLog covers the starred.log fold error arm.
func TestMigrateIdentityUnreadableStarredLog(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "starred.log")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if _, err := MigrateIdentity(dir, true); err == nil {
		t.Fatal("expected error from unreadable starred.log")
	}
}

// TestMigrateIdentityEmptyGUIDSkipped covers the empty-guid seeding skip and
// ensures an empty-guid item recomputes to its link identity.
func TestMigrateIdentityEmptyGUIDSkipped(t *testing.T) {
	dir := t.TempDir()
	fh := "feedEmptyGUID"
	writeFeedNDJSON(t, dir, fh, []Entry{
		{Hash: "1111222233334444", GUID: "", Link: "https://x/only", Title: "L"},
	})
	rep, err := MigrateIdentity(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.StickyMarkings != 0 || rep.Totals.IntendedCollapses != 0 {
		t.Fatalf("empty-guid item must not mark/collapse: %+v", rep.Totals)
	}
}

// TestFoldStateFileGarbageLine covers the malformed (len!=3) line skip.
func TestFoldStateFileGarbageLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "read.log"),
		[]byte("garbage-no-fields\n2024-01-01T00:00:00Z r h1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	live, err := foldStateFile(filepath.Join(dir, "read.log"), 'r')
	if err != nil {
		t.Fatal(err)
	}
	if !live[StoreEntryHash("h1")] {
		t.Fatal("valid line after garbage must still fold")
	}
}

// TestMigrateIdentityOrphanStateHash covers the remapState pass-through and the
// assertStatePreserved skip for a state-log hash with no on-disk entry.
func TestMigrateIdentityOrphanStateHash(t *testing.T) {
	dir := t.TempDir()
	fh := "feedOrphan"
	guid := "https://blog/?p=7"
	writeFeedNDJSON(t, dir, fh, []Entry{
		{Hash: EntryHashLegacyForTest(guid, "https://blog/a"), GUID: guid, Link: "https://blog/a", FetchedAt: time.Unix(1, 0)},
		{Hash: EntryHashLegacyForTest(guid, "https://blog/b"), GUID: guid, Link: "https://blog/b", FetchedAt: time.Unix(2, 0)},
	})
	// A starred hash for an item that is NOT on disk (archived out).
	if err := os.WriteFile(filepath.Join(dir, "starred.log"),
		[]byte("2024-01-01T00:00:00Z s deadbeefdeadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := MigrateIdentity(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	// Orphan star passes through untouched → StarAfter still 1.
	if rep.Totals.StarBefore != 1 || rep.Totals.StarAfter != 1 {
		t.Fatalf("orphan star must pass through: %+v", rep.Totals)
	}
}

// TestGatherFeedEntriesUnreadableFeedDir covers the ReadDir error arm of
// gatherFeedEntries.
func TestGatherFeedEntriesUnreadableFeedDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	fh := "feedNoRead"
	writeFeedNDJSON(t, dir, fh, []Entry{{Hash: "1111222233334444", GUID: "g", Link: "l"}})
	feedDir := filepath.Join(dir, "entries", fh)
	if err := os.Chmod(feedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(feedDir, 0o755) })
	if _, err := MigrateIdentity(dir, true); err == nil {
		t.Fatal("expected gatherFeedEntries ReadDir error")
	}
}

// TestAssignEntryHashesForFeedSaveError covers the sticky-set save failure arm
// (and saveStickyGUIDs' MkdirAll failure) when the entries dir is read-only.
func TestAssignEntryHashesForFeedSaveError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	ent := filepath.Join(dir, "entries")
	if err := os.MkdirAll(ent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ent, 0o755) })
	s := &Store{Dir: dir}
	// A co-occurring guid makes the sticky set grow → triggers save.
	batch := []Entry{
		{GUID: "g", Link: "https://x/a"},
		{GUID: "g", Link: "https://x/b"},
	}
	if err := s.AssignEntryHashesForFeed("feedRO", batch); err == nil {
		t.Fatal("expected save error into read-only entries dir")
	}
}

// TestLoadStickyGUIDsUnreadable covers loadStickyGUIDs' ReadFile error arm
// (distinct from the JSON-parse arm).
func TestLoadStickyGUIDsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	fh := "feedSticky"
	p := filepath.Join(dir, "entries", fh)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	side := filepath.Join(p, "reused-guids.json")
	if err := os.WriteFile(side, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(side, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(side, 0o644) })
	if _, err := (&Store{Dir: dir}).loadStickyGUIDs(fh); err == nil {
		t.Fatal("expected loadStickyGUIDs ReadFile error")
	}
}
