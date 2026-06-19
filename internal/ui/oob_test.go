package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/kfet/harb/internal/store"
)

// seedTagged appends `count` entries to a single feed with the given
// tags and returns the feed URL. All entries start unread.
func seedTagged(t *testing.T, st *store.Store, op *memOPML, url, title string, tags []string, count int) string {
	t.Helper()
	op.op.Feeds = append(op.op.Feeds, store.Feed{XMLURL: url, Title: title, Tags: tags})
	now := time.Now().UTC()
	var es []store.Entry
	for i := 0; i < count; i++ {
		es = append(es, store.Entry{
			GUID:      url + "-g" + strings.Repeat("x", i+1),
			Link:      url + "/" + strings.Repeat("p", i+1),
			Title:     "T",
			Content:   "<p>body</p>",
			Published: now,
			FetchedAt: now,
		})
	}
	st.AppendEntries(store.FeedHash(url), es)
	return url
}

func oobID(body, id string) bool {
	// The element with this id must carry hx-swap-oob="true". We assert
	// on the id substring and the OOB marker being present in the body;
	// fragments are emitted as distinct top-level elements.
	return strings.Contains(body, `id="`+id+`"`)
}

// TestToggleReadOOBSet asserts the EXACT count-badge fan-out across the
// feed-tag shapes the design must cover: untagged, single-tag, and
// multi-tag feeds, in both read directions, plus the star (no-count)
// case. Every mutation must emit the row + detail fragments and, for
// read changes only, the affected count badges with absolute values.
func TestToggleReadOOBSet(t *testing.T) {
	cases := []struct {
		name      string
		tags      []string
		wantHeads []string // count-grouphead-* ids expected
		wantSides []string // count-side-* ids expected (besides all)
	}{
		{"untagged", nil, []string{"count-grouphead-untagged"}, []string{"count-side-untagged"}},
		{"single-tag", []string{"tech"}, []string{"count-grouphead-tech"}, []string{"count-side-tech"}},
		{"multi-tag", []string{"tech", "news"}, []string{"count-grouphead-tech", "count-grouphead-news"}, []string{"count-side-tech", "count-side-news"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, mux, st, op, tok, _ := fixture(t)
			u := seedTagged(t, st, op, "https://"+c.name+".example/feed", "F", c.tags, 2)
			es, _ := st.ListEntries(store.FeedHash(u))
			h := es[0].Hash

			w := do(mux, req("POST", "/ui/entry/read?id="+h+"&state=1", tok, nil))
			if w.Code != 200 {
				t.Fatalf("code=%d", w.Code)
			}
			body := w.Body.String()
			// Row + detail fragments, both OOB.
			for _, id := range []string{"entry-" + h, "entry-detail-" + h} {
				if !oobID(body, id) {
					t.Fatalf("missing fragment id=%s: %s", id, body)
				}
			}
			if !strings.Contains(body, `hx-swap-oob="true"`) {
				t.Fatalf("no OOB markers: %s", body)
			}
			// Always-present count badges.
			fh := store.FeedHash(u)
			for _, id := range []string{"count-feed-" + fh, "count-scope", "count-side-all", "count-total"} {
				if !oobID(body, id) {
					t.Fatalf("missing count id=%s: %s", id, body)
				}
			}
			for _, id := range append(append([]string{}, c.wantHeads...), c.wantSides...) {
				if !oobID(body, id) {
					t.Fatalf("missing tag count id=%s: %s", id, body)
				}
			}
			// The feed had 2 unread; after marking one read the feed
			// count badge must read the ABSOLUTE 1.
			if !strings.Contains(body, `id="count-feed-`+fh+`" class="count" hx-swap-oob="true">1</span>`) {
				t.Fatalf("feed count not absolute 1: %s", body)
			}
			if !strings.Contains(body, `id="count-scope" class="count-inline" hx-swap-oob="true">1</span>`) {
				t.Fatalf("scope count not absolute 1: %s", body)
			}
			// Untagged badges must NOT appear for tagged feeds, and
			// vice-versa.
			if len(c.tags) > 0 && oobID(body, "count-grouphead-untagged") {
				t.Fatalf("tagged feed emitted untagged head: %s", body)
			}
		})
	}
}

// TestMarkAllReadFeedOOB exercises the oob=1 mode used by the home
// master-detail "r" key: marking a whole feed read returns the affected
// count badges (feed→0 plus recomputed aggregates) as OOB swaps instead
// of a redirect, so the server stays the single source of truth.
func TestMarkAllReadFeedOOB(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seedTagged(t, st, op, "https://oob.example/feed", "F", []string{"tech"}, 3)
	seedTagged(t, st, op, "https://other.example/feed", "O", nil, 2) // 2 more unread
	fh := store.FeedHash(u)
	w := do(mux, req("POST", "/ui/mark-all-read?scope=feed&oob=1&id="+u, tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Feed badge → 0; tag + all + total recomputed (3 cleared, 2 left).
	if !strings.Contains(body, `id="count-feed-`+fh+`" class="count" hx-swap-oob="true">0</span>`) {
		t.Fatalf("feed not zeroed: %s", body)
	}
	if !strings.Contains(body, `id="count-grouphead-tech" class="count" hx-swap-oob="true">0</span>`) {
		t.Fatalf("tag head not zeroed: %s", body)
	}
	if !strings.Contains(body, `id="count-side-all" class="count" hx-swap-oob="true">2</span>`) {
		t.Fatalf("all not 2: %s", body)
	}
	if !strings.Contains(body, `id="count-total" class="count-inline" hx-swap-oob="true">2</span>`) {
		t.Fatalf("total not 2: %s", body)
	}
	if !st.EntryState(mustFirstHash(t, st, u)).Read {
		t.Fatal("entries not marked read")
	}
}

// TestMarkAllReadFeedOOBUnknownFeed covers the oob path when the id is
// not a subscribed feed: nothing is emitted (empty 200), no panic.
func TestMarkAllReadFeedOOBUnknownFeed(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://known.example/feed", Title: "K"}}
	w := do(mux, req("POST", "/ui/mark-all-read?scope=feed&oob=1&id=https://nope.example/feed", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Fatalf("expected empty body for unknown feed, got %q", w.Body.String())
	}
}

func mustFirstHash(t *testing.T, st *store.Store, u string) string {
	t.Helper()
	es, _ := st.ListEntries(store.FeedHash(u))
	if len(es) == 0 {
		t.Fatal("no entries")
	}
	return es[0].Hash
}

// and detail but emits NO count badges (star never moves unread counts).
func TestToggleStarOmitsCounts(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seedTagged(t, st, op, "https://star.example/feed", "S", []string{"tech"}, 1)
	es, _ := st.ListEntries(store.FeedHash(u))
	h := es[0].Hash
	w := do(mux, req("POST", "/ui/entry/star?id="+h+"&state=1", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	if !oobID(body, "entry-"+h) || !oobID(body, "entry-detail-"+h) {
		t.Fatalf("star toggle missing row/detail: %s", body)
	}
	if !strings.Contains(body, "unstar") {
		t.Fatalf("star toggle didn't flip to unstar: %s", body)
	}
	for _, id := range []string{"count-feed-", "count-scope", "count-side-all", "count-total", "count-grouphead-tech"} {
		if strings.Contains(body, `id="`+id) {
			t.Fatalf("star toggle leaked count badge %s: %s", id, body)
		}
	}
}

// TestToggleReadAbsoluteTotals checks that count-total / count-side-all
// reflect the global unread total across feeds, not a per-feed delta.
func TestToggleReadAbsoluteTotals(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	a := seedTagged(t, st, op, "https://a.example/feed", "A", []string{"tech"}, 2)
	seedTagged(t, st, op, "https://b.example/feed", "B", nil, 3) // 3 more unread
	es, _ := st.ListEntries(store.FeedHash(a))
	// 5 unread total; mark one of A read → 4 total.
	w := do(mux, req("POST", "/ui/entry/read?id="+es[0].Hash+"&state=1", tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, `id="count-total" class="count-inline" hx-swap-oob="true">4</span>`) {
		t.Fatalf("total not absolute 4: %s", body)
	}
	if !strings.Contains(body, `id="count-side-all" class="count" hx-swap-oob="true">4</span>`) {
		t.Fatalf("side-all not absolute 4: %s", body)
	}
}
