package poll

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kfet/harb/internal/store"
)

const (
	// richTextSelector is the Webflow rich-text container that holds a
	// post's article body on its detail page. A page may have several
	// (figure captions, callouts); the article body is the largest by text.
	richTextSelector = ".w-richtext"
	// enrichWorkers bounds concurrent detail-page fetches so the first
	// poll of a large blog (~26 pages for claude.com/blog) stays sane
	// without hammering the origin.
	enrichWorkers = 4
	// enrichTimeout caps each detail-page fetch.
	enrichTimeout = 15 * time.Second
	// enrichMaxBytes caps each detail-page response (matches the preview
	// path's 5 MiB read cap).
	enrichMaxBytes = 5 * 1024 * 1024
	// minArticleChars is the floor below which an extracted article body is
	// treated as too thin to be real content (nav scraps, a cookie banner),
	// so link-only enrichment leaves the entry untouched.
	minArticleChars = 200
)

// articleStripSelectors are removed before article extraction: site
// chrome that is never article content but holds text that would skew a
// naive largest-text heuristic.
const articleStripSelectors = "script,style,noscript,nav,header,footer,aside,form,svg,iframe"

// linkOnlyMediaSelector lists elements whose presence means an entry body
// has real content even when all its text sits inside links (a linked
// hero image, an embedded table or video).
const linkOnlyMediaSelector = "img,picture,figure,video,audio,iframe,embed,object,table"

// enrichWebflowContent fills entry.Content for every entry whose hash is
// new (isNew returns true) by fetching the entry's detail page and
// extracting the largest rich-text block's inner HTML. It mutates entries
// in place and is strictly best-effort: any per-entry fetch/parse failure
// leaves that entry's Content untouched and it never returns an error, so
// the caller's poll cannot fail because of enrichment. Detail-page fetches
// are bounded to enrichWorkers concurrent requests through the SSRF-safe
// client passed in (the poller's own client). Each worker writes a
// distinct slice element, so the in-place mutation is race-free.
func enrichWebflowContent(ctx context.Context, client *http.Client, ua string, entries []store.Entry, isNew func(hash string) bool) {
	var idxs []int
	for i := range entries {
		if entries[i].Link != "" && isNew(entries[i].Hash) {
			idxs = append(idxs, i)
		}
	}
	fetchInto(ctx, client, ua, entries, idxs, fetchRichText, replaceContent)
}

// replaceContent is the default compose: the fetched body becomes the new
// Content verbatim. Used by the webflow path, which has no separate forum
// link to preserve.
func replaceContent(_ store.Entry, fetched string) string { return fetched }

// enrichLinkOnlyContent fills entry.Content for every NEW entry whose feed
// body is link-only (bodyIsLinkOnly) by fetching its external Link and
// extracting the article body. Aggregator feeds (Lobsters, Hacker News,
// Reddit) ship only a "Comments"/"Source" link as the item body; this
// turns such entries into a readable preview. Like enrichWebflowContent it
// is bounded, in-place, race-free, and strictly best-effort — a per-entry
// failure or a page with no extractable article leaves Content untouched.
func enrichLinkOnlyContent(ctx context.Context, client *http.Client, ua string, entries []store.Entry, isNew func(hash string) bool) {
	var idxs []int
	for i := range entries {
		if entries[i].Link != "" && isNew(entries[i].Hash) && bodyIsLinkOnly(entries[i]) {
			idxs = append(idxs, i)
		}
	}
	fetchInto(ctx, client, ua, entries, idxs, fetchArticle, withPreservedLink)
}

// withPreservedLink composes the new Content for a link-only entry that was
// successfully enriched: the entry's original link-only body (the forum/
// "Comments" discussion link) wrapped in a marker paragraph FIRST, then the
// fetched article — the thread link leads the preview instead of trailing it.
// The original anchor markup is preserved verbatim to keep the feed's own
// label. If the original body carries no anchor (nothing to preserve), the
// article is returned unchanged to avoid a dangling empty paragraph.
func withPreservedLink(orig store.Entry, article string) string {
	body := strings.TrimSpace(linkOnlyBody(orig))
	if body == "" || !hasAnchor(body) {
		return article
	}
	return `<p class="enriched-source-link">` + body + `</p>` + article
}

// hasAnchor reports whether s contains at least one <a> element.
func hasAnchor(s string) bool {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(s))
	if err != nil {
		return false
	}
	return doc.Find("a").Length() > 0
}

// fetchInto runs fetch over entries[idxs] on enrichWorkers goroutines and,
// for any non-empty result, writes compose(entry, result) into that entry's
// Content. Each worker owns a distinct slice element, so the in-place
// mutation needs no lock.
func fetchInto(ctx context.Context, client *http.Client, ua string, entries []store.Entry, idxs []int, fetch func(context.Context, *http.Client, string, string) string, compose func(store.Entry, string) string) {
	if len(idxs) == 0 {
		return
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < enrichWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if c := fetch(ctx, client, ua, entries[i].Link); c != "" {
					entries[i].Content = compose(entries[i], c)
				}
			}
		}()
	}
	for _, i := range idxs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// fetchHTML GETs url with the SSRF-safe client, reads up to enrichMaxBytes,
// and returns the response body, or nil on any failure (best-effort). A
// successful empty response is a non-nil empty slice, so callers can use a
// nil check to distinguish failure from an empty body.
func fetchHTML(ctx context.Context, client *http.Client, ua, url string) []byte {
	ctx, cancel := context.WithTimeout(ctx, enrichTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, enrichMaxBytes+1))
	if err != nil {
		return nil
	}
	if int64(len(body)) > enrichMaxBytes {
		return nil
	}
	return body
}

// fetchRichText fetches url and returns the largest .w-richtext block's
// inner HTML, or "" on any failure (best-effort).
func fetchRichText(ctx context.Context, client *http.Client, ua, url string) string {
	body := fetchHTML(ctx, client, ua, url)
	if body == nil {
		return ""
	}
	return largestRichText(body, richTextSelector)
}

// fetchArticle fetches url and returns its extracted main article body as
// inner HTML, or "" on any failure or when no block is substantial enough
// (best-effort).
func fetchArticle(ctx context.Context, client *http.Client, ua, url string) string {
	body := fetchHTML(ctx, client, ua, url)
	if body == nil {
		return ""
	}
	return extractArticle(body)
}

// largestRichText returns the trimmed inner HTML of the selector-matching
// element with the most text content, or "" if none match or the winner is
// empty. Raw HTML is intentional: harb stores entry Content as HTML.
func largestRichText(body []byte, selector string) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	return largestByText(doc.Find(selector), 1)
}

// extractArticle pulls the main readable article body out of a full HTML
// page with a small readability heuristic, returning its inner HTML, or ""
// when no block clears minArticleChars. Best-effort: the UI re-sanitizes
// the result before display, so returning raw page HTML is safe.
//
// Heuristic: after stripping site chrome, prefer the largest-by-text
// <article> then <main> semantic container; failing that, pick the element
// whose direct-child <p> elements hold the most text (the tight article
// body wrapper, not an outer page shell that merely contains it).
func extractArticle(body []byte) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	doc.Find(articleStripSelectors).Remove()
	for _, tag := range []string{"article", "main"} {
		if h := largestByText(doc.Find(tag), minArticleChars); h != "" {
			return h
		}
	}
	return largestByDirectParagraphs(doc)
}

// largestByText returns the inner HTML of the selection's element with the
// most text content when it has at least minLen characters, else "".
func largestByText(sel *goquery.Selection, minLen int) string {
	var best *goquery.Selection
	bestLen := 0
	sel.Each(func(_ int, s *goquery.Selection) {
		if n := len(strings.TrimSpace(s.Text())); n > bestLen {
			bestLen = n
			best = s
		}
	})
	if best == nil || bestLen < minLen {
		return ""
	}
	return innerHTML(best)
}

// largestByDirectParagraphs returns the inner HTML of the element whose
// immediate-child <p> text totals the most (the article body wrapper),
// when it clears minArticleChars, else "".
func largestByDirectParagraphs(doc *goquery.Document) string {
	var best *goquery.Selection
	bestLen := 0
	doc.Find("*").Each(func(_ int, s *goquery.Selection) {
		n := 0
		s.ChildrenFiltered("p").Each(func(_ int, p *goquery.Selection) {
			n += len(strings.TrimSpace(p.Text()))
		})
		if n > bestLen {
			bestLen = n
			best = s
		}
	})
	if best == nil || bestLen < minArticleChars {
		return ""
	}
	return innerHTML(best)
}

// innerHTML returns the trimmed inner HTML of s. s.Html() renders into an
// in-memory buffer and does not error in practice, so its error is ignored.
func innerHTML(s *goquery.Selection) string {
	h, _ := s.Html()
	return strings.TrimSpace(h)
}

// linkOnlyBody returns the entry's displayable body for link-only
// handling: Content when non-empty (after trimming), else Summary. Mirrors
// the UI's entryBody fallback.
func linkOnlyBody(e store.Entry) string {
	if strings.TrimSpace(e.Content) != "" {
		return e.Content
	}
	return e.Summary
}

// bodyIsLinkOnly reports whether e has no displayable article body — its
// Content (falling back to Summary, mirroring the UI's entryBody) is
// empty, whitespace, or only a bare link such as the "Comments"/"Source"
// link aggregator feeds publish in place of a body.
func bodyIsLinkOnly(e store.Entry) bool {
	return isLinkOnlyHTML(linkOnlyBody(e))
}

// isLinkOnlyHTML reports whether s carries no meaningful content outside
// of <a> link labels: empty/whitespace, or text that lives only inside
// anchors, with no embedded media. Mirrors the UI's isLinkOnly (which uses
// x/net/html) with goquery, the parser this package already depends on.
func isLinkOnlyHTML(s string) bool {
	if strings.TrimSpace(s) == "" {
		return true
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(s))
	if err != nil {
		return false
	}
	body := doc.Find("body")
	if body.Find(linkOnlyMediaSelector).Length() > 0 {
		return false
	}
	stripped := body.Clone()
	stripped.Find("a").Remove()
	return strings.TrimSpace(stripped.Text()) == ""
}
