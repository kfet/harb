package ui

import (
	"strings"
	"testing"

	"github.com/kfet/harb/internal/store"
)

// TestSanitizeHTMLHostile feeds the sanitizer a battery of XSS payloads
// and asserts that the dangerous bits are gone while benign markup and
// the open-in-new-tab rewrite survive.
func TestSanitizeHTMLHostile(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		absent  []string // substrings that must NOT appear (case-insensitive)
		present []string // substrings that MUST appear
	}{
		{
			name:   "script tag dropped with contents",
			in:     `<p>hi</p><script>alert(1)</script>`,
			absent: []string{"<script", "alert(1)"},
			// "hi" must remain.
			present: []string{"hi"},
		},
		{
			name:   "img onerror handler stripped",
			in:     `<img src="x" onerror="alert(1)">`,
			absent: []string{"onerror", "alert"},
		},
		{
			name:   "javascript href neutralised",
			in:     `<a href="javascript:alert(1)">x</a>`,
			absent: []string{"javascript:"},
			// link text kept, anchor may remain without href.
			present: []string{"x"},
		},
		{
			name:   "obfuscated javascript scheme with control chars",
			in:     "<a href=\"java\tscript:alert(1)\">x</a>",
			absent: []string{"javascript", "alert"},
		},
		{
			name:   "data uri href dropped",
			in:     `<a href="data:text/html,<script>alert(1)</script>">x</a>`,
			absent: []string{"data:text/html"},
		},
		{
			name:   "vbscript href dropped",
			in:     `<a href="vbscript:msgbox(1)">x</a>`,
			absent: []string{"vbscript:"},
		},
		{
			name:    "inline style stripped",
			in:      `<p style="background:url(javascript:alert(1))">x</p>`,
			absent:  []string{"style", "javascript"},
			present: []string{"x"},
		},
		{
			name:    "iframe dropped with contents",
			in:      `<iframe src="https://evil.example"></iframe>after`,
			absent:  []string{"<iframe", "evil.example"},
			present: []string{"after"},
		},
		{
			name:    "svg payload dropped",
			in:      `<svg><script>alert(1)</script></svg>ok`,
			absent:  []string{"<svg", "<script", "alert"},
			present: []string{"ok"},
		},
		{
			name:    "object/embed dropped",
			in:      `<object data="x"></object><embed src="y">tail`,
			absent:  []string{"<object", "<embed"},
			present: []string{"tail"},
		},
		{
			name:    "form input button dropped",
			in:      `<form><input name="a"><button>go</button></form>body`,
			absent:  []string{"<form", "<input", "<button"},
			present: []string{"body"},
		},
		{
			name:    "comment dropped",
			in:      `<!-- [if IE]><script>x</script><![endif] -->visible`,
			absent:  []string{"<!--", "<script"},
			present: []string{"visible"},
		},
		{
			name:   "xlink namespaced attr dropped",
			in:     `<a xlink:href="javascript:alert(1)">x</a>`,
			absent: []string{"xlink", "javascript"},
		},
		{
			name:   "mutation xss via misnested tags",
			in:     `<noscript><p title="</noscript><img src=x onerror=alert(1)>">`,
			absent: []string{"onerror", "<noscript"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeHTML(c.in, nil)
			low := strings.ToLower(got)
			for _, a := range c.absent {
				if strings.Contains(low, strings.ToLower(a)) {
					t.Errorf("sanitized output must not contain %q\ngot: %s", a, got)
				}
			}
			for _, p := range c.present {
				if !strings.Contains(low, strings.ToLower(p)) {
					t.Errorf("sanitized output must contain %q\ngot: %s", p, got)
				}
			}
		})
	}
}

// TestSanitizeHTMLPreservesSafeMarkup checks that benign formatting and
// safe links/images survive intact.
func TestSanitizeHTMLPreservesSafeMarkup(t *testing.T) {
	in := `<p>Hello <strong>world</strong> <a href="https://example.com/post">link</a></p>` +
		`<ul><li>one</li><li>two</li></ul>` +
		`<img src="https://example.com/a.png" alt="pic">` +
		`<blockquote cite="https://example.com">quote</blockquote>`
	got := sanitizeHTML(in, nil)
	for _, want := range []string{
		"<strong>world</strong>",
		`href="https://example.com/post"`,
		"<li>one</li>",
		`src="https://example.com/a.png"`,
		`alt="pic"`,
		"<blockquote",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output\ngot: %s", want, got)
		}
	}
}

// TestSanitizeHTMLOpenLinks verifies the open-in-new-tab rewrite is
// applied to safe links and that an author-specified target wins.
func TestSanitizeHTMLOpenLinks(t *testing.T) {
	got := sanitizeHTML(`<a href="https://example.com">x</a>`, nil)
	if !strings.Contains(got, `target="_blank"`) || !strings.Contains(got, `rel="noopener noreferrer"`) {
		t.Fatalf("expected target/rel injection, got: %s", got)
	}
	got2 := sanitizeHTML(`<a href="https://example.com" target="_self">x</a>`, nil)
	if strings.Contains(got2, "_blank") {
		t.Fatalf("author target should win, got: %s", got2)
	}
	if !strings.Contains(got2, `target="_self"`) {
		t.Fatalf("author target dropped, got: %s", got2)
	}
}

// TestSanitizeHTMLEmpty covers the empty-input fast path.
func TestSanitizeHTMLEmpty(t *testing.T) {
	if got := sanitizeHTML("", nil); got != "" {
		t.Fatalf("empty input must yield empty output, got %q", got)
	}
}

// TestSanitizeHTMLUnknownTagUnwrapped checks that an unrecognised but
// non-dangerous element is unwrapped: the tag is dropped, its safe text
// children survive.
func TestSanitizeHTMLUnknownTagUnwrapped(t *testing.T) {
	got := sanitizeHTML(`<center><marquee>hi <b>there</b></marquee></center>`, nil)
	if strings.Contains(got, "marquee") || strings.Contains(got, "center") {
		t.Fatalf("unknown tags should be unwrapped, got: %s", got)
	}
	if !strings.Contains(got, "hi") || !strings.Contains(got, "<b>there</b>") {
		t.Fatalf("children of unwrapped tag should survive, got: %s", got)
	}
}

// TestSanitizeHTMLRelativeURL keeps relative and scheme-relative links.
func TestSanitizeHTMLRelativeURL(t *testing.T) {
	for _, in := range []string{
		`<a href="/path/to/post">x</a>`,
		`<a href="post.html">x</a>`,
		`<a href="//cdn.example/p">x</a>`,
		`<a href="foo/bar:baz">x</a>`,
		`<a href="mailto:a@b.com">x</a>`,
	} {
		got := sanitizeHTML(in, nil)
		if !strings.Contains(got, "href=") {
			t.Errorf("relative/safe href should be kept for %q, got: %s", in, got)
		}
	}
}

// TestEntryBodySanitizes wires the sanitizer through entryBody and
// confirms a hostile feed body cannot inject script.
func TestEntryBodySanitizes(t *testing.T) {
	e := store.Entry{Content: `<p>ok</p><script>alert(1)</script>`}
	got := string(entryBody(e, nil))
	if strings.Contains(got, "<script") {
		t.Fatalf("script leaked through entryBody: %s", got)
	}
	if !strings.Contains(got, "ok") {
		t.Fatalf("benign content dropped: %s", got)
	}
	// Summary fallback path is also sanitized.
	e2 := store.Entry{Summary: `<img src=x onerror=alert(1)>`}
	if strings.Contains(string(entryBody(e2, nil)), "onerror") {
		t.Fatalf("onerror leaked via summary fallback")
	}
}

// TestSanitizeHTMLMagnetLink covers torrent feeds (e.g. showRSS) whose
// item bodies link to magnet: URIs. The body sanitizer must keep the
// magnet href; previously it allowed only http/https/mailto and silently
// stripped the link.
func TestSanitizeHTMLMagnetLink(t *testing.T) {
	in := `<a href="magnet:?xt=urn:btih:24FFFE3CE9A05888DC422C65FD4D7DD17284885E">grab</a>`
	got := sanitizeHTML(in, nil)
	if !strings.Contains(got, "magnet:?xt=urn:btih:24FFFE") {
		t.Fatalf("magnet href should survive sanitization, got: %s", got)
	}
}

// TestLinkURL covers the "source" link policy: trusted schemes (now
// including magnet:) pass through as a template.URL, while unsafe schemes
// and empties yield "" so the template omits the anchor instead of
// emitting html/template's #ZgotmplZ placeholder.
func TestLinkURL(t *testing.T) {
	keep := []string{
		"https://example.com/post",
		"http://example.com/post",
		"mailto:a@b.com",
		"magnet:?xt=urn:btih:ABCDEF",
		"/relative/path",
		// Arbitrary navigable schemes are inert (handler dispatch, no
		// script in our origin) and must survive the deny-list.
		"tel:+15551234",
		"ed2k://|file|x|0|HASH|/",
		"feed:https://example.com/rss",
		"xmpp:user@example.com",
	}
	for _, v := range keep {
		if LinkURL(v, nil) == "" {
			t.Errorf("LinkURL(%q) should be kept, got empty", v)
		}
	}
	drop := []string{
		"",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		"blob:https://example.com/uuid",
		"JaVaScRiPt:alert(1)",
		"java\tscript:alert(1)",
	}
	for _, v := range drop {
		if LinkURL(v, nil) != "" {
			t.Errorf("LinkURL(%q) should be dropped, got %q", v, LinkURL(v, nil))
		}
	}
}

// TestEntryBodyLinkOnlyFallback covers feeds (e.g. CIRA's WordPress feed)
// whose content:encoded is only a bare "Source" link with no article
// body. entryBody must treat such Content as empty and fall back to the
// Summary excerpt, while keeping real Content (including media-only or
// link-wrapped media) intact.
func TestEntryBodyLinkOnlyFallback(t *testing.T) {
	cases := []struct {
		name        string
		entry       store.Entry
		wantContain string
		wantAbsent  string
	}{
		{
			name: "bare source link falls back to summary",
			entry: store.Entry{
				Content: `<p><a href="https://ex.com/x">Source</a></p>`,
				Summary: "The real article excerpt.",
			},
			wantContain: "The real article excerpt.",
			wantAbsent:  "Source",
		},
		{
			name:        "empty content falls back to summary",
			entry:       store.Entry{Content: "", Summary: "excerpt here"},
			wantContain: "excerpt here",
		},
		{
			name:        "whitespace-only content falls back to summary",
			entry:       store.Entry{Content: "  \n\t ", Summary: "excerpt here"},
			wantContain: "excerpt here",
		},
		{
			name: "real text content is kept over summary",
			entry: store.Entry{
				Content: `<p>Full body text.</p>`,
				Summary: "short excerpt",
			},
			wantContain: "Full body text.",
			wantAbsent:  "short excerpt",
		},
		{
			name: "link plus real text is kept",
			entry: store.Entry{
				Content: `<p>Intro paragraph. <a href="https://ex.com">more</a></p>`,
				Summary: "excerpt",
			},
			wantContain: "Intro paragraph.",
			wantAbsent:  "excerpt",
		},
		{
			name: "image-only content is meaningful and kept",
			entry: store.Entry{
				Content: `<p><img src="https://ex.com/p.png" alt="pic"></p>`,
				Summary: "excerpt",
			},
			wantContain: "<img",
			wantAbsent:  "excerpt",
		},
		{
			name: "linked image is meaningful and kept",
			entry: store.Entry{
				Content: `<a href="https://ex.com"><img src="https://ex.com/p.png"></a>`,
				Summary: "excerpt",
			},
			wantContain: "<img",
			wantAbsent:  "excerpt",
		},
		{
			name:        "both empty yields empty",
			entry:       store.Entry{Content: "", Summary: ""},
			wantContain: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(entryBody(tc.entry, nil))
			if tc.wantContain != "" && !strings.Contains(got, tc.wantContain) {
				t.Fatalf("got %q, want it to contain %q", got, tc.wantContain)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Fatalf("got %q, want it to NOT contain %q", got, tc.wantAbsent)
			}
		})
	}
}

// TestIsLinkOnlyParseFallback covers the defensive parse-failure branch:
// malformed input the fragment parser rejects is treated as meaningful so
// a genuine body is never discarded. ParseFragment is total on string
// input, so this also documents that empty/whitespace short-circuits.
func TestIsLinkOnly(t *testing.T) {
	if !isLinkOnly("") {
		t.Fatal("empty should be link-only")
	}
	if !isLinkOnly("   \n ") {
		t.Fatal("whitespace should be link-only")
	}
	if isLinkOnly("plain text") {
		t.Fatal("plain text is meaningful")
	}
	if !isLinkOnly(`<a href="x">label</a>`) {
		t.Fatal("bare link is link-only")
	}
	if isLinkOnly(`<table><tr><td>x</td></tr></table>`) {
		t.Fatal("table is meaningful")
	}
	if !isLinkOnly(`<!-- comment --><a href="x">label</a>`) {
		t.Fatal("comment plus bare link is link-only")
	}
}

// TestHoistSourceLink covers render-time hoisting of the enrichment
// source-link marker: a trailing marker (how entries enriched before the
// ordering flip are stored on disk) moves to the front, an already-leading
// marker is left byte-for-byte alone, and content with no marker is
// untouched.
func TestHoistSourceLink(t *testing.T) {
	const marker = `<div class="enriched-source-link"><a href="x">Comments</a></div>`
	const article = `<p>one</p><p>two</p>`

	if got, want := hoistSourceLink(article+marker), marker+article; got != want {
		t.Errorf("trailing marker not hoisted:\n got %q\nwant %q", got, want)
	}
	// Already first: passed through unchanged.
	if got := hoistSourceLink(marker + article); got != marker+article {
		t.Errorf("leading marker should be untouched, got %q", got)
	}
	// No marker at all: unchanged (and no reparse/reserialize).
	if got := hoistSourceLink(article); got != article {
		t.Errorf("markerless body should be untouched, got %q", got)
	}
	// The class substring appears but on no eligible element: unchanged.
	const decoy = `<span class="enriched-source-link">not a wrapper</span><p>body</p>`
	if got := hoistSourceLink(decoy); got != decoy {
		t.Errorf("non-wrapper decoy should be untouched, got %q", got)
	}
	// Multi-valued class attribute still matches.
	multi := `<p>lead</p><div class="foo enriched-source-link"><a href="x">C</a></div>`
	if got := hoistSourceLink(multi); !strings.HasPrefix(got, `<div class="foo enriched-source-link">`) {
		t.Errorf("multi-class marker not hoisted, got %q", got)
	}
	// Non-class attributes ahead of the class are skipped, not matched.
	attrs := `<p>lead</p><div id="m" class="enriched-source-link"><a href="x">C</a></div>`
	if got := hoistSourceLink(attrs); !strings.HasPrefix(got, `<div id="m" class="enriched-source-link">`) {
		t.Errorf("marker with extra attrs not hoisted, got %q", got)
	}
	// A wrapper carrying an unrelated class is not mistaken for the marker.
	other := `<p>lead</p><div class="other">tail</div>`
	if got := hoistSourceLink(other); got != other {
		t.Errorf("unrelated class should be untouched, got %q", got)
	}
	// Legacy <p> marker wrapping a bare anchor (valid nesting, no split)
	// is still recognised and hoisted.
	legacy := `<p>lead</p><p class="enriched-source-link"><a href="x">C</a></p>`
	if got, want := hoistSourceLink(legacy), `<p class="enriched-source-link"><a href="x">C</a></p><p>lead</p>`; got != want {
		t.Errorf("legacy p marker not hoisted:\n got %q\nwant %q", got, want)
	}
}

// TestHoistSourceLinkSplitParagraph pins the repair of the shape actually
// on disk for entries enriched by v0.20.0-0.20.2: the marker was a <p> and
// the aggregator body is itself a <p>, so the parser splits the invalid
// p-inside-p into an EMPTY marker <p>, the real anchor <p>, and a trailing
// empty <p> from the outer close tag. The whole group must move, and the
// empty artefacts must be dropped.
func TestHoistSourceLinkSplitParagraph(t *testing.T) {
	const article = `<p>article one</p><p>article two</p>`
	const stored = `<p class="enriched-source-link"><p><a href="https://lobste.rs/s/x">Comments</a></p></p>`

	// Legacy footer order (v0.20.0/0.20.1 on disk).
	got := hoistSourceLink(article + stored)
	want := `<p><a href="https://lobste.rs/s/x">Comments</a></p>` + article
	if got != want {
		t.Errorf("split footer not repaired:\n got %q\nwant %q", got, want)
	}

	// v0.20.2 order: the marker leads, but the split still leaves stray
	// empty paragraphs that must be dropped.
	got = hoistSourceLink(stored + article)
	if got != want {
		t.Errorf("split leading marker not cleaned:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "<p></p>") {
		t.Errorf("stray empty paragraph rendered: %q", got)
	}

	// Empty marker with no terminating empty <p> is not the known split
	// shape: only the marker itself is dropped, the article is not
	// swallowed into the hoisted group.
	odd := `<p>lead</p><p class="enriched-source-link"></p><p>tail</p>`
	if got, want := hoistSourceLink(odd), `<p>lead</p><p>tail</p>`; got != want {
		t.Errorf("unterminated empty marker mishandled:\n got %q\nwant %q", got, want)
	}
}

// TestEntryBodyHoistsStoredSourceLink pins the end-to-end render path for
// an already-stored entry whose enriched Content has the source link as a
// trailing footer: the web UI shows the discussion link first, and the
// reordering survives the sanitizer (which strips the marker class).
func TestEntryBodyHoistsStoredSourceLink(t *testing.T) {
	stored := `<p>article body text</p><div class="enriched-source-link"><a href="https://ex.test/c">Comments</a></div>`
	got := string(entryBody(store.Entry{Content: stored}, nil))
	link := `<a href="https://ex.test/c" target="_blank" rel="noopener noreferrer">Comments</a>`
	if !strings.HasPrefix(got, "<div>"+link+"</div>") {
		t.Errorf("source link not first after sanitize: %q", got)
	}
	if !strings.HasSuffix(got, "<p>article body text</p>") {
		t.Errorf("article should follow the link: %q", got)
	}
	// Stored content itself is never rewritten — hoisting is render-only.
	if stored != `<p>article body text</p><div class="enriched-source-link"><a href="https://ex.test/c">Comments</a></div>` {
		t.Fatal("stored content mutated")
	}
}

// TestEntryBodyHoistsRealLobstersShape is the regression test for the
// v0.20.2 bug: the shape below is verbatim what prod stores for a real
// lobste.rs entry (marker <p> wrapping the aggregator's own <p><a>), and
// v0.20.2 rendered the Comments link at the BOTTOM plus two stray empty
// paragraphs, because it hoisted the empty split-off marker instead.
func TestEntryBodyHoistsRealLobstersShape(t *testing.T) {
	const url = "https://lobste.rs/s/x"
	stored := `<p>article body text</p>` +
		`<p class="enriched-source-link"><p><a href="` + url + `">Comments</a></p></p>`
	got := string(entryBody(store.Entry{Content: stored}, nil))

	iLink := strings.Index(got, url)
	iArticle := strings.Index(got, "article body text")
	if iLink < 0 || iArticle < 0 {
		t.Fatalf("missing link or article in %q", got)
	}
	if iLink > iArticle {
		t.Errorf("Comments link must precede the article body: %q", got)
	}
	if strings.Contains(got, "<p></p>") {
		t.Errorf("stray empty paragraph rendered: %q", got)
	}
}
