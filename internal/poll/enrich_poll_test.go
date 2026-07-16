package poll

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kfet/harb/internal/store"
)

const webflowIndex = `<html data-wf-site="x"><head><title>Blog</title></head><body>
	<div class="w-dyn-item"><a href="/p/1"><h3>First</h3></a><div class="cap">May 1, 2026</div></div>
	<div class="w-dyn-item"><a href="/p/2"><h3>Second</h3></a><div class="cap">May 2, 2026</div></div>
</body></html>`

func postPage(path string) string {
	return `<html><body>
		<div class="w-richtext"><p>tiny caption</p></div>
		<div class="w-richtext"><p>This is the real article body for ` + path +
		` containing a good amount of text to be the largest block.</p></div>
	</body></html>`
}

// First poll enriches all new entries from their .w-richtext detail pages;
// a second poll appends nothing new and re-fetches no detail pages.
func TestPollWebflowEnrichesNewEntriesOnly(t *testing.T) {
	p, st, _ := newPoller(t)
	var hitsMu sync.Mutex
	postHits := map[string]int{}
	hit := func(path string) {
		hitsMu.Lock()
		postHits[path]++
		hitsMu.Unlock()
	}
	hitsOf := func(path string) int {
		hitsMu.Lock()
		defer hitsMu.Unlock()
		return postHits[path]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/p/", func(w http.ResponseWriter, r *http.Request) {
		hit(r.URL.Path)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, postPage(r.URL.Path))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, webflowIndex)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	added, err := p.Poll(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Fatalf("added=%d, want 2", added)
	}
	ents, _ := st.ListEntries(store.FeedHash(srv.URL))
	if len(ents) != 2 {
		t.Fatalf("entries=%d", len(ents))
	}
	for _, e := range ents {
		if !strings.Contains(e.Content, "real article body") {
			t.Errorf("entry %q not enriched: %q", e.Link, e.Content)
		}
		if strings.Contains(e.Content, "tiny caption") {
			t.Errorf("picked wrong .w-richtext for %q: %q", e.Link, e.Content)
		}
		if e.Published.IsZero() {
			t.Errorf("entry %q has zero publish date", e.Link)
		}
	}
	if hitsOf("/p/1") != 1 || hitsOf("/p/2") != 1 {
		t.Fatalf("first poll detail hits=%v, want each 1", postHits)
	}

	// Second poll: nothing new, so no detail-page re-fetch.
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if hitsOf("/p/1") != 1 || hitsOf("/p/2") != 1 {
		t.Fatalf("second poll re-fetched detail pages: hits=%v", postHits)
	}
}

// A failing detail-page fetch leaves Content empty and never fails the poll.
func TestPollWebflowEnrichFailureIsSafe(t *testing.T) {
	p, st, _ := newPoller(t)
	index := `<html data-wf-site="x"><head><title>Blog</title></head><body>
		<div class="w-dyn-item"><a href="/p/boom"><h3>Boom</h3></a><div class="cap">May 3, 2026</div></div>
	</body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/p/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, index)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	added, err := p.Poll(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("poll failed despite best-effort enrich: %v", err)
	}
	if added != 1 {
		t.Fatalf("added=%d", added)
	}
	ents, _ := st.ListEntries(store.FeedHash(srv.URL))
	if len(ents) != 1 || ents[0].Content != "" {
		t.Fatalf("expected empty content on fetch failure, got %q", ents[0].Content)
	}
}

// A normal (non-Webflow) RSS feed whose items already carry a body is not
// link-only, so no detail pages are ever fetched.
func TestPollNonWebflowNotEnriched(t *testing.T) {
	p, _, _ := newPoller(t)
	postHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/p/", func(w http.ResponseWriter, r *http.Request) { postHits++ })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>R</title>`+
			`<item><title>i</title><link>`+srvSelf(r)+`/p/1</link>`+
			`<description>This item already has a full readable body.</description></item>`+
			`</channel></rss>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if postHits != 0 {
		t.Fatalf("feed with real bodies was enriched: postHits=%d", postHits)
	}
}

// An aggregator-style RSS feed (Lobsters/HN/Reddit) whose item body is
// only a bare "Comments" link triggers link-only enrichment: the external
// article is fetched and its readable content fills the entry body.
func TestPollLinkOnlyEnriched(t *testing.T) {
	p, st, _ := newPoller(t)
	body := strings.Repeat("The linked external article prose goes here. ", 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><body><article><p>`+body+`</p></article></body></html>`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Agg</title>`+
			`<item><title>story</title><link>`+srvSelf(r)+`/article</link>`+
			`<description>&lt;p&gt;&lt;a href="`+srvSelf(r)+`/c"&gt;Comments&lt;/a&gt;&lt;/p&gt;</description>`+
			`</item></channel></rss>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	ents, _ := st.ListEntries(store.FeedHash(srv.URL))
	if len(ents) != 1 {
		t.Fatalf("entries=%d, want 1", len(ents))
	}
	if !strings.Contains(ents[0].Content, "linked external article prose") {
		t.Errorf("link-only entry not enriched: %q", ents[0].Content)
	}
}

func srvSelf(r *http.Request) string { return "http://" + r.Host }

// When the store can't report known hashes (corrupt ndjson), enrichment is
// skipped; the poll then surfaces the underlying store error from append.
func TestPollWebflowEnrichSkippedOnStoreError(t *testing.T) {
	p, _, dir := newPoller(t)
	index := `<html data-wf-site="x"><head><title>Blog</title></head><body>
		<div class="w-dyn-item"><a href="/p/1"><h3>First</h3></a></div>
	</body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, index)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Make the feed's entries path un-creatable so AppendEntries' MkdirAll
	// fails and the poll surfaces a store error. (Dedup no longer disk-scans,
	// so a corrupt ndjson would be silently appended to — we need a real
	// write-path failure instead.)
	fh := store.FeedHash(srv.URL)
	if err := os.MkdirAll(filepath.Join(dir, "entries"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entries", fh), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected poll error from corrupt store")
	}
}
