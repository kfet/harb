package reader

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kfet/harb/internal/store"
)

// xcancelRules is the canonical operator rule set from the README.
var xcancelRules = map[string]string{
	"x.com":       "xcancel.com",
	"twitter.com": "xcancel.com",
}

// seedBodies puts one entry per body into a feed and returns the feed
// URL plus the entries in store order.
func seedBodies(t *testing.T, op *memOPML, st *store.Store, bodies ...string) (string, []store.Entry) {
	t.Helper()
	u := "https://feed.example/lr"
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{
		XMLURL: u, Title: "F", HTMLURL: "https://feed.example",
	})
	fh := store.FeedHash(u)
	now := time.Now().UTC()
	es := make([]store.Entry, len(bodies))
	for i, b := range bodies {
		es[i] = store.Entry{
			GUID:      "g" + string(rune('a'+i)),
			Link:      "https://x.com/status/" + string(rune('a'+i)),
			Title:     "Item",
			Content:   b,
			Published: now.Add(time.Duration(-i) * time.Minute),
			FetchedAt: now,
		}
	}
	if _, err := st.AppendEntries(fh, es); err != nil {
		t.Fatal(err)
	}
	out, err := st.ListEntries(fh)
	if err != nil {
		t.Fatal(err)
	}
	return u, out
}

// TestStreamContentsLinkRewrite is the headline behaviour: an x.com
// anchor inside the served article body is remapped, while the item's
// identity (id) and its alternate link are left exactly as stored.
func TestStreamContentsLinkRewrite(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.LinkRewrite = xcancelRules
	feed, entries := seedBodies(t, op, st,
		`<p>see <a href="https://x.com/a/status/1">this</a></p>`)

	w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+feed, tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp streamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	it := resp.Items[0]
	if !strings.Contains(it.Summary.Content, `href="https://xcancel.com/a/status/1"`) {
		t.Errorf("body not rewritten: %q", it.Summary.Content)
	}
	// Identity and "open in browser" target must survive untouched:
	// rewriting `id` resurfaces everything as unread.
	if it.ID != itemID(entries[0].Hash) {
		t.Errorf("id=%q, want %q", it.ID, itemID(entries[0].Hash))
	}
	if len(it.Alternate) != 1 || it.Alternate[0].HREF != entries[0].Link {
		t.Errorf("alternate=%+v, want %q", it.Alternate, entries[0].Link)
	}
	if !strings.Contains(entries[0].Link, "x.com") {
		t.Fatalf("fixture no longer pins an x.com alternate: %q", entries[0].Link)
	}
	// Nothing on disk changed — the transform is serve-time only.
	fresh, err := st.ListEntries(store.FeedHash(feed))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fresh[0].Content, "https://x.com/a/status/1") {
		t.Errorf("stored content was mutated: %q", fresh[0].Content)
	}
}

// TestItemsContentsLinkRewrite pins the same behaviour on the other
// content endpoint Reeder uses.
func TestItemsContentsLinkRewrite(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.LinkRewrite = xcancelRules
	_, entries := seedBodies(t, op, st,
		`<a href="https://x.com/a">one</a> <a href="https://twitter.com/b">two</a>`)

	body := url.Values{"i": {itemID(entries[0].Hash)}}
	w := do(t, mux, "POST", "/reader/api/0/stream/items/contents", tok, body)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp streamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	got := resp.Items[0].Summary.Content
	if strings.Contains(got, "x.com") || strings.Contains(got, "twitter.com") {
		t.Errorf("anchors not all rewritten: %q", got)
	}
	if strings.Count(got, "xcancel.com") != 2 {
		t.Errorf("want two rewritten anchors, got %q", got)
	}
	if resp.Items[0].ID != itemID(entries[0].Hash) {
		t.Errorf("id changed: %q", resp.Items[0].ID)
	}
}

// TestStreamContentsLinkRewriteNonMatching covers the paths that must
// leave the body byte-identical: no rules configured, no matching host,
// non-anchor URL attributes, and markup the tokenizer cannot round-trip
// (fail open).
func TestStreamContentsLinkRewriteNonMatching(t *testing.T) {
	cases := []struct {
		name  string
		rules map[string]string
		body  string
	}{
		{name: "no rules configured", rules: nil,
			body: `<a href="https://x.com/a">t</a>`},
		{name: "no matching host", rules: xcancelRules,
			body: `<p>plain <a href="https://example.com/a">t</a></p>`},
		{name: "img src untouched", rules: xcancelRules,
			body: `<img src="https://x.com/p.png" alt="a">`},
		{name: "malformed markup fails open", rules: xcancelRules,
			body: `<a href="https://x.com/a>unterminated`},
		{name: "bare mention is not a link", rules: xcancelRules,
			body: `<p>found on x.com, sadly</p>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, mux, tok, op, st := fixture(t)
			srv.LinkRewrite = c.rules
			feed, _ := seedBodies(t, op, st, c.body)
			w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+feed, tok, nil)
			if w.Code != 200 {
				t.Fatalf("code=%d", w.Code)
			}
			var resp streamResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Items) != 1 {
				t.Fatalf("items=%d", len(resp.Items))
			}
			if got := resp.Items[0].Summary.Content; got != c.body {
				t.Errorf("content = %q, want byte-identical %q", got, c.body)
			}
		})
	}
}

// TestStreamContentsLinkRewriteSummaryFallback pins that the rewrite
// also covers the Summary-as-body fallback (entries with no Content).
func TestStreamContentsLinkRewriteSummaryFallback(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.LinkRewrite = xcancelRules
	u := "https://feed.example/sf"
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{XMLURL: u, Title: "F"})
	now := time.Now().UTC()
	if _, err := st.AppendEntries(store.FeedHash(u), []store.Entry{{
		GUID:      "g1",
		Title:     "T",
		Summary:   `<A HREF='https://X.com/a'>t</A>`,
		Published: now,
		FetchedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+u, tok, nil)
	var resp streamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	// Upper-case tag and attribute names are preserved verbatim; only
	// the host inside the value moves.
	if got := resp.Items[0].Summary.Content; got != `<A HREF='https://xcancel.com/a'>t</A>` {
		t.Errorf("content=%q", got)
	}
}

// TestStreamContentsLinkRewriteEntities pins that an entity-encoded
// href survives the rewrite still encoded — the query string must not
// come back with a bare `&`, which would corrupt the markup.
func TestStreamContentsLinkRewriteEntities(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.LinkRewrite = xcancelRules
	feed, _ := seedBodies(t, op, st,
		`<a href="https://x.com/s?a=1&amp;b=2">t</a>`)
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+feed, tok, nil)
	var resp streamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	if got := resp.Items[0].Summary.Content; got != `<a href="https://xcancel.com/s?a=1&amp;b=2">t</a>` {
		t.Errorf("content=%q", got)
	}
}
