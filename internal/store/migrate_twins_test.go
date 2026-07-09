package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMigrateCollapsesSlugEditedTwins reproduces the svobodnatochka.bg bug:
// the same article (identical guid) stored twice because its <link> moved
// from /istorii/ to /novini/ after publish. Migration must collapse the two
// on-disk lines to one (the guid-only identity) and carry read/starred state
// from BOTH stored hashes to the survivor.
func TestMigrateCollapsesSlugEditedTwins(t *testing.T) {
	dir := t.TempDir()
	fh := FeedHash("https://svobodnatochka.bg/feed")
	entDir := filepath.Join(dir, "entries", fh)
	if err := os.MkdirAll(entDir, 0o755); err != nil {
		t.Fatal(err)
	}

	guid := "https://svobodnatochka.bg/?p=12345"
	pub := time.Unix(1700000000, 0)
	// Two lines: the pre-0.15 (guid,link) hash for each link variant.
	oldA := EntryHashLegacyForTest(guid, "https://svobodnatochka.bg/istorii/slug/")
	oldB := EntryHashLegacyForTest(guid, "https://svobodnatochka.bg/novini/slug/")
	if oldA == oldB {
		t.Fatal("fixture: legacy hashes must differ (that was the bug)")
	}
	survivor := EntryHash(guid, "https://svobodnatochka.bg/istorii/slug/", "Title", pub)

	lines := []Entry{
		{Hash: oldA, FeedHash: fh, GUID: guid, Link: "https://svobodnatochka.bg/istorii/slug/", Title: "Title", Published: pub, FetchedAt: pub},
		{Hash: oldB, FeedHash: fh, GUID: guid, Link: "https://svobodnatochka.bg/novini/slug/", Title: "Title", Published: pub, FetchedAt: pub.Add(time.Hour)},
	}
	var b strings.Builder
	for _, e := range lines {
		l, _ := jsonMarshal(e)
		b.Write(l)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(entDir, "current.ndjson"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// Read state on the first copy, starred on the second — both must follow.
	if err := os.WriteFile(filepath.Join(dir, "read.log"), []byte("2024-01-01T00:00:00Z r "+oldA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "starred.log"), []byte("2024-01-01T00:00:00Z s "+oldB+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Identity collapse is the explicit, version-gated migration (not Open).
	if _, err := MigrateIdentity(dir, false); err != nil {
		t.Fatal(err)
	}
	// Re-open so the folded state + index reflect the migrated data.
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListEntries(fh)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("twins not collapsed: listed=%d %+v", len(listed), listed)
	}
	if listed[0].Hash != survivor {
		t.Fatalf("survivor hash=%s want %s", listed[0].Hash, survivor)
	}
	st := s.EntryState(survivor)
	if !st.Read || !st.Starred {
		t.Fatalf("read+starred state must merge to survivor: %+v", st)
	}
	// Neither legacy hash may remain on disk.
	for _, p := range []string{
		filepath.Join(entDir, "current.ndjson"),
		filepath.Join(dir, "read.log"),
		filepath.Join(dir, "starred.log"),
	} {
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), oldA) || strings.Contains(string(data), oldB) {
			t.Fatalf("%s still contains a legacy twin hash:\n%s", p, data)
		}
	}
}

// EntryHashLegacyForTest reproduces the pre-0.15 (guid,link) identity for
// building migration fixtures: NormalizeGUID(guid) \0 link, masked.
func EntryHashLegacyForTest(guid, link string) string {
	return entryHashKey(guid, link, "", time.Time{}, true)
}

// TestMigrateScanError covers the first-pass scanEntries error in
// migrateEntryFile: a malformed ndjson record must fail Open at migration.
func TestMigrateScanError(t *testing.T) {
	dir := t.TempDir()
	fd := filepath.Join(dir, "entries", "feedX")
	if err := os.MkdirAll(fd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fd, "current.ndjson"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("expected migration scanEntries error")
	}
}
