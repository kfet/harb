package store

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The corrected NWR "Star Fox Gets Free Demo" item, exactly as it appears in
// the live production feed (testdata/nwr-current.ndjson). Its volatile guid
// normalises to "news/75967".
const (
	nwrGUID  = "news/75967 Tue, 09 Jun 2026 11:35:00 EDT"
	nwrLink  = "http://www.nintendoworldreport.com/news/75967/star-fox-gets-free-demo"
	nwrTitle = "Star Fox Gets Free Demo"
	// The publisher's earlier typo variant that shared the same guid but a
	// distinct title/slug — the sibling whose presence makes news/75967
	// "reused" (D4). It scrolls out of the feed window over time.
	nwrTypoLink  = "http://www.nintendoworldreport.com/news/75967/star-fox-to-gets-free-demo"
	nwrTypoTitle = "Star Fox TO Gets Free Demo"
)

// loadLiveCorrected reads the real corrected news/75967 entry from the staged
// production current.ndjson so the regression uses actual on-disk bytes.
func loadLiveCorrected(t *testing.T) Entry {
	t.Helper()
	f, err := os.Open("testdata/nwr-current.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.GUID == nwrGUID {
			return e
		}
	}
	t.Fatalf("corrected news/75967 entry not found in live testdata")
	return Entry{}
}

// TestNWRStarFoxNoReDup is the real-data regression for the guid-reuse re-dup
// (case 1). Preconditions from disk: news/75967 carries two distinct-title
// entries (corrected + typo), so its identity basis is D4. A later poll that
// contains ONLY the corrected item must resolve to the same on-disk D4 hash and
// add nothing — the batch-only verdict would flip it to D1 and re-store it.
func TestNWRStarFoxNoReDup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fh = "nwrfeed"

	corrected := loadLiveCorrected(t)
	corrected.FeedHash = fh
	typo := Entry{FeedHash: fh, GUID: nwrGUID, Link: nwrTypoLink, Title: nwrTypoTitle}

	// Seed disk: both title-siblings present → D4 verdict.
	seed := []Entry{corrected, typo}
	s.AssignEntryHashesForFeed(fh, seed)
	added, err := s.AppendEntries(fh, seed)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("seed: want 2 stored, got %d", len(added))
	}
	d4 := seed[0].Hash // corrected under D4

	// The batch-only (pre-fix) verdict for a lone corrected item is D1 — a
	// DIFFERENT hash. That divergence is exactly the re-dup.
	lone := []Entry{{FeedHash: fh, GUID: nwrGUID, Link: nwrLink, Title: nwrTitle}}
	AssignEntryHashes(lone)
	if lone[0].Hash == d4 {
		t.Fatal("expected batch-only verdict to differ (D1 vs D4) — test premise broken")
	}

	// Later poll: ONLY the corrected item. Feed-aware assignment must pin D4
	// from the on-disk siblings, so nothing new is added.
	poll := []Entry{{FeedHash: fh, GUID: nwrGUID, Link: nwrLink, Title: nwrTitle}}
	s.AssignEntryHashesForFeed(fh, poll)
	if poll[0].Hash != d4 {
		t.Fatalf("feed-aware hash flipped: got %s want on-disk D4 %s", poll[0].Hash, d4)
	}
	added, err = s.AppendEntries(fh, poll)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("re-dup: corrected item stored again (%d added)", len(added))
	}
}

// TestAssignEntryHashesForFeed_WordPressSlugDrift is case 2: a stable-guid,
// same-title article whose link drifts /istorii<->/novini must still collapse
// (guid-only D1) even with the union verdict — link drift is NOT reuse.
func TestAssignEntryHashesForFeed_WordPressSlugDrift(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fh = "wp"
	first := []Entry{{FeedHash: fh, GUID: "https://blog/?p=1", Link: "https://blog/istorii/x", Title: "Same"}}
	s.AssignEntryHashesForFeed(fh, first)
	if _, err := s.AppendEntries(fh, first); err != nil {
		t.Fatal(err)
	}
	// Same article, link drifted to /novini.
	drift := []Entry{{FeedHash: fh, GUID: "https://blog/?p=1", Link: "https://blog/novini/x", Title: "Same"}}
	s.AssignEntryHashesForFeed(fh, drift)
	if drift[0].Hash != first[0].Hash {
		t.Fatalf("slug-drift twin must collapse: %s vs %s", drift[0].Hash, first[0].Hash)
	}
	added, err := s.AppendEntries(fh, drift)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("slug-drift must not re-store (%d added)", len(added))
	}
}

// TestAssignEntryHashesForFeed_SharedGUIDDistinct is case 3: genuinely-distinct
// articles that share one guid (differing titles, CBC-style) must stay distinct
// under the union verdict (D4). Here the sibling is only on disk, not in the
// batch — the whole point of folding in existing entries.
func TestAssignEntryHashesForFeed_SharedGUIDDistinct(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fh = "cbc"
	seed := []Entry{{FeedHash: fh, GUID: "shared", Link: "https://cbc/a", Title: "Article A"}}
	s.AssignEntryHashesForFeed(fh, seed)
	if _, err := s.AppendEntries(fh, seed); err != nil {
		t.Fatal(err)
	}
	// A DISTINCT article reusing the same guid arrives; with only itself in
	// the batch the verdict would be D1 and could collide, but the on-disk
	// sibling (distinct title) makes "shared" reused → D4 → distinct hash.
	next := []Entry{{FeedHash: fh, GUID: "shared", Link: "https://cbc/b", Title: "Article B"}}
	s.AssignEntryHashesForFeed(fh, next)
	if next[0].Hash == seed[0].Hash {
		t.Fatal("distinct shared-guid articles must not collapse")
	}
	added, err := s.AppendEntries(fh, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Fatalf("distinct article must be stored (%d added)", len(added))
	}
}

// TestAssignEntryHashesForFeed_NoExistingHashChange is case 4: the union
// verdict must not alter the hash a guid-only (never-reused) item already has
// on disk. Prove the feed-aware path yields the same hash as the plain
// batch-only path when there is no reuse anywhere.
func TestAssignEntryHashesForFeed_NoExistingHashChange(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fh = "plain"
	e := []Entry{{FeedHash: fh, GUID: "g-unique", Link: "https://x/1", Title: "Solo"}}
	want := EntryHash("g-unique", "https://x/1", "Solo", time.Time{})
	s.AssignEntryHashesForFeed(fh, e)
	if e[0].Hash != want {
		t.Fatalf("no-reuse feed-aware hash must equal guid-only EntryHash: %s vs %s", e[0].Hash, want)
	}
}
