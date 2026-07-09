package store

import (
	"math/rand"
	"testing"
	"time"
)

// TestIdentityHashPureFunctionOfFieldsAndStickySet is the core guarantee: the
// assigned hash is a pure function of (entry fields, sticky set). We generate a
// pool of items (fixed guid/link/title/published each) — some sharing a guid —
// then poll them under many RANDOMLY PERMUTED batch compositions and orderings.
// Because the sticky set only ever grows, once the run has warmed up every item
// must map to exactly ONE hash regardless of which siblings share its batch.
func TestIdentityHashPureFunctionOfFieldsAndStickySet(t *testing.T) {
	type item struct{ guid, link, title string }
	pub := time.Unix(1700000000, 0).UTC()

	// Pool: two guids reused across distinct links (co-occur when both drawn),
	// two guid-only singletons, one empty-guid link-only, one linkless.
	pool := []item{
		{"g-reuse-1", "https://x/a", "A"},
		{"g-reuse-1", "https://x/b", "B"},
		{"g-reuse-2", "https://y/a", "C"},
		{"g-reuse-2", "https://y/b", "D"},
		{"g-solo-1", "https://s/1", "E"},
		{"g-solo-2", "https://s/2", "F"},
		{"", "https://link-only/z", "G"},
		{"", "", "H"},
	}

	rng := rand.New(rand.NewSource(1))
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fh = "propfeed"

	seen := map[string]string{} // item key -> the single hash it ever gets
	key := func(it item) string { return it.guid + "\x00" + it.link + "\x00" + it.title }

	// A warm-up pass that forces both reuse guids to co-occur at least once,
	// so their sticky mark is established, then many random permutations.
	warm := []Entry{
		{FeedHash: fh, GUID: "g-reuse-1", Link: "https://x/a", Title: "A", Published: pub},
		{FeedHash: fh, GUID: "g-reuse-1", Link: "https://x/b", Title: "B", Published: pub},
		{FeedHash: fh, GUID: "g-reuse-2", Link: "https://y/a", Title: "C", Published: pub},
		{FeedHash: fh, GUID: "g-reuse-2", Link: "https://y/b", Title: "D", Published: pub},
	}
	if err := s.AssignEntryHashesForFeed(fh, warm); err != nil {
		t.Fatal(err)
	}

	for iter := 0; iter < 400; iter++ {
		// Random subset in random order.
		perm := rng.Perm(len(pool))
		nsub := 1 + rng.Intn(len(pool))
		var batch []Entry
		for _, idx := range perm[:nsub] {
			it := pool[idx]
			batch = append(batch, Entry{
				FeedHash: fh, GUID: it.guid, Link: it.link, Title: it.title, Published: pub,
			})
		}
		if err := s.AssignEntryHashesForFeed(fh, batch); err != nil {
			t.Fatal(err)
		}
		for _, e := range batch {
			k := key(item{e.GUID, e.Link, e.Title})
			if prev, ok := seen[k]; ok {
				if prev != e.Hash {
					t.Fatalf("iter %d: item %q got two hashes: %s then %s",
						iter, k, prev, e.Hash)
				}
			} else {
				seen[k] = e.Hash
			}
		}
	}

	// Sanity: the two reuse guids' distinct-link members must have distinct
	// hashes (D4 kept them apart), and the solo guids collapse under D1.
	if seen[key(pool[0])] == seen[key(pool[1])] {
		t.Fatal("reused-guid distinct links must not collapse")
	}
}
