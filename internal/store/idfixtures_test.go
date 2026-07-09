package store

import (
	"os"
	"path/filepath"
	"testing"
)

// loadFixtureFeed writes a testdata/idfixtures/<name>.ndjson file into a fresh
// data dir as entries/<feedHash>/current.ndjson and returns the dir + feedHash.
func loadFixtureFeed(t *testing.T, name, feedHash string) string {
	t.Helper()
	dir := t.TempDir()
	entDir := filepath.Join(dir, "entries", feedHash)
	if err := os.MkdirAll(entDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("testdata", "idfixtures", name+".ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entDir, "current.ndjson"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// survivorGUIDLinks returns, per surviving on-disk entry, its (guid, link, hash)
// after a real migration + reopen.
func survivorsAfterMigrate(t *testing.T, dir, feedHash string) []Entry {
	t.Helper()
	if _, err := MigrateIdentity(dir, false); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListEntries(feedHash)
	if err != nil {
		t.Fatal(err)
	}
	return listed
}

func stickySet(t *testing.T, dir, feedHash string) map[string]bool {
	t.Helper()
	set, err := (&Store{Dir: dir}).loadStickyGUIDs(feedHash)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// TestFixtureSvobodaWordPressSlugDriftCollapses is the real-data regression for
// the WordPress slug-drift case: same URI-form guid (?p=N), drifted link. The
// guid is a stable publisher identity, so each same-guid group collapses to one
// survivor at the guid-only D1 identity, and NO guid is marked sticky.
func TestFixtureSvobodaWordPressSlugDriftCollapses(t *testing.T) {
	const fh = "a5a2e1ef25e7a4889e82"
	dir := loadFixtureFeed(t, "svoboda", fh)
	got := survivorsAfterMigrate(t, dir, fh)
	// 3 dup groups (2 members each) + 1 singleton = 7 lines → 4 survivors.
	if len(got) != 4 {
		t.Fatalf("svoboda survivors=%d want 4: %+v", len(got), guidsOf(got))
	}
	if n := len(stickySet(t, dir, fh)); n != 0 {
		t.Fatalf("svoboda must mark NO sticky guids, got %d", n)
	}
	// Each survivor's hash is the guid-only D1 identity.
	for _, e := range got {
		if e.Hash != EntryHash(e.GUID, e.Link, e.Title, e.Published) {
			t.Fatalf("survivor %s not at guid-only D1 identity", e.GUID)
		}
	}
}

// TestFixtureCBCSharedGUIDStaysDistinct is the real-data regression for the CBC
// case: an OPAQUE-token guid (article number) recycled across genuinely-distinct
// articles that appear with distinct links. Each such guid is marked sticky and
// its members stay distinct (D4); nothing collapses.
func TestFixtureCBCSharedGUIDStaysDistinct(t *testing.T) {
	const fh = "410ed8fd6e3b239d70fb"
	dir := loadFixtureFeed(t, "cbc-bc", fh)
	got := survivorsAfterMigrate(t, dir, fh)
	// 2 dup groups (2 distinct-link members each) + 1 singleton = 5 lines →
	// all 5 survive (no collapse).
	if len(got) != 5 {
		t.Fatalf("cbc survivors=%d want 5 (must stay distinct): %+v", len(got), guidsOf(got))
	}
	sticky := stickySet(t, dir, fh)
	for _, g := range []string{"9.7227935", "9.7229367"} {
		if !sticky[g] {
			t.Fatalf("cbc guid %s must be marked sticky", g)
		}
	}
	// The two members of a shared guid must carry DISTINCT hashes.
	byGUID := map[string][]string{}
	for _, e := range got {
		byGUID[e.GUID] = append(byGUID[e.GUID], e.Hash)
	}
	for _, g := range []string{"9.7227935", "9.7229367"} {
		if h := byGUID[g]; len(h) != 2 || h[0] == h[1] {
			t.Fatalf("cbc guid %s members not distinct: %v", g, h)
		}
	}
}

// TestFixtureCBCTopCollapsesOnlyExactDuplicateLink covers a sticky (opaque) guid
// whose members include BOTH genuinely-distinct links AND an exact-duplicate
// link line. Under D4 the distinct links stay separate; only the identical-link
// re-poll collapses.
func TestFixtureCBCTopCollapsesOnlyExactDuplicateLink(t *testing.T) {
	const fh = "7f22d08cb2d090144fc8"
	dir := loadFixtureFeed(t, "cbc-top", fh)
	got := survivorsAfterMigrate(t, dir, fh)
	// 9.7050838: 2 distinct links → 2 survivors.
	// 9.7263731: 2 distinct links but one is duplicated (3 lines) → 2 survivors.
	// 5 lines → 4 survivors.
	if len(got) != 4 {
		t.Fatalf("cbc-top survivors=%d want 4: %+v", len(got), guidsOf(got))
	}
	sticky := stickySet(t, dir, fh)
	if !sticky["9.7050838"] || !sticky["9.7263731"] {
		t.Fatalf("cbc-top guids must be sticky: %v", sticky)
	}
}

// TestFixtureNWRReusedGUIDStable is the NWR "batch-flip" regression: a
// volatile-date guid (NormalizeGUID → stable token) reused across a typo/
// corrected link pair. The opaque-token rule marks it sticky (D4) so members
// stay distinct — the conservative direction (keep the dup, never drop an
// article) — and, crucially, the assignment is stable across re-polls.
func TestFixtureNWRReusedGUIDStable(t *testing.T) {
	const fh = "772c70d879cde0ce47d8"
	dir := loadFixtureFeed(t, "nwr", fh)
	got := survivorsAfterMigrate(t, dir, fh)
	if len(got) != 4 {
		t.Fatalf("nwr survivors=%d want 4: %+v", len(got), guidsOf(got))
	}
	sticky := stickySet(t, dir, fh)
	if !sticky["news/75555"] || !sticky["review/75637"] {
		t.Fatalf("nwr normalised guids must be sticky: %v", sticky)
	}
	// Re-poll the corrected news/75555 item alone: the persisted sticky mark
	// must keep it at its D4 hash (no flip, no re-dup).
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	poll := []Entry{{FeedHash: fh, GUID: "news/75555 Tue, 12 May 2026 09:22:00 EDT",
		Link: "http://www.nintendoworldreport.com/news/75555/switch-2-bundles-to-return-offering-choice-of-three-games"}}
	if err := s.AssignEntryHashesForFeed(fh, poll); err != nil {
		t.Fatal(err)
	}
	added, err := s.AppendEntries(fh, poll)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("nwr corrected re-poll re-stored (%d added) — flip/re-dup", len(added))
	}
}

// TestFixtureSarahEntityDriftStable is the entity-drift regression
// (sarahcandersen): URI-form guids, no on-disk dup guids. Migration is a no-op
// on identity, and a re-poll whose title differs ONLY by HTML-entity decoding
// must NOT change the hash (title never enters identity).
func TestFixtureSarahEntityDriftStable(t *testing.T) {
	const fh = "b970e14dc874e55174bc"
	dir := loadFixtureFeed(t, "sarah", fh)
	got := survivorsAfterMigrate(t, dir, fh)
	if len(got) != 5 {
		t.Fatalf("sarah survivors=%d want 5: %+v", len(got), guidsOf(got))
	}
	if n := len(stickySet(t, dir, fh)); n != 0 {
		t.Fatalf("sarah must mark NO sticky guids, got %d", n)
	}
	// Same guid, title differs only by entity decode → identical hash.
	guid := "https://sarahcandersen.com/post/771722575813492736"
	a := EntryHash(guid, guid, "Books &amp; art", got[0].Published)
	b := EntryHash(guid, guid, "Books & art", got[0].Published)
	if a != b {
		t.Fatalf("entity-drifted title must not change hash: %s vs %s", a, b)
	}
}

func guidsOf(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.GUID
	}
	return out
}
