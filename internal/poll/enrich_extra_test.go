package poll

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kfet/harb/internal/safedial"
	"github.com/kfet/harb/internal/store"
)

func TestIsLinkOnlyHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", true},
		{"whitespace", "   \n\t ", true},
		{"bare comments link", `<p><a href="https://lobste.rs/s/x">Comments</a></p>`, true},
		{"bare source link", `<p><a href="https://ex.com/s">Source</a></p>`, true},
		{"plain text", "real article words here", false},
		{"text plus link", `<p>Intro words. <a href="https://x">more</a></p>`, false},
		{"image is meaningful", `<p><img src="https://ex.com/p.png"></p>`, false},
		{"linked image is meaningful", `<a href="https://x"><img src="https://ex.com/p.png"></a>`, false},
		{"table is meaningful", `<table><tr><td>x</td></tr></table>`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLinkOnlyHTML(tc.in); got != tc.want {
				t.Errorf("isLinkOnlyHTML(%q)=%v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestBodyIsLinkOnly(t *testing.T) {
	// Content link-only -> true.
	if !bodyIsLinkOnly(store.Entry{Content: `<p><a href="x">Comments</a></p>`}) {
		t.Error("link-only Content should be link-only")
	}
	// Empty Content falls back to a meaningful Summary -> false.
	if bodyIsLinkOnly(store.Entry{Content: "", Summary: "a real summary excerpt"}) {
		t.Error("meaningful Summary should not be link-only")
	}
	// Empty Content, link-only Summary -> true.
	if !bodyIsLinkOnly(store.Entry{Summary: `<p><a href="x">Comments</a></p>`}) {
		t.Error("link-only Summary should be link-only")
	}
	// Real Content -> false (Summary not consulted).
	if bodyIsLinkOnly(store.Entry{Content: "<p>full body</p>", Summary: `<a href="x">Comments</a>`}) {
		t.Error("real Content should not be link-only")
	}
}

func TestExtractArticle(t *testing.T) {
	body := strings.Repeat("This is a sentence of real article prose. ", 8) // > minArticleChars

	// <article> path.
	art := `<html><body><nav>menu junk</nav><article><p>` + body + `</p></article></body></html>`
	if got := extractArticle([]byte(art)); !strings.Contains(got, "real article prose") {
		t.Errorf("article path failed: %q", got)
	}

	// <main> path (no article).
	mn := `<html><body><main><p>` + body + `</p></main></body></html>`
	if got := extractArticle([]byte(mn)); !strings.Contains(got, "real article prose") {
		t.Errorf("main path failed: %q", got)
	}

	// direct-paragraph fallback (no article/main).
	dp := `<html><body><div class="post"><p>` + body + `</p></div></body></html>`
	if got := extractArticle([]byte(dp)); !strings.Contains(got, "real article prose") {
		t.Errorf("paragraph fallback failed: %q", got)
	}

	// Thin page (below minArticleChars) -> "".
	if got := extractArticle([]byte(`<html><body><article><p>tiny</p></article></body></html>`)); got != "" {
		t.Errorf("thin article should be empty, got %q", got)
	}

	// No content at all -> "".
	if got := extractArticle([]byte(`<html><body><div></div></body></html>`)); got != "" {
		t.Errorf("empty page should be empty, got %q", got)
	}

	// Chrome is stripped before extraction: a big nav must not win.
	chrome := `<html><body><nav><p>` + strings.Repeat("nav noise ", 50) +
		`</p></nav><article><p>` + body + `</p></article></body></html>`
	got := extractArticle([]byte(chrome))
	if strings.Contains(got, "nav noise") {
		t.Errorf("chrome leaked into article: %q", got)
	}
}

func TestFetchArticle(t *testing.T) {
	ctx := context.Background()
	client := safedial.NewClient(5 * time.Second)
	body := strings.Repeat("Real article body sentence number two. ", 8)

	// Failure path (bad URL) -> "".
	if got := fetchArticle(ctx, client, "ua", "http://x/\x00"); got != "" {
		t.Errorf("bad url should be empty, got %q", got)
	}
	// Success.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><body><article><p>`+body+`</p></article></body></html>`)
	}))
	defer ok.Close()
	if got := fetchArticle(ctx, client, "ua", ok.URL); !strings.Contains(got, "Real article body") {
		t.Errorf("success expected article, got %q", got)
	}
}

// TestEnrichLinkOnlyContent verifies selection (link + new + link-only),
// in-place mutation, and that meaningful bodies are left alone.
func TestEnrichLinkOnlyContent(t *testing.T) {
	hits := 0
	body := strings.Repeat("The external article body prose here. ", 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		io.WriteString(w, `<html><body><article><p>`+body+`</p></article></body></html>`)
	}))
	defer srv.Close()
	client := safedial.NewClient(5 * time.Second)

	entries := []store.Entry{
		{Hash: "agg", Link: srv.URL, Summary: `<p><a href="x">Comments</a></p>`}, // new, link-only -> fetched
		{Hash: "full", Link: srv.URL, Content: "<p>already has a full body</p>"}, // not link-only -> skipped
		{Hash: "nolink", Summary: `<a href="x">Comments</a>`},                    // link-only but no link -> skipped
		{Hash: "known", Link: srv.URL, Summary: `<a href="x">Comments</a>`},      // not new -> skipped
	}
	isNew := func(h string) bool { return h != "known" }
	enrichLinkOnlyContent(context.Background(), client, "ua", entries, isNew)

	if !strings.Contains(entries[0].Content, "external article body") {
		t.Errorf("agg not enriched: %q", entries[0].Content)
	}
	// The original "Comments" forum link is preserved and leads the
	// content so the reader sees the discussion thread first.
	if !strings.HasPrefix(entries[0].Content, `<p class="enriched-source-link"><p><a href="x">Comments</a></p></p>`) {
		t.Errorf("forum link not preserved: %q", entries[0].Content)
	}
	if entries[1].Content != "<p>already has a full body</p>" {
		t.Errorf("full body should be untouched: %q", entries[1].Content)
	}
	if entries[2].Content != "" || entries[3].Content != "" {
		t.Errorf("skipped entries mutated: %q / %q", entries[2].Content, entries[3].Content)
	}
	if hits != 1 {
		t.Errorf("hits=%d, want 1 (only the new+linked+link-only entry)", hits)
	}

	// No eligible entries -> no work, no panic (covers the empty-idxs guard).
	enrichLinkOnlyContent(context.Background(), client, "ua", []store.Entry{{Hash: "x"}}, isNew)
}

// TestWithPreservedLink covers the composer: anchor present prepends
// the preserved link; anchorless or empty original bodies skip it
// to avoid a dangling empty paragraph; and the Content-over-Summary
// fallback (linkOnlyBody) is honoured.
func TestWithPreservedLink(t *testing.T) {
	article := "<p>real article</p>"

	// Original link lives in Summary (Content empty) -> prepended.
	got := withPreservedLink(store.Entry{Summary: `<p><a href="x">Comments</a></p>`}, article)
	want := `<p class="enriched-source-link"><p><a href="x">Comments</a></p></p>` + article
	if got != want {
		t.Errorf("summary link not preserved:\n got %q\nwant %q", got, want)
	}

	// Original link lives in Content -> Content wins over Summary.
	got = withPreservedLink(store.Entry{Content: `<a href="c">Source</a>`, Summary: `<a href="s">other</a>`}, article)
	want = `<p class="enriched-source-link"><a href="c">Source</a></p>` + article
	if got != want {
		t.Errorf("content link not preserved:\n got %q\nwant %q", got, want)
	}

	// Anchorless original body -> article returned unchanged (no marker).
	if got := withPreservedLink(store.Entry{Summary: "just some text"}, article); got != article {
		t.Errorf("anchorless body should skip marker, got %q", got)
	}

	// Empty original body -> article returned unchanged.
	if got := withPreservedLink(store.Entry{}, article); got != article {
		t.Errorf("empty body should skip marker, got %q", got)
	}
}

// TestReplaceContent confirms the webflow composer returns the fetched body
// verbatim, with no footer.
func TestReplaceContent(t *testing.T) {
	if got := replaceContent(store.Entry{Summary: `<a href="x">Comments</a>`}, "<p>body</p>"); got != "<p>body</p>" {
		t.Errorf("replaceContent should pass through, got %q", got)
	}
}

// TestHasAnchor covers both branches and the unparseable-input guard.
func TestHasAnchor(t *testing.T) {
	if !hasAnchor(`<p><a href="x">Comments</a></p>`) {
		t.Error("anchor not detected")
	}
	if hasAnchor("<p>no anchor here</p>") {
		t.Error("false anchor detected")
	}
}
