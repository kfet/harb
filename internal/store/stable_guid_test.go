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

	// Seed disk: both siblings share news/75967 in ONE batch → within-batch
	// co-occurrence marks the guid sticky (reused) → D4, and it stays sticky.
	seed := []Entry{corrected, typo}
	if err := s.AssignEntryHashesForFeed(fh, seed); err != nil {
		t.Fatal(err)
	}
	added, err := s.AppendEntries(fh, seed)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("seed: want 2 stored, got %d", len(added))
	}
	d4 := seed[0].Hash // corrected under D4

	// The batch-only (no-sticky) verdict for a lone corrected item is D1 — a
	// DIFFERENT hash. That divergence is exactly the re-dup.
	lone := []Entry{{FeedHash: fh, GUID: nwrGUID, Link: nwrLink, Title: nwrTitle}}
	AssignEntryHashes(lone)
	if lone[0].Hash == d4 {
		t.Fatal("expected batch-only verdict to differ (D1 vs D4) — test premise broken")
	}

	// Later poll: ONLY the corrected item. The PERSISTENT sticky set pins D4,
	// so nothing new is added.
	poll := []Entry{{FeedHash: fh, GUID: nwrGUID, Link: nwrLink, Title: nwrTitle}}
	if err := s.AssignEntryHashesForFeed(fh, poll); err != nil {
		t.Fatal(err)
	}
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

// TestAssignEntryHashesForFeed_WordPressSlugDrift is case 2: a stable-guid
// article whose link drifts /istorii<->/novini across SEPARATE polls must
// collapse (guid-only D1). Each poll lists the guid exactly once, so it never
// co-occurs and never becomes sticky — link never enters its identity.
func TestAssignEntryHashesForFeed_WordPressSlugDrift(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fh = "wp"
	first := []Entry{{FeedHash: fh, GUID: "https://blog/?p=1", Link: "https://blog/istorii/x", Title: "Same"}}
	if err := s.AssignEntryHashesForFeed(fh, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEntries(fh, first); err != nil {
		t.Fatal(err)
	}
	// Same article, link drifted to /novini — a SEPARATE poll batch.
	drift := []Entry{{FeedHash: fh, GUID: "https://blog/?p=1", Link: "https://blog/novini/x", Title: "Same"}}
	if err := s.AssignEntryHashesForFeed(fh, drift); err != nil {
		t.Fatal(err)
	}
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
// articles that share one guid (CBC-style) co-occur in a single feed fetch, so
// the guid is marked sticky (D4) and they stay distinct — and STAY distinct on
// later solo polls because the sticky mark persists.
func TestAssignEntryHashesForFeed_SharedGUIDDistinct(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fh = "cbc"
	// Both distinct articles reuse guid "shared" in ONE batch → sticky.
	batch := []Entry{
		{FeedHash: fh, GUID: "shared", Link: "https://cbc/a", Title: "Article A"},
		{FeedHash: fh, GUID: "shared", Link: "https://cbc/b", Title: "Article B"},
	}
	if err := s.AssignEntryHashesForFeed(fh, batch); err != nil {
		t.Fatal(err)
	}
	if batch[0].Hash == batch[1].Hash {
		t.Fatal("distinct shared-guid articles must not collapse")
	}
	added, err := s.AppendEntries(fh, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("both distinct articles must be stored (%d added)", len(added))
	}
	// A later poll listing only Article A: the persisted sticky mark keeps it
	// at D4, matching the on-disk hash, so nothing re-stores.
	solo := []Entry{{FeedHash: fh, GUID: "shared", Link: "https://cbc/a", Title: "Article A"}}
	if err := s.AssignEntryHashesForFeed(fh, solo); err != nil {
		t.Fatal(err)
	}
	if solo[0].Hash != batch[0].Hash {
		t.Fatalf("sticky mark must persist: %s vs %s", solo[0].Hash, batch[0].Hash)
	}
	added, err = s.AppendEntries(fh, solo)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("solo re-poll of sticky article must not re-store (%d added)", len(added))
	}
}

// TestAssignEntryHashesForFeed_NoExistingHashChange is case 4: a never-reused
// guid-only item gets the plain guid-only EntryHash (D1) — the sticky path must
// not alter it.
func TestAssignEntryHashesForFeed_NoExistingHashChange(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fh = "plain"
	e := []Entry{{FeedHash: fh, GUID: "g-unique", Link: "https://x/1", Title: "Solo"}}
	want := EntryHash("g-unique", "https://x/1", "Solo", time.Time{})
	if err := s.AssignEntryHashesForFeed(fh, e); err != nil {
		t.Fatal(err)
	}
	if e[0].Hash != want {
		t.Fatalf("no-reuse feed-aware hash must equal guid-only EntryHash: %s vs %s", e[0].Hash, want)
	}
}
