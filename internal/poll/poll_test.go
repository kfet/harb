package poll

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfet/harb/internal/poll/observe"
	"github.com/kfet/harb/internal/poll/resolve"
	"github.com/kfet/harb/internal/store"
)

const sampleRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>S</title>
    <item>
      <title>A</title>
      <link>https://x.example/a</link>
      <guid>guid-a</guid>
      <pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate>
      <description>desc-a</description>
    </item>
    <item>
      <title>B</title>
      <link>https://x.example/b</link>
      <guid>guid-b</guid>
      <author>jane@example.com (Jane)</author>
      <pubDate>Tue, 03 Jan 2006 15:04:05 GMT</pubDate>
    </item>
  </channel>
</rss>`

func newPoller(t *testing.T) (*Poller, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := New(s)
	return p, s, dir
}

func TestPollSuccess(t *testing.T) {
	p, _, _ := newPoller(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()
	added, err := p.Poll(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Fatalf("added=%d", added)
	}
	st, _ := p.Store.LoadFeedState(store.FeedHash(srv.URL))
	if st.ETag != `"v1"` {
		t.Fatalf("etag=%q", st.ETag)
	}
	if st.ErrorCount != 0 {
		t.Fatalf("err count=%d", st.ErrorCount)
	}
	if !st.RetryAfter.IsZero() {
		t.Fatalf("retry-after should be zero on success: %v", st.RetryAfter)
	}
	if st.LastFetched.IsZero() {
		t.Fatal("last-fetched not recorded")
	}
}

// TestPollGUIDStableLinkDriftDedups is the end-to-end regression for the
// svobodnatochka.bg bug: a feed re-serves the same <guid> with a drifted
// <link> (slug/category edit). The second poll must NOT create a second
// entry — identity is the guid alone (D1), so the article stays single.
func TestPollGUIDStableLinkDriftDedups(t *testing.T) {
	p, s, _ := newPoller(t)
	const tmpl = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>ST</title>
  <item>
    <title>Same article</title>
    <link>https://svoboda.example%s</link>
    <guid isPermaLink="false">https://svoboda.example/?p=12345</guid>
    <pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate>
    <description>body</description>
  </item>
</channel></rss>`
	link := "/istorii/slug/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, fmt.Sprintf(tmpl, link))
	}))
	defer srv.Close()

	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	// Slug edited after publish: same guid, link moves /istorii/ → /novini/.
	link = "/novini/slug/"
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListEntries(store.FeedHash(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("link-drift twin not deduped: got %d entries %+v", len(listed), listed)
	}
}

// behaviour for the title-entity-decode fix: feed titles / author
// names that arrive with HTML entities (numeric, hex, or named) must
// be decoded once at ingestion so the rest of the pipeline sees plain
// unicode text. Compare against the bug report: a title rendered as
// literal "&#8216;unintended&#8217;" in the web UI instead of curly
// quotes.
func TestPollDecodesHTMLEntitiesInTitleAndAuthor(t *testing.T) {
	p, _, _ := newPoller(t)
	const rssWithEntities = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>S</title>
    <item>
      <title>Motorola says affiliate hijacking of Amazon app was &amp;#8216;unintended&amp;#8217; &amp;amp; broken &amp;#x27;quote&amp;#x27;</title>
      <link>https://x.example/a</link>
      <guid>guid-a</guid>
      <author>jane@example.com (Jane &amp;amp; Co)</author>
      <pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate>
    </item>
  </channel>
</rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, rssWithEntities)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	es, err := p.Store.ListEntries(store.FeedHash(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 {
		t.Fatalf("got %d entries", len(es))
	}
	want := "Motorola says affiliate hijacking of Amazon app was \u2018unintended\u2019 & broken 'quote'"
	if es[0].Title != want {
		t.Fatalf("title not decoded:\n got=%q\nwant=%q", es[0].Title, want)
	}
	if got := es[0].Author; got != "Jane & Co" {
		t.Fatalf("author not decoded: got=%q want=%q", got, "Jane & Co")
	}
}

func TestPollNotModified(t *testing.T) {
	p, _, _ := newPoller(t)
	first := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			w.Header().Set("ETag", `"v1"`)
			io.WriteString(w, sampleRSS)
			return
		}
		if r.Header.Get("If-None-Match") != `"v1"` {
			t.Errorf("missing If-None-Match: %v", r.Header)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	added, err := p.Poll(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatalf("added=%d", added)
	}
}

// TestPollLastSuccessTracking pins the LastSuccess field that the web UI
// surfaces as "last succeeded": it advances on a 2xx sync and on a 304
// not-modified (both are healthy), but stays pinned across an error
// while LastFetched (the attempt time) keeps moving.
func TestPollLastSuccessTracking(t *testing.T) {
	p, _, _ := newPoller(t)
	var mode string // "ok" | "304" | "err"
	first := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "ok":
			if first {
				first = false
				w.Header().Set("ETag", `"v1"`)
			}
			io.WriteString(w, sampleRSS)
		case "304":
			w.WriteHeader(http.StatusNotModified)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	fh := store.FeedHash(srv.URL)

	// 1) Successful 2xx sync records LastSuccess.
	t1 := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return t1 }
	mode = "ok"
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	st, _ := p.Store.LoadFeedState(fh)
	if !st.LastSuccess.Equal(t1) {
		t.Fatalf("LastSuccess=%v want %v after 2xx", st.LastSuccess, t1)
	}

	// 2) A 304 is a healthy sync — LastSuccess advances.
	t2 := t1.Add(time.Hour)
	p.Now = func() time.Time { return t2 }
	mode = "304"
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	st, _ = p.Store.LoadFeedState(fh)
	if !st.LastSuccess.Equal(t2) {
		t.Fatalf("LastSuccess=%v want %v after 304", st.LastSuccess, t2)
	}

	// 3) An error must NOT advance LastSuccess, but LastFetched moves.
	t3 := t2.Add(time.Hour)
	p.Now = func() time.Time { return t3 }
	mode = "err"
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error")
	}
	st, _ = p.Store.LoadFeedState(fh)
	if !st.LastSuccess.Equal(t2) {
		t.Fatalf("LastSuccess=%v want it pinned at %v after error", st.LastSuccess, t2)
	}
	if !st.LastFetched.Equal(t3) {
		t.Fatalf("LastFetched=%v want %v (attempt time advances on error)", st.LastFetched, t3)
	}
	if st.ErrorCount != 1 {
		t.Fatalf("ErrorCount=%d want 1", st.ErrorCount)
	}
}

func TestPollServerError(t *testing.T) {
	p, _, _ := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error")
	}
	st, _ := p.Store.LoadFeedState(store.FeedHash(srv.URL))
	if st.ErrorCount != 1 {
		t.Fatalf("count=%d", st.ErrorCount)
	}
	// Non-429/503 errors do NOT set RetryAfter — the next cycle
	// re-tries immediately.
	if !st.RetryAfter.IsZero() {
		t.Fatalf("retry-after should be zero on 500: %v", st.RetryAfter)
	}
	// Second failure → error count grows (no scheduling change).
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error 2")
	}
	st2, _ := p.Store.LoadFeedState(store.FeedHash(srv.URL))
	if st2.ErrorCount != 2 {
		t.Fatalf("count2=%d", st2.ErrorCount)
	}
}

func TestPollRateLimitedSetsRetryAfter(t *testing.T) {
	p, _, _ := newPoller(t)
	fixed := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return fixed }
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error")
	}
	st, _ := p.Store.LoadFeedState(store.FeedHash(srv.URL))
	want := fixed.Add(300 * time.Second)
	if !st.RetryAfter.Equal(want) {
		t.Fatalf("retry-after=%v want %v", st.RetryAfter, want)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
	// Subsequent Poll within the window returns ErrCooldown without
	// a network round-trip.
	p.Now = func() time.Time { return fixed.Add(60 * time.Second) }
	_, err := p.Poll(context.Background(), srv.URL)
	if !errors.Is(err, ErrCooldown) {
		t.Fatalf("err=%v want ErrCooldown", err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d (server hit during cooldown)", hits)
	}
	// After RetryAfter elapses, Poll proceeds (server hit again).
	p.Now = func() time.Time { return fixed.Add(301 * time.Second) }
	_, _ = p.Poll(context.Background(), srv.URL)
	if hits != 2 {
		t.Fatalf("hits=%d (server not re-hit after window)", hits)
	}
}

func TestPoll503HTTPDateRetryAfter(t *testing.T) {
	p, _, _ := newPoller(t)
	fixed := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return fixed }
	when := fixed.Add(2 * time.Hour).Format(http.TimeFormat)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Retry-After", when)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := p.Poll(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	st, _ := p.Store.LoadFeedState(store.FeedHash(srv.URL))
	if st.RetryAfter.Before(fixed.Add(time.Hour)) {
		t.Fatalf("retry-after too short: %v", st.RetryAfter)
	}
	// Inside the window → ErrCooldown, no hit.
	p.Now = func() time.Time { return fixed.Add(30 * time.Minute) }
	if _, err := p.Poll(context.Background(), srv.URL); !errors.Is(err, ErrCooldown) {
		t.Fatalf("err=%v want ErrCooldown", err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestPollRetryAfterMissing(t *testing.T) {
	if d := parseRetryAfter("", time.Now()); d != 0 {
		t.Fatal("expected 0 for empty")
	}
	if d := parseRetryAfter("not a date", time.Now()); d != 0 {
		t.Fatal("expected 0 for garbage")
	}
	if d := parseRetryAfter(time.Now().Add(-time.Hour).Format(http.TimeFormat), time.Now()); d != 0 {
		t.Fatal("expected 0 for past date")
	}
	if d := parseRetryAfter("-3", time.Now()); d != 0 {
		t.Fatal("expected 0 for negative seconds")
	}
	if d := parseRetryAfter("42", time.Now()); d != 42*time.Second {
		t.Fatalf("want 42s got %v", d)
	}
}

func TestPoll429NoRetryAfterAppliesDefaultCooldown(t *testing.T) {
	p, _, _ := newPoller(t)
	fixed := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return fixed }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error")
	}
	st, _ := p.Store.LoadFeedState(store.FeedHash(srv.URL))
	want := fixed.Add(defaultCooldown)
	if !st.RetryAfter.Equal(want) {
		t.Fatalf("retry-after=%v want %v", st.RetryAfter, want)
	}
}

func TestPollBadXML(t *testing.T) {
	p, _, _ := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not xml at all")
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected parse error")
	}
}

// TestPollSanitizesIllegalXMLControlChars reproduces a real-world feed
// (answer.ai) that embeds a U+0008 byte: Go's encoding/xml rejects the
// whole document, so before sanitization every item was dropped and the
// feed silently stopped updating. After sanitization the stray byte is
// stripped and the items parse normally.
func TestPollSanitizesIllegalXMLControlChars(t *testing.T) {
	p, _, _ := newPoller(t)
	dirty := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<rss version=\"2.0\"><channel>" +
		"<title>S\x08</title>" +
		"<item><title>A\x08B</title><link>https://x.example/a</link>" +
		"<guid>guid-a</guid><description>line1\tline2\x08\x0c</description></item>" +
		"</channel></rss>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, dirty)
	}))
	defer srv.Close()
	added, err := p.Poll(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("poll failed on sanitizable feed: %v", err)
	}
	if added != 1 {
		t.Fatalf("added=%d, want 1", added)
	}
	entries, _ := p.Store.ListEntries(store.FeedHash(srv.URL))
	if len(entries) != 1 {
		t.Fatalf("stored %d entries, want 1", len(entries))
	}
	if entries[0].Title != "AB" {
		t.Fatalf("title=%q, want %q (control char stripped, surrounding text kept)", entries[0].Title, "AB")
	}
	if entries[0].Summary != "line1\tline2" {
		t.Fatalf("summary=%q, want %q (tab kept, U+0008/U+000C stripped)", entries[0].Summary, "line1\tline2")
	}
}

func TestPollBadURL(t *testing.T) {
	p, _, _ := newPoller(t)
	if _, err := p.Poll(context.Background(), "http://[::1]:badport"); err == nil {
		t.Fatal("expected URL build error")
	}
}

func TestPollHTTPError(t *testing.T) {
	p, _, _ := newPoller(t)
	if _, err := p.Poll(context.Background(), "http://127.0.0.1:1/"); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestPoll404(t *testing.T) {
	p, _, _ := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error")
	}
}

func TestPollBodyTooLarge(t *testing.T) {
	p, _, _ := newPoller(t)
	p.MaxBodyBytes = 16
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected too-large error")
	}
}

func TestPollLoadStateError(t *testing.T) {
	p, _, dir := newPoller(t)
	fh := store.FeedHash("http://x/")
	if err := makeStateDir(dir, fh); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Poll(context.Background(), "http://x/"); err == nil {
		t.Fatal("expected load error")
	}
}

func TestPollIfModifiedSince(t *testing.T) {
	p, _, _ := newPoller(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
			io.WriteString(w, sampleRSS)
			return
		}
		if r.Header.Get("If-Modified-Since") != "Wed, 21 Oct 2015 07:28:00 GMT" {
			t.Errorf("missing IMS")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
}

func TestPollAppendError(t *testing.T) {
	p, _, dir := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()
	fh := store.FeedHash(srv.URL)
	if err := makeEntriesNonDir(dir, fh); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected append error")
	}
}

func TestPollSaveStateErrorAfterAppend(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	p, _, dir := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	stateDir := dir + "/state"
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stateDir, 0o755) })
	_, err := p.Poll(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected save error")
	}
}

func TestPollRecordErrSaveFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	p, _, dir := newPoller(t)
	stateDir := dir + "/state"
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stateDir, 0o755) })
	if _, err := p.Poll(context.Background(), "http://127.0.0.1:1/"); err == nil {
		t.Fatal("expected save error in recordErr")
	}
}

func TestPollRateLimitedSaveFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	p, _, dir := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	stateDir := dir + "/state"
	os.MkdirAll(stateDir, 0o755)
	os.Chmod(stateDir, 0o500)
	t.Cleanup(func() { os.Chmod(stateDir, 0o755) })
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error")
	}
}

func TestPoll304SaveFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	p, _, dir := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	stateDir := dir + "/state"
	os.MkdirAll(stateDir, 0o755)
	os.Chmod(stateDir, 0o500)
	t.Cleanup(func() { os.Chmod(stateDir, 0o755) })
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected save error")
	}
}

func TestNewSensibleDefaults(t *testing.T) {
	p := New(nil)
	if p.Client.Timeout == 0 || p.Now == nil || p.MaxBodyBytes == 0 {
		t.Fatalf("New defaults: %+v", p)
	}
}

func TestPollNotModifiedClearsRetryAfter(t *testing.T) {
	p, _, _ := newPoller(t)
	// Seed a RetryAfter in the past (so we don't gate).
	fh := store.FeedHash("http://x/")
	st := store.FeedState{URL: "http://x/", RetryAfter: time.Unix(1, 0).UTC()}
	if err := p.Store.SaveFeedState(fh, st); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	got, _ := p.Store.LoadFeedState(store.FeedHash(srv.URL))
	if !got.RetryAfter.IsZero() {
		t.Fatalf("retry-after should be cleared on 304: %v", got.RetryAfter)
	}
}

const sampleAtom = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Atom</title>
  <updated>2003-12-13T18:30:02Z</updated>
  <id>urn:uuid:60a76c80-d399-11d9-b93C-0003939e0af6</id>
  <entry>
    <title>Atom-Powered Robots Run Amok</title>
    <link href="http://example.org/2003/12/13/atom03"/>
    <id>urn:uuid:1225c695-cfb8-4ebb-aaaa-80da344efa6a</id>
    <updated>2003-12-13T18:30:02Z</updated>
    <summary>Some text.</summary>
  </entry>
</feed>`

func TestPollAtomUpdatedFallback(t *testing.T) {
	p, _, _ := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		io.WriteString(w, sampleAtom)
	}))
	defer srv.Close()
	added, err := p.Poll(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("added=%d", added)
	}
}

const noDateRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel><title>S</title>
  <item><title>X</title><link>https://x/x</link><guid>g</guid></item>
</channel></rss>`

func TestPollNoDateFallsBackToNow(t *testing.T) {
	p, _, _ := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, noDateRSS)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
}

func TestPollBodyReadError(t *testing.T) {
	p, _, _ := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, "short")
		if h, ok := w.(http.Hijacker); ok {
			conn, _, _ := h.Hijack()
			conn.Close()
		}
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected body read error")
	}
}

func TestResetCooldown(t *testing.T) {
	p, _, _ := newPoller(t)
	// No state yet → no-op, no error.
	if err := p.ResetCooldown("http://nope/"); err != nil {
		t.Fatalf("noop err: %v", err)
	}
	// Seed a RetryAfter in the future, then clear it.
	fh := store.FeedHash("http://x/")
	st := store.FeedState{URL: "http://x/", RetryAfter: time.Now().Add(time.Hour)}
	if err := p.Store.SaveFeedState(fh, st); err != nil {
		t.Fatal(err)
	}
	if err := p.ResetCooldown("http://x/"); err != nil {
		t.Fatal(err)
	}
	got, _ := p.Store.LoadFeedState(fh)
	if !got.RetryAfter.IsZero() {
		t.Fatalf("not cleared: %v", got.RetryAfter)
	}
}

func TestResetCooldownLoadError(t *testing.T) {
	p, _, dir := newPoller(t)
	fh := store.FeedHash("http://x/")
	if err := makeStateDir(dir, fh); err != nil {
		t.Fatal(err)
	}
	if err := p.ResetCooldown("http://x/"); err == nil {
		t.Fatal("expected load error")
	}
}

func makeStateDir(dir, fh string) error {
	p := dir + "/state/" + fh + ".json"
	return os.MkdirAll(p, 0o755)
}

func makeEntriesNonDir(dir, fh string) error {
	p := dir + "/entries"
	if err := os.MkdirAll(p, 0o755); err != nil {
		return err
	}
	return os.WriteFile(p+"/"+fh, nil, 0o644)
}

func TestPollUserAgent(t *testing.T) {
	cases := []struct{ set, want string }{
		{"", DefaultUserAgent},       // empty falls back to default
		{"harb/9.9.9", "harb/9.9.9"}, // explicit override is sent verbatim
	}
	for _, c := range cases {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("User-Agent")
			io.WriteString(w, sampleRSS)
		}))
		p, _, _ := newPoller(t)
		p.UserAgent = c.set
		if _, err := p.Poll(context.Background(), srv.URL); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if got != c.want {
			t.Fatalf("UserAgent set=%q: sent %q, want %q", c.set, got, c.want)
		}
		// Guard against the regression we just fixed: never advertise the
		// disclosure URL that tripped CDN bot rules.
		if strings.Contains(got, "github.com") {
			t.Fatalf("User-Agent must not contain a github URL: %q", got)
		}
	}
}

// --- resolver sidecar + observability integration ----------------------

func writeSidecar(t *testing.T, dir, feedURL string, specs []resolve.Spec) {
	t.Helper()
	fh := store.FeedHash(feedURL)
	path := resolve.SidecarPath(dir, fh)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readObservations(t *testing.T, dir, feedURL string) []observe.Observation {
	t.Helper()
	fh := store.FeedHash(feedURL)
	f, err := os.Open(filepath.Join(dir, "observe", fh+".ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []observe.Observation
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev observe.Observation
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// TestPollSidecarShapesRequestAndRecordsSuccess proves a sidecar Spec can
// override the outgoing User-Agent (the CDN-tarpit class of breakage) and
// that the poll outcome is observed with the applied resolver chain.
func TestPollSidecarShapesRequestAndRecordsSuccess(t *testing.T) {
	p, _, dir := newPoller(t)
	p.Observer = observe.NewDiskObserver(dir)

	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()

	writeSidecar(t, dir, srv.URL, []resolve.Spec{
		{Name: "set-header", Params: map[string]string{"key": "User-Agent", "value": "sidecar-ua/9"}, Source: "agent", Note: "CDN tarpit"},
	})

	added, err := p.Poll(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Fatalf("added=%d", added)
	}
	if gotUA != "sidecar-ua/9" {
		t.Fatalf("sidecar did not shape UA: got %q", gotUA)
	}
	evs := readObservations(t, dir, srv.URL)
	last := evs[len(evs)-1]
	if last.Outcome != observe.Success || last.NewEntries != 2 {
		t.Fatalf("observation=%+v", last)
	}
	if len(last.Resolvers) != 3 || last.Resolvers[2] != "set-header" {
		t.Fatalf("resolvers=%v", last.Resolvers)
	}
}

// TestPollSidecarRecodeFixesBrokenFeed proves a response-transforming
// sidecar Spec can repair a body the parser would otherwise reject —
// here a windows-1252 high byte in an element value.
func TestPollSidecarRecodeFixesBrokenFeed(t *testing.T) {
	p, _, dir := newPoller(t)
	p.Observer = observe.NewDiskObserver(dir)

	// 0x92 (CP1252 right-quote) inside the title; declared UTF-8, so it is
	// invalid UTF-8 and parsing yields a mangled title without recode.
	broken := "<?xml version=\"1.0\" encoding=\"UTF-8\"?><rss version=\"2.0\"><channel><title>T</title>" +
		"<item><title>it\x92s</title><link>https://x/a</link><guid>g</guid></item></channel></rss>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, broken)
	}))
	defer srv.Close()

	writeSidecar(t, dir, srv.URL, []resolve.Spec{
		{Name: "recode-charset", Params: map[string]string{"from": "windows-1252"}, Source: "agent"},
	})

	added, err := p.Poll(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if added != 1 {
		t.Fatalf("added=%d", added)
	}
}

// TestPollParseErrorWritesSample proves a parse failure records a
// ParseError observation and saves the raw body sample for the fixer.
func TestPollParseErrorWritesSample(t *testing.T) {
	p, _, dir := newPoller(t)
	p.Observer = observe.NewDiskObserver(dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>not a feed</html>")
	}))
	defer srv.Close()

	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("want parse error")
	}
	evs := readObservations(t, dir, srv.URL)
	last := evs[len(evs)-1]
	if last.Outcome != observe.ParseError || !last.Sample {
		t.Fatalf("observation=%+v", last)
	}
	fh := store.FeedHash(srv.URL)
	sample, err := os.ReadFile(filepath.Join(dir, "observe", fh+".sample"))
	if err != nil {
		t.Fatalf("sample not written: %v", err)
	}
	if !strings.Contains(string(sample), "not a feed") {
		t.Fatalf("sample=%q", sample)
	}
}

// hookResolver is a test double implementing resolve.Resolver, used with an
// injected Poller.Resolve to drive the ShapeRequest / Transform error paths
// that no real primitive can trigger.
type hookResolver struct {
	shapeErr, transErr error
}

func (h hookResolver) Name() string                  { return "hook" }
func (h hookResolver) Applies(resolve.FeedMeta) bool { return true }
func (h hookResolver) ShapeRequest(*http.Request) error {
	return h.shapeErr
}
func (h hookResolver) Transform(b []byte, _ resolve.FeedMeta) ([]byte, error) {
	return b, h.transErr
}

func TestPollShapeRequestError(t *testing.T) {
	p, _, dir := newPoller(t)
	p.Observer = observe.NewDiskObserver(dir)
	boom := errors.New("shape boom")
	p.Resolve = func(string, string) (resolve.Chain, error) {
		return resolve.NewChain(hookResolver{shapeErr: boom}), nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()

	if _, err := p.Poll(context.Background(), srv.URL); err != boom {
		t.Fatalf("err=%v, want shape boom", err)
	}
	evs := readObservations(t, dir, srv.URL)
	if last := evs[len(evs)-1]; last.Outcome != observe.FetchError {
		t.Fatalf("outcome=%v, want fetch-error", last.Outcome)
	}
	st, _ := p.Store.LoadFeedState(store.FeedHash(srv.URL))
	if st.ErrorCount != 1 {
		t.Fatalf("error count=%d", st.ErrorCount)
	}
}

func TestPollTransformError(t *testing.T) {
	p, _, dir := newPoller(t)
	p.Observer = observe.NewDiskObserver(dir)
	boom := errors.New("transform boom")
	p.Resolve = func(string, string) (resolve.Chain, error) {
		return resolve.NewChain(hookResolver{transErr: boom}), nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()

	if _, err := p.Poll(context.Background(), srv.URL); err != boom {
		t.Fatalf("err=%v, want transform boom", err)
	}
	evs := readObservations(t, dir, srv.URL)
	last := evs[len(evs)-1]
	if last.Outcome != observe.ParseError || !last.Sample {
		t.Fatalf("observation=%+v", last)
	}
	fh := store.FeedHash(srv.URL)
	if _, err := os.Stat(filepath.Join(dir, "observe", fh+".sample")); err != nil {
		t.Fatalf("sample not written: %v", err)
	}
}

// TestPollNilHooksUseDefaults covers the obs() and Resolve nil-guards: a
// Poller whose Observer and Resolve are nil must poll without panicking,
// falling back to observe.Nop and resolve.Load respectively.
func TestPollNilHooksUseDefaults(t *testing.T) {
	p, _, _ := newPoller(t)
	p.Observer = nil
	p.Resolve = nil
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
}

// TestMain opts the suite out of the SSRF guard: the test httptest
// servers all bind to loopback, which the guard blocks by default. The
// guard itself is verified by TestPollSSRFBlocksLoopback (which clears
// the override) and the internal/safedial tests.
func TestMain(m *testing.M) {
	os.Setenv("HARB_ALLOW_PRIVATE_FETCH", "1")
	os.Exit(m.Run())
}

// TestPollSSRFBlocksLoopback confirms the SSRF guard is wired into the
// default Poller: with the opt-out cleared, polling a loopback URL is
// refused before any HTTP exchange.
func TestPollSSRFBlocksLoopback(t *testing.T) {
	t.Setenv("HARB_ALLOW_PRIVATE_FETCH", "")
	p, _, _ := newPoller(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()
	if _, err := p.Poll(context.Background(), srv.URL); err == nil {
		t.Fatal("expected SSRF guard to block loopback poll")
	} else if !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("expected non-public block error, got: %v", err)
	}
}
