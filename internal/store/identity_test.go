package store

import (
	"testing"
	"time"
)

// TestEntryHashGUIDIsSoleIdentity is the core D1 spec assertion: when a guid
// is present, the link is NOT part of the identity, so a slug/category edit
// that moves the link (the svobodnatochka.bg /istorii/→/novini/ bug) keeps the
// same hash.
func TestEntryHashGUIDIsSoleIdentity(t *testing.T) {
	guid := "https://svobodnatochka.bg/?p=12345"
	a := EntryHash(guid, "https://svobodnatochka.bg/istorii/some-slug/", "T", time.Unix(1, 0))
	b := EntryHash(guid, "https://svobodnatochka.bg/novini/some-slug/", "T", time.Unix(1, 0))
	if a != b {
		t.Fatalf("guid-stable link-drift must collapse: %s vs %s", a, b)
	}
	// A different guid is a different identity even with the same link.
	c := EntryHash("https://svobodnatochka.bg/?p=99999", "https://svobodnatochka.bg/istorii/some-slug/", "T", time.Unix(1, 0))
	if c == a {
		t.Fatal("distinct guids must not collapse")
	}
}

// TestEntryHashLinkFallback: empty guid → link is the identity, and distinct
// links stay distinct, identical links collapse.
func TestEntryHashLinkFallback(t *testing.T) {
	a := EntryHash("", "https://example.com/a", "", time.Unix(1, 0))
	a2 := EntryHash("", "https://example.com/a", "different title", time.Unix(99, 0))
	b := EntryHash("", "https://example.com/b", "", time.Unix(1, 0))
	if a != a2 {
		t.Fatal("link-only identity must ignore title/date")
	}
	if a == b {
		t.Fatal("distinct links must not collapse")
	}
}

// TestEntryHashTitleDateLastResort: D3 — no guid and no link → distinct
// title/date stay distinct instead of all collapsing to one "no identity" id.
func TestEntryHashTitleDateLastResort(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)
	a := EntryHash("", "", "First post", t0)
	b := EntryHash("", "", "Second post", t0)
	c := EntryHash("", "", "First post", t1)
	if a == b || a == c || b == c {
		t.Fatalf("title+date must distinguish: %s %s %s", a, b, c)
	}
	// Same title+date → same id (stable).
	if a != EntryHash("", "", "First post", t0.UTC()) {
		t.Fatal("title+date identity must be stable")
	}
}

// TestEntryHashWhitespaceTrim: D2 — surrounding whitespace on guid/link is
// trimmed, but the identity is NOT otherwise normalised (no lowercasing).
func TestEntryHashWhitespaceTrim(t *testing.T) {
	if EntryHash("  g  ", "l", "", time.Time{}) != EntryHash("g", "l", "", time.Time{}) {
		t.Fatal("guid whitespace must be trimmed")
	}
	if EntryHash("", "  https://x/y  ", "", time.Time{}) != EntryHash("", "https://x/y", "", time.Time{}) {
		t.Fatal("link whitespace must be trimmed")
	}
	// Case is significant (IRI path/query are case-sensitive): must NOT collapse.
	if EntryHash("G", "", "", time.Time{}) == EntryHash("g", "", "", time.Time{}) {
		t.Fatal("identity must not be lowercased")
	}
}

// TestEntryHashNormalizeGUIDComposes: the volatile-date strip still applies
// under guid-only identity (NWR-class feeds).
func TestEntryHashNormalizeGUIDComposes(t *testing.T) {
	g1 := "news/42 Mon, 18 May 2026 21:12:26 EDT"
	g2 := "news/42 Mon, 18 May 2026 21:12:00 EDT" // seconds drifted
	if EntryHash(g1, "l1", "", time.Time{}) != EntryHash(g2, "l2", "", time.Time{}) {
		t.Fatal("volatile trailing date must be stripped under guid-only identity")
	}
}

// TestAssignEntryHashesGUIDReuseGuard: D4 — a feed that reuses one guid on
// distinct items must NOT collapse them; the guard mixes the link back in for
// that guid's batch.
func TestAssignEntryHashesGUIDReuseGuard(t *testing.T) {
	// Same guid, different link AND title → reuse → distinct ids.
	reuse := []Entry{
		{GUID: "shared", Link: "https://example.com/1", Title: "One"},
		{GUID: "shared", Link: "https://example.com/2", Title: "Two"},
	}
	AssignEntryHashes(reuse)
	if reuse[0].Hash == reuse[1].Hash {
		t.Fatal("guid-reuse guard must keep distinct items distinct")
	}

	// Same guid, same (link,title) on two lines → a re-poll of one item, NOT
	// reuse → collapse to one id (guid-only).
	dup := []Entry{
		{GUID: "g", Link: "https://example.com/x", Title: "X"},
		{GUID: "g", Link: "https://example.com/x", Title: "X"},
	}
	AssignEntryHashes(dup)
	if dup[0].Hash != dup[1].Hash {
		t.Fatal("identical re-poll must collapse")
	}
	// Same guid, SAME title, different link → slug-edited twin of one
	// article (link drift), NOT reuse → must collapse (guid-only).
	drift := []Entry{
		{GUID: "g", Link: "https://example.com/istorii/s", Title: "Same"},
		{GUID: "g", Link: "https://example.com/novini/s", Title: "Same"},
		{GUID: "", Link: "https://example.com/no-guid", Title: "No guid"}, // empty guid skipped by guard
	}
	AssignEntryHashes(drift)
	if drift[0].Hash != drift[1].Hash {
		t.Fatal("slug-edited twin (same title, drifted link) must collapse")
	}
	if drift[2].Hash == drift[0].Hash {
		t.Fatal("empty-guid entry must keep its own (link) identity")
	}
	// And the non-reuse case equals the plain guid-only EntryHash.
	if dup[0].Hash != EntryHash("g", "https://example.com/x", "X", time.Time{}) {
		t.Fatal("non-reuse identity must equal guid-only EntryHash")
	}
}
