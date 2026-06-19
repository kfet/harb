package ui

import (
	"html/template"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfet/harb/internal/auth"
	"github.com/kfet/harb/internal/config"
	"github.com/kfet/harb/internal/reader"
	"github.com/kfet/harb/internal/store"
)

type memOPML struct {
	op      store.OPML
	loadErr error
	saveErr error
}

func (m *memOPML) Load() (*store.OPML, error) {
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	cp := m.op
	cp.Feeds = append([]store.Feed{}, m.op.Feeds...)
	return &cp, nil
}
func (m *memOPML) Save(o *store.OPML) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.op = *o
	return nil
}

// Update mirrors config.FileOPML.Update: load a copy, mutate, save.
func (m *memOPML) Update(fn func(*store.OPML) error) error {
	cur, err := m.Load()
	if err != nil {
		return err
	}
	if err := fn(cur); err != nil {
		return err
	}
	return m.Save(cur)
}

// Compile-time check our memOPML satisfies the reader's interface too —
// keeps the two packages aligned.
var _ reader.OPMLProvider = (*memOPML)(nil)

var testPwHash = mustHashPw()

func mustHashPw() string {
	h, err := auth.HashPassword("p")
	if err != nil {
		panic(err)
	}
	return h
}

func fixture(t *testing.T) (*Server, *http.ServeMux, *store.Store, *memOPML, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	as, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"), auth.Config{Username: "u", PasswordHash: testPwHash})
	op := &memOPML{}
	overrideDir := filepath.Join(dir, "cfg")
	srv, err := New(st, as, op, "dark", overrideDir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	tok, _ := as.IssueSession("u", "p")
	return srv, mux, st, op, tok, dir
}

func req(method, path, tok string, form url.Values) *http.Request {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if tok != "" {
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	}
	return r
}

func do(mux *http.ServeMux, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestLoginGetAndPost(t *testing.T) {
	_, mux, _, _, _, _ := fixture(t)
	w := do(mux, req("GET", "/ui/login", "", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "sign in") {
		t.Fatalf("login GET: %d %s", w.Code, w.Body.String())
	}
	form := url.Values{"username": {"u"}, "password": {"p"}}
	w2 := do(mux, req("POST", "/ui/login", "", form))
	if w2.Code != 303 {
		t.Fatalf("login POST: %d", w2.Code)
	}
	// bad creds
	w3 := do(mux, req("POST", "/ui/login", "", url.Values{"username": {"u"}, "password": {"x"}}))
	if w3.Code != 401 {
		t.Fatalf("bad code: %d", w3.Code)
	}
	// bad method
	w4 := do(mux, req("DELETE", "/ui/login", "", nil))
	if w4.Code != 405 {
		t.Fatalf("delete code: %d", w4.Code)
	}
}

func TestLogout(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	w := do(mux, req("POST", "/ui/logout", tok, nil))
	if w.Code != 303 {
		t.Fatalf("code=%d", w.Code)
	}
	// Without a session cookie too.
	w2 := do(mux, req("POST", "/ui/logout", "", nil))
	if w2.Code != 303 {
		t.Fatalf("nocookie code=%d", w2.Code)
	}
}

func TestRequireSessionRedirects(t *testing.T) {
	_, mux, _, _, _, _ := fixture(t)
	w := do(mux, req("GET", "/ui/", "", nil))
	if w.Code != 303 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestHome(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	// Home now defaults to "unread only", which would hide a feed
	// with no entries. Pass ?unread=0 to exercise the show-all path
	// where the seeded feed is visible.
	w := do(mux, req("GET", "/ui/?unread=0", tok, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "X") {
		t.Fatalf("home: %d %s", w.Code, w.Body.String())
	}
	// Non-/ui/ path under prefix → 404.
	w2 := do(mux, req("GET", "/ui/nope", tok, nil))
	if w2.Code != 404 {
		t.Fatalf("nope code=%d", w2.Code)
	}
	// Load err
	op.loadErr = errBoom
	w3 := do(mux, req("GET", "/ui/", tok, nil))
	if w3.Code != 500 {
		t.Fatalf("home load err: %d", w3.Code)
	}
}

func TestHelpOverlayTouchGestures(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	w := do(mux, req("GET", "/ui/", tok, nil))
	if w.Code != 200 {
		t.Fatalf("home code=%d", w.Code)
	}
	body := w.Body.String()
	// The ? help overlay must document the revised (v2) swipe mapping:
	// swipe-right = up, swipe-left = context action on entry/article and
	// feed rows.
	for _, want := range []string{
		"touch gestures",
		"swipe →",
		"up the hierarchy",
		"swipe ← (entry/article)",
		"short: toggle read · long: toggle star",
		"swipe ← (feed row)",
		"open / drill into the feed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("help overlay missing %q", want)
		}
	}
}

func seed(t *testing.T, st *store.Store, op *memOPML, count int) string {
	t.Helper()
	u := "https://demo.example/feed"
	op.op.Feeds = []store.Feed{{XMLURL: u, Title: "Demo", HTMLURL: "https://demo.example"}}
	now := time.Now().UTC()
	var es []store.Entry
	for i := 0; i < count; i++ {
		es = append(es, store.Entry{
			GUID:      "g" + strings.Repeat("x", i+1),
			Link:      "https://demo.example/" + strings.Repeat("p", i+1),
			Title:     "T" + strings.Repeat("!", i+1),
			Content:   "<p>body</p>",
			Summary:   "summary",
			Published: now,
			FetchedAt: now,
		})
	}
	st.AppendEntries(store.FeedHash(u), es)
	return u
}

func TestFeedAndEntry(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 2)
	w := do(mux, req("GET", "/ui/feed?id="+u, tok, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Demo") {
		t.Fatalf("feed code=%d body=%s", w.Code, w.Body.String())
	}
	// missing id
	if w := do(mux, req("GET", "/ui/feed", tok, nil)); w.Code != 400 {
		t.Fatalf("miss code=%d", w.Code)
	}
	// not found
	if w := do(mux, req("GET", "/ui/feed?id=nope", tok, nil)); w.Code != 404 {
		t.Fatalf("nope code=%d", w.Code)
	}
	// entry
	es, _ := st.ListEntries(store.FeedHash(u))
	w2 := do(mux, req("GET", "/ui/entry?id="+es[0].Hash, tok, nil))
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "body") {
		t.Fatalf("entry code=%d body=%s", w2.Code, w2.Body.String())
	}
	// entry missing
	if w := do(mux, req("GET", "/ui/entry", tok, nil)); w.Code != 400 {
		t.Fatalf("entry miss=%d", w.Code)
	}
	if w := do(mux, req("GET", "/ui/entry?id=nosuch", tok, nil)); w.Code != 404 {
		t.Fatalf("entry nosuch=%d", w.Code)
	}
	// unread-only filter on a feed: mark first entry read, then ?unread=1
	// should hide it (and trigger the !st.Read==false continue branch).
	if err := st.SetRead(es[0].Hash, true); err != nil {
		t.Fatalf("setread: %v", err)
	}
	wu := do(mux, req("GET", "/ui/feed?id="+u+"&unread=1", tok, nil))
	if wu.Code != 200 {
		t.Fatalf("feed unread code=%d", wu.Code)
	}
	if strings.Contains(wu.Body.String(), "id=\"entry-"+es[0].Hash+"\"") {
		t.Fatalf("read entry should be filtered out by ?unread=1")
	}
	if !strings.Contains(wu.Body.String(), "unread only") {
		t.Fatalf("unread-only pill missing: %s", wu.Body.String())
	}
}

func TestFeedAndEntryErrors(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	// Missing id on the feed page → 400.
	if w := do(mux, req("GET", "/ui/feed", tok, nil)); w.Code != 400 {
		t.Fatalf("feed missing id: %d", w.Code)
	}
	// Unknown feed id → 404.
	if w := do(mux, req("GET", "/ui/feed?id=https://nope/x", tok, nil)); w.Code != 404 {
		t.Fatalf("feed unknown id: %d", w.Code)
	}
	// Missing id on the entry page → 400.
	if w := do(mux, req("GET", "/ui/entry", tok, nil)); w.Code != 400 {
		t.Fatalf("entry missing id: %d", w.Code)
	}
	// Unknown entry hash → 404.
	if w := do(mux, req("GET", "/ui/entry?id=deadbeef", tok, nil)); w.Code != 404 {
		t.Fatalf("entry unknown hash: %d", w.Code)
	}
	// Load err for feed + entry → 500.
	op.loadErr = errBoom
	for _, p := range []string{"/ui/feed?id=" + u, "/ui/entry?id=x"} {
		w := do(mux, req("GET", p, tok, nil))
		if w.Code != 500 {
			t.Fatalf("%s load err: %d", p, w.Code)
		}
	}
}

func TestSetReadAndStarred(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	es, _ := st.ListEntries(store.FeedHash(u))
	h := es[0].Hash
	// read
	w := do(mux, req("POST", "/ui/entry/read?id="+h+"&state=1", tok, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "mark unread") {
		t.Fatalf("read code=%d body=%s", w.Code, w.Body.String())
	}
	if !st.EntryState(h).Read {
		t.Fatal("not set")
	}
	// starred
	w = do(mux, req("POST", "/ui/entry/star?id="+h+"&state=1", tok, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "unstar") {
		t.Fatalf("star code=%d body=%s", w.Code, w.Body.String())
	}
	// missing id
	if w := do(mux, req("POST", "/ui/entry/read", tok, nil)); w.Code != 400 {
		t.Fatalf("miss=%d", w.Code)
	}
	// load err
	op.loadErr = errBoom
	if w := do(mux, req("POST", "/ui/entry/read?id="+h, tok, nil)); w.Code != 500 {
		t.Fatalf("loaderr=%d", w.Code)
	}
}

func TestSetReadNotFound(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://nofeed/feed"}}
	if w := do(mux, req("POST", "/ui/entry/read?id=nosuch&state=1", tok, nil)); w.Code != 404 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestSetFlagStoreError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	es, _ := st.ListEntries(store.FeedHash(u))
	h := es[0].Hash
	os.Chmod(st.Dir, 0o500)
	t.Cleanup(func() { os.Chmod(st.Dir, 0o755) })
	if w := do(mux, req("POST", "/ui/entry/read?id="+h+"&state=1", tok, nil)); w.Code != 500 {
		t.Fatalf("read code=%d", w.Code)
	}
	if w := do(mux, req("POST", "/ui/entry/star?id="+h+"&state=1", tok, nil)); w.Code != 500 {
		t.Fatalf("star code=%d", w.Code)
	}
}

func TestStatic(t *testing.T) {
	_, mux, _, _, _, _ := fixture(t)
	w := do(mux, req("GET", "/ui/static/style.css", "", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("style: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	// missing
	if w := do(mux, req("GET", "/ui/static/nope.css", "", nil)); w.Code != 404 {
		t.Fatalf("nope=%d", w.Code)
	}
}

func TestThemeOverride(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "cfg", "overrides")
	os.MkdirAll(overrides, 0o755)
	os.WriteFile(filepath.Join(overrides, "theme.css"), []byte("body { color: red }"), 0o644)
	// Template override too — replace home.html.
	tplOverrides := filepath.Join(overrides, "templates")
	os.MkdirAll(tplOverrides, 0o755)
	os.WriteFile(filepath.Join(tplOverrides, "home.html"), []byte(
		`{{define "home"}}OVERRIDDEN{{end}}`,
	), 0o644)

	st, _ := store.Open(t.TempDir())
	as, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"), auth.Config{Username: "u", PasswordHash: testPwHash})
	srv, err := New(st, as, &memOPML{}, "", filepath.Join(dir, "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	tok, _ := as.IssueSession("u", "p")
	// home renders override
	w := do(mux, req("GET", "/ui/", tok, nil))
	if !strings.Contains(w.Body.String(), "OVERRIDDEN") {
		t.Fatalf("override not applied: %s", w.Body.String())
	}
	// theme.css served from override
	w2 := do(mux, req("GET", "/ui/static/theme.css", "", nil))
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "red") {
		t.Fatalf("theme code=%d body=%s", w2.Code, w2.Body.String())
	}
}

func TestNewBadOverrides(t *testing.T) {
	dir := t.TempDir()
	// Make overrides/templates/foo.html invalid.
	bad := filepath.Join(dir, "cfg", "overrides", "templates")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, "x.html"), []byte("{{define"), 0o644)
	st, _ := store.Open(t.TempDir())
	as, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"), auth.Config{})
	if _, err := New(st, as, &memOPML{}, "", filepath.Join(dir, "cfg")); err == nil {
		t.Fatal("expected parse err")
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	srv, _, _, _, _, _ := fixture(t)
	w := httptest.NewRecorder()
	srv.render(w, "no-such", nil)
	if w.Code != 500 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestLoadTemplatesGlobError(t *testing.T) {
	// filepath.Glob errors on a malformed pattern. We can drive this by
	// supplying an overrides root containing a `[` literal — but Glob
	// treats the whole path uniformly. Simulate by providing a path
	// that contains an unmatched bracket.
	dir := t.TempDir()
	bad := dir + string(filepath.Separator) + "["
	srv := &Server{Overrides: bad}
	if err := srv.loadTemplates(); err == nil {
		t.Fatal("expected glob err")
	}
}

// errBoom for shared test errors.
type berr string

func (b berr) Error() string { return string(b) }

var errBoom = berr("boom")

func TestAllUnread(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 3)
	es, _ := st.ListEntries(store.FeedHash(u))
	st.SetRead(es[0].Hash, true) // 2 unread left
	w := do(mux, req("GET", "/ui/all", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	// 2 unread <li> rows
	if strings.Count(body, `<li id="entry-`) != 2 {
		t.Fatalf("expected 2 entries, body=%s", body)
	}
	// Mark-all button present
	if !strings.Contains(body, "mark all read") {
		t.Fatal("missing mark-all button")
	}
}

func TestStarredView(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 3)
	es, _ := st.ListEntries(store.FeedHash(u))
	st.SetStarred(es[0].Hash, true)
	st.SetStarred(es[1].Hash, true)
	w := do(mux, req("GET", "/ui/starred", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if strings.Count(w.Body.String(), `<li id="entry-`) != 2 {
		t.Fatalf("expected 2 starred, body=%s", w.Body.String())
	}
}

func TestCrossFeedLoadErr(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.loadErr = errBoom
	for _, p := range []string{"/ui/all", "/ui/starred"} {
		w := do(mux, req("GET", p, tok, nil))
		if w.Code != 500 {
			t.Fatalf("%s code=%d", p, w.Code)
		}
	}
}

func TestMarkAllReadFeed(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 3)
	w := do(mux, req("POST", "/ui/mark-all-read?scope=feed&id="+u, tok, nil))
	if w.Code != 303 {
		t.Fatalf("code=%d", w.Code)
	}
	// After marking a feed read, send the user back to the feeds list
	// and let their persisted "unread only" choice (or the default)
	// decide the view — keep walking the queue rather than re-pinning
	// the filter on every action.
	if loc := w.Header().Get("Location"); loc != "./" {
		t.Fatalf("redirect=%q", loc)
	}
	for _, e := range mustList(t, st, u) {
		if !st.EntryState(e.Hash).Read {
			t.Fatalf("not read: %s", e.Hash)
		}
	}
}

func TestMarkAllReadAll(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 2)
	w := do(mux, req("POST", "/ui/mark-all-read?scope=all", tok, nil))
	if w.Code != 303 {
		t.Fatalf("code=%d", w.Code)
	}
	for _, e := range mustList(t, st, u) {
		if !st.EntryState(e.Hash).Read {
			t.Fatalf("not read: %s", e.Hash)
		}
	}
	// idempotent: second call leaves entries read (and exercises the
	// already-read continue branch).
	if w := do(mux, req("POST", "/ui/mark-all-read?scope=all", tok, nil)); w.Code != 303 {
		t.Fatalf("second code=%d", w.Code)
	}
}

func TestMarkAllReadBadInputs(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	if w := do(mux, req("GET", "/ui/mark-all-read?scope=feed&id=x", tok, nil)); w.Code != 405 {
		t.Fatalf("method=%d", w.Code)
	}
	if w := do(mux, req("POST", "/ui/mark-all-read", tok, nil)); w.Code != 400 {
		t.Fatalf("scope=%d", w.Code)
	}
	if w := do(mux, req("POST", "/ui/mark-all-read?scope=feed", tok, nil)); w.Code != 400 {
		t.Fatalf("missing id=%d", w.Code)
	}
}

func TestMarkAllReadErrors(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	seed(t, st, op, 1)
	// Load err
	op.loadErr = errBoom
	if w := do(mux, req("POST", "/ui/mark-all-read?scope=all", tok, nil)); w.Code != 500 {
		t.Fatalf("load=%d", w.Code)
	}
}

func TestMarkAllReadSetReadErrFeed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	if err := os.Chmod(st.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(st.Dir, 0o755) })
	if w := do(mux, req("POST", "/ui/mark-all-read?scope=feed&id="+u, tok, nil)); w.Code != 500 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMarkAllReadSetReadErrAll(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	_, mux, st, op, tok, _ := fixture(t)
	seed(t, st, op, 1)
	if err := os.Chmod(st.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(st.Dir, 0o755) })
	if w := do(mux, req("POST", "/ui/mark-all-read?scope=all", tok, nil)); w.Code != 500 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func mustList(t *testing.T, st *store.Store, u string) []store.Entry {
	t.Helper()
	es, err := st.ListEntries(store.FeedHash(u))
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestEntryViewHasButtons(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	es, _ := st.ListEntries(store.FeedHash(u))
	w := do(mux, req("GET", "/ui/entry?id="+es[0].Hash, tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "mark read") || !strings.Contains(body, "star") {
		t.Fatalf("entry detail missing buttons: %s", body)
	}
	// feed back-link
	if !strings.Contains(body, "Demo") {
		t.Fatalf("entry detail missing feed link: %s", body)
	}
}

func TestSetReadDetailView(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	es, _ := st.ListEntries(store.FeedHash(u))
	h := es[0].Hash
	w := do(mux, req("POST", "/ui/entry/read?id="+h+"&state=1&view=detail", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "entry-detail-"+h) {
		t.Fatalf("expected detail fragment, got %s", body)
	}
	if !strings.Contains(body, "mark unread") {
		t.Fatalf("expected unread toggle: %s", body)
	}
	// Out-of-band row patch keeps the list row in sync with the
	// detail toggle in split-panel mode.
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Fatalf("expected OOB row fragment: %s", body)
	}
	if !strings.Contains(body, `id="entry-`+h+`"`) {
		t.Fatalf("expected OOB row to carry list-row id: %s", body)
	}
	// star toggle in detail view — also emits OOB row.
	w = do(mux, req("POST", "/ui/entry/star?id="+h+"&state=1&view=detail", tok, nil))
	if !strings.Contains(w.Body.String(), "unstar") {
		t.Fatalf("expected star: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `hx-swap-oob="true"`) {
		t.Fatalf("expected OOB row fragment on star toggle: %s", w.Body.String())
	}
}

func TestFeedAdd(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	form := url.Values{"url": {"https://new.example/feed"}, "title": {"New"}, "tags": {"News"}}
	w := do(mux, req("POST", "/ui/feed/add", tok, form))
	if w.Code != 303 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if len(op.op.Feeds) != 1 || !op.op.Feeds[0].HasTag("News") {
		t.Fatalf("feeds=%+v", op.op.Feeds)
	}
	// title defaults to url
	form2 := url.Values{"url": {"https://other.example/feed"}}
	if w := do(mux, req("POST", "/ui/feed/add", tok, form2)); w.Code != 303 {
		t.Fatalf("default title code=%d", w.Code)
	}
	if op.op.Feeds[1].Title != "https://other.example/feed" {
		t.Fatalf("title=%q", op.op.Feeds[1].Title)
	}
}

func TestFeedAddErrors(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	// wrong method
	if w := do(mux, req("GET", "/ui/feed/add", tok, nil)); w.Code != 405 {
		t.Fatalf("method=%d", w.Code)
	}
	// missing url
	if w := do(mux, req("POST", "/ui/feed/add", tok, url.Values{})); w.Code != 400 {
		t.Fatalf("missing=%d", w.Code)
	}
	// load err
	op.loadErr = errBoom
	form := url.Values{"url": {"https://x/feed"}}
	if w := do(mux, req("POST", "/ui/feed/add", tok, form)); w.Code != 500 {
		t.Fatalf("load=%d", w.Code)
	}
	op.loadErr = nil
	op.saveErr = errBoom
	if w := do(mux, req("POST", "/ui/feed/add", tok, form)); w.Code != 500 {
		t.Fatalf("save=%d", w.Code)
	}
}

func TestFeedRemove(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	form := url.Values{"url": {"https://x/feed"}}
	if w := do(mux, req("POST", "/ui/feed/remove", tok, form)); w.Code != 303 {
		t.Fatalf("code=%d", w.Code)
	}
	if len(op.op.Feeds) != 0 {
		t.Fatalf("not removed: %+v", op.op.Feeds)
	}
}

func TestFeedRemoveErrors(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	if w := do(mux, req("GET", "/ui/feed/remove", tok, nil)); w.Code != 405 {
		t.Fatalf("method=%d", w.Code)
	}
	if w := do(mux, req("POST", "/ui/feed/remove", tok, url.Values{})); w.Code != 400 {
		t.Fatalf("missing=%d", w.Code)
	}
	op.loadErr = errBoom
	form := url.Values{"url": {"x"}}
	if w := do(mux, req("POST", "/ui/feed/remove", tok, form)); w.Code != 500 {
		t.Fatalf("load=%d", w.Code)
	}
	op.loadErr = nil
	op.saveErr = errBoom
	if w := do(mux, req("POST", "/ui/feed/remove", tok, form)); w.Code != 500 {
		t.Fatalf("save=%d", w.Code)
	}
}

func TestSetReadDetailViewSummaryFallback(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := "https://nc.example/feed"
	op.op.Feeds = []store.Feed{{XMLURL: u, Title: "X"}}
	now := time.Now().UTC()
	st.AppendEntries(store.FeedHash(u), []store.Entry{
		{GUID: "g", Link: "https://nc/x", Title: "T", Summary: "summ-only", Published: now, FetchedAt: now},
	})
	es, _ := st.ListEntries(store.FeedHash(u))
	w := do(mux, req("POST", "/ui/entry/read?id="+es[0].Hash+"&state=1&view=detail", tok, nil))
	if !strings.Contains(w.Body.String(), "summ-only") {
		t.Fatalf("missing summary: %s", w.Body.String())
	}
}

func TestFeedAddParseFormErr(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	// A POST with a malformed Content-Type: application/x-www-form-urlencoded
	// but unparseable body triggers ParseForm error.
	r := httptest.NewRequest("POST", "/ui/feed/add", strings.NewReader("%ZZ"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHomeShowsAddLinkNoInlineRemove(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	w := do(mux, req("GET", "/ui/", tok, nil))
	body := w.Body.String()
	// home should *link* to the add-feed page, not carry an inline form.
	// Link is relative (no leading slash) so the UI works under any
	// deployment prefix.
	if !strings.Contains(body, `href="feed/new"`) {
		t.Fatalf("missing add-feed link: %s", body)
	}
	// home should NOT carry the unsubscribe form anymore — that moved
	// onto the per-feed page so it isn't a single click from a glance.
	if strings.Contains(body, "feed/remove") {
		t.Fatalf("remove form should not be on /ui/: %s", body)
	}
}

func TestFeedPageShowsUnsubscribe(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	w := do(mux, req("GET", "/ui/feed?id=https%3A%2F%2Fx%2Ffeed", tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, "feed/remove") || !strings.Contains(body, "unsubscribe") {
		t.Fatalf("feed page should carry unsubscribe form: %s", body)
	}
	// Cross-feed views (all / starred) must not show it.
	w = do(mux, req("GET", "/ui/all", tok, nil))
	if strings.Contains(w.Body.String(), "unsubscribe") {
		t.Fatalf("all-view should not show unsubscribe")
	}
}

func TestFeedRemoveParseFormErr(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	r := httptest.NewRequest("POST", "/ui/feed/remove", strings.NewReader("%ZZ"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestStaticJSContentType(t *testing.T) {
	_, mux, _, _, _, _ := fixture(t)
	w := do(mux, req("GET", "/ui/static/htmx.min.js", "", nil))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("js: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	w = do(mux, req("GET", "/ui/static/keys.js", "", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "harb keyboard nav") {
		t.Fatalf("keys.js: %d %s", w.Code, w.Body.String())
	}
}

func TestHomeWithEntries(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 3)
	es, _ := st.ListEntries(store.FeedHash(u))
	st.SetRead(es[0].Hash, true) // 1 read, 2 unread
	w := do(mux, req("GET", "/ui/", tok, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `id="count-total" class="count-inline">2</span> unread`) {
		t.Fatalf("home: %d %s", w.Code, w.Body.String())
	}
}

func TestEntrySummaryFallback(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := "https://nocontent.example/feed"
	op.op.Feeds = []store.Feed{{XMLURL: u, Title: "X"}}
	now := time.Now().UTC()
	st.AppendEntries(store.FeedHash(u), []store.Entry{
		{GUID: "g", Link: "https://x", Title: "T", Summary: "summary-only", Published: now, FetchedAt: now},
	})
	es, _ := st.ListEntries(store.FeedHash(u))
	w := do(mux, req("GET", "/ui/entry?id="+es[0].Hash, tok, nil))
	if !strings.Contains(w.Body.String(), "summary-only") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestLoadTemplatesParseFSError(t *testing.T) {
	orig := pageExtra
	defer func() { pageExtra = orig }()
	pageExtra = func(string) []string { return []string{"templates/no-such.html"} }
	dir := t.TempDir()
	st, _ := store.Open(t.TempDir())
	as, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"), auth.Config{})
	if _, err := New(st, as, &memOPML{}, "", ""); err == nil {
		t.Fatal("expected parse err")
	}
}

func TestRenderExecuteError(t *testing.T) {
	srv, _, _, _, _, _ := fixture(t)
	// Inject a broken template into pages so ExecuteTemplate returns an
	// error. Reusing the page name lets render() reach it.
	bad, _ := template.New("broken").Parse(`{{define "broken"}}{{.MissingField.SubField}}{{end}}`)
	srv.pages["broken"] = bad
	w := httptest.NewRecorder()
	srv.render(w, "broken", struct{}{})
	if w.Code != 500 {
		t.Fatalf("code=%d", w.Code)
	}
}

// ---- /ui/settings + /ui/settings/passwd -------------------------------

// settingsFixture wires up an on-disk config.json so the password-change
// flow has something to read and write. Returns the server, mux, auth
// store, session token, and the config path.
func settingsFixture(t *testing.T) (*Server, *http.ServeMux, *auth.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := config.Default()
	cfg.Auth.Username = "u"
	cfg.Auth.PasswordHash = testPwHash
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	as, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"), cfg.Auth)
	srv, err := New(st, as, &memOPML{}, "light", dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.ConfigPath = cfgPath
	srv.StaticVer = "abc123"
	mux := http.NewServeMux()
	srv.Routes(mux)
	tok, _ := as.IssueSession("u", "p")
	return srv, mux, as, tok, cfgPath
}

func TestSettingsGet(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	w := do(mux, req("GET", "/ui/settings", tok, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "change password") {
		t.Fatalf("settings GET: %d %s", w.Code, w.Body.String())
	}
	// Cache-busting suffix appears.
	if !strings.Contains(w.Body.String(), "style.css?v=abc123") {
		t.Fatalf("missing static-ver bust: %s", w.Body.String())
	}
}

func TestSettingsGetOkBanner(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	w := do(mux, req("GET", "/ui/settings?ok=1", tok, nil))
	if !strings.Contains(w.Body.String(), "password updated") {
		t.Fatalf("missing ok banner: %s", w.Body.String())
	}
}

func TestSettingsMethodNotAllowed(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	r := httptest.NewRequest("PUT", "/ui/settings", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	w := do(mux, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestSettingsDisabledWhenNoConfigPath(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t) // fixture leaves ConfigPath == ""
	w := do(mux, req("GET", "/ui/settings", tok, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	w = do(mux, req("POST", "/ui/settings/passwd", tok, url.Values{"old": {"p"}, "new": {"newpass1"}, "confirm": {"newpass1"}}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("passwd expected 404, got %d", w.Code)
	}
}

func TestPasswdSuccess(t *testing.T) {
	_, mux, as, tok, cfgPath := settingsFixture(t)
	form := url.Values{"old": {"p"}, "new": {"newsecret"}, "confirm": {"newsecret"}}
	w := do(mux, req("POST", "/ui/settings/passwd", tok, form))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "../login?passwd=1" {
		t.Fatalf("redirect=%q", loc)
	}
	// Old password no longer verifies.
	if err := as.Verify("u", "p"); err == nil {
		t.Fatal("old password still verifies")
	}
	if err := as.Verify("u", "newsecret"); err != nil {
		t.Fatalf("new password should verify: %v", err)
	}
	// Persisted to disk.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.PasswordHash == testPwHash {
		t.Fatal("config.json was not rewritten")
	}
	// Existing session revoked.
	if as.CheckSession(tok) {
		t.Fatal("session should be revoked")
	}
}

func TestPasswdLoginShowsBanner(t *testing.T) {
	_, mux, _, _, _ := settingsFixture(t)
	w := do(mux, req("GET", "/ui/login?passwd=1", "", nil))
	if !strings.Contains(w.Body.String(), "password changed") {
		t.Fatalf("missing banner: %s", w.Body.String())
	}
}

func TestPasswdMethodNotAllowed(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	w := do(mux, req("GET", "/ui/settings/passwd", tok, nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestPasswdMismatch(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	form := url.Values{"old": {"p"}, "new": {"aaaaaaaa"}, "confirm": {"bbbbbbbb"}}
	w := do(mux, req("POST", "/ui/settings/passwd", tok, form))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "do not match") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPasswdTooShort(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	form := url.Values{"old": {"p"}, "new": {"short"}, "confirm": {"short"}}
	w := do(mux, req("POST", "/ui/settings/passwd", tok, form))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "at least 8") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPasswdWrongOld(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	form := url.Values{"old": {"WRONG"}, "new": {"newsecret"}, "confirm": {"newsecret"}}
	w := do(mux, req("POST", "/ui/settings/passwd", tok, form))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "current password") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPasswdParseFormError(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	r := httptest.NewRequest("POST", "/ui/settings/passwd", strings.NewReader("%%"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	w := do(mux, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad form") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPasswdSaveError(t *testing.T) {
	srv, mux, _, tok, cfgPath := settingsFixture(t)
	// Lock down the directory so Load (read) still works but Save
	// (atomic write of a tempfile in the same dir) cannot.
	dir := filepath.Dir(cfgPath)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_ = srv // ConfigPath is left pointing at the real cfgPath
	form := url.Values{"old": {"p"}, "new": {"newsecret"}, "confirm": {"newsecret"}}
	w := do(mux, req("POST", "/ui/settings/passwd", tok, form))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "save config") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPasswdLoadError(t *testing.T) {
	srv, mux, _, tok, _ := settingsFixture(t)
	// Point at a path containing junk JSON so config.Load errors.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.ConfigPath = bad
	form := url.Values{"old": {"p"}, "new": {"newsecret"}, "confirm": {"newsecret"}}
	w := do(mux, req("POST", "/ui/settings/passwd", tok, form))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "load config") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// HashPassword unconditionally returns "hash" + nil under normal stdlib.
// To cover the hash-error branch of handlePasswd, monkey-patch it for
// the duration of this test.
func TestPasswdHashError(t *testing.T) {
	_, mux, _, tok, _ := settingsFixture(t)
	orig := authHashPasswordHook
	t.Cleanup(func() { authHashPasswordHook = orig })
	authHashPasswordHook = func(string) (string, error) { return "", errBoom }
	form := url.Values{"old": {"p"}, "new": {"newsecret"}, "confirm": {"newsecret"}}
	w := do(mux, req("POST", "/ui/settings/passwd", tok, form))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "hash:") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// ---- handleStatic cache-busting ---------------------------------------

func TestStaticCacheControl(t *testing.T) {
	_, mux, _, _, _, _ := fixture(t)
	w := do(mux, req("GET", "/ui/static/style.css?v=abc", "", nil))
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("immutable cache: %q", cc)
	}
	w = do(mux, req("GET", "/ui/static/style.css", "", nil))
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("revalidate cache: %q", cc)
	}
}

// ---- /ui/feed/new -----------------------------------------------------

type stubPreviewer struct {
	called string
	out    *FeedPreview
	err    error
}

func (s *stubPreviewer) Preview(u string) (*FeedPreview, error) {
	s.called = u
	return s.out, s.err
}

func TestFeedNewGet(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	w := do(mux, req("GET", "/ui/feed/new", tok, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "feed URL") {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
}

func TestFeedNewPostPreview(t *testing.T) {
	srv, mux, _, _, tok, _ := fixture(t)
	srv.Previewer = &stubPreviewer{out: &FeedPreview{
		Title:       "Example",
		Description: "an example feed",
		Link:        "https://example.com",
		Items:       []FeedPreviewItem{{Title: "Hello"}, {Title: "World"}},
	}}
	w := do(mux, req("POST", "/ui/feed/new", tok, url.Values{"url": {"https://example.com/feed.xml"}}))
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	b := w.Body.String()
	for _, want := range []string{"Example", "an example feed", "Hello", "World", "subscribe"} {
		if !strings.Contains(b, want) {
			t.Fatalf("missing %q in: %s", want, b)
		}
	}
}

func TestFeedNewPostMissingURL(t *testing.T) {
	srv, mux, _, _, tok, _ := fixture(t)
	srv.Previewer = &stubPreviewer{}
	w := do(mux, req("POST", "/ui/feed/new", tok, url.Values{}))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "feed URL is required") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFeedNewPostNoPreviewer(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t) // fixture leaves Previewer nil
	w := do(mux, req("POST", "/ui/feed/new", tok, url.Values{"url": {"https://x/"}}))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestFeedNewPostPreviewError(t *testing.T) {
	srv, mux, _, _, tok, _ := fixture(t)
	srv.Previewer = &stubPreviewer{err: berr("nope")}
	w := do(mux, req("POST", "/ui/feed/new", tok, url.Values{"url": {"https://x/"}}))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "could not fetch feed") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFeedNewPostParseFormError(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	r := httptest.NewRequest("POST", "/ui/feed/new", strings.NewReader("%%"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	w := do(mux, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestFeedNewMethodNotAllowed(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	r := httptest.NewRequest("PUT", "/ui/feed/new", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	w := do(mux, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d", w.Code)
	}
}

// TestFeedNewHasTagDatalist confirms the add-feed page exposes a
// <datalist> of every known tag, wired to the comma-separated tags
// input, on both the GET form and the POST preview render path — so the
// tags field can autocomplete existing tags consistently with the edit
// view.
func TestFeedNewHasTagDatalist(t *testing.T) {
	srv, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://a/feed", Title: "Alpha", Tags: []string{"tech", "weekly"}},
		{XMLURL: "https://b/feed", Title: "Bravo", Tags: []string{"news"}},
	}
	datalist := []string{
		`<datalist id="newfeed-all-tags">`,
		`<option value="tech">`,
		`<option value="weekly">`,
		`<option value="news">`,
	}
	inputWiring := []string{
		`list="newfeed-all-tags"`,
		`data-tag-autocomplete`,
	}

	// GET: the empty form carries the datalist (always rendered, so the
	// tags input in the preview form can reference it).
	w := do(mux, req("GET", "/ui/feed/new", tok, nil))
	body := w.Body.String()
	for _, s := range datalist {
		if !strings.Contains(body, s) {
			t.Fatalf("GET add-feed missing %q: %s", s, body)
		}
	}

	// POST (preview render path): the datalist survives AND the tags
	// input is wired to it with the token-aware enhancement hook.
	srv.Previewer = &stubPreviewer{out: &FeedPreview{Title: "Example", Link: "https://example.com"}}
	w = do(mux, req("POST", "/ui/feed/new", tok, url.Values{"url": {"https://example.com/feed.xml"}}))
	body = w.Body.String()
	for _, s := range append(append([]string{}, datalist...), inputWiring...) {
		if !strings.Contains(body, s) {
			t.Fatalf("POST add-feed missing %q: %s", s, body)
		}
	}
}

// ---- formatPublished --------------------------------------------------

func TestFormatPublished(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct{ in, want string }{
		{"", ""}, // zero
		{"2026-06-15T11:59:50Z", "now"},
		{"2026-06-15T11:30:00Z", "30m"},
		{"2026-06-15T08:00:00Z", "4h"},
		{"2026-06-13T12:00:00Z", "2d"},
		{"2026-05-15T12:00:00Z", "May 15"}, // same year, >7d
		{"2025-06-15T12:00:00Z", "2025-06-15"},
	}
	for _, c := range cases {
		var got string
		if c.in == "" {
			got = formatPublished(time.Time{}, now)
		} else {
			tp, _ := time.Parse(time.RFC3339, c.in)
			got = formatPublished(tp, now)
		}
		if got != c.want {
			t.Errorf("formatPublished(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestHomeUnreadFilter(t *testing.T) {
	srv, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://x/empty", Title: "Empty"},
		{XMLURL: "https://x/hot", Title: "Hot"},
	}
	// Hot has an unread entry; Empty has nothing.
	now := time.Now()
	fh := store.FeedHash("https://x/hot")
	if _, err := st.AppendEntries(fh, []store.Entry{
		{GUID: "1", Title: "post", Published: now, FetchedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	_ = srv
	// Default view is "show unread only" → only Hot is visible. The
	// toggle pill is in its active state and points at ?unread=0.
	w := do(mux, req("GET", "/ui/", tok, nil))
	body := w.Body.String()
	if strings.Contains(body, "Empty") {
		t.Fatalf("Empty should be filtered out by default unread-only view: %s", body)
	}
	if !strings.Contains(body, "Hot") {
		t.Fatalf("Hot should be visible in default view: %s", body)
	}
	if !strings.Contains(body, "unread only (1) ×") {
		t.Fatalf("expected active filter pill by default: %s", body)
	}
	// Explicit "show all" via ?unread=0 reveals Empty too.
	w = do(mux, req("GET", "/ui/?unread=0", tok, nil))
	body = w.Body.String()
	if !strings.Contains(body, "Empty") || !strings.Contains(body, "Hot") {
		t.Fatalf("expected both feeds with unread=0: %s", body)
	}
	if !strings.Contains(body, "show unread only (1)") {
		t.Fatalf("expected toggle to say (1): %s", body)
	}
	// Explicit ?unread=1 also shows the active pill.
	w = do(mux, req("GET", "/ui/?unread=1", tok, nil))
	body = w.Body.String()
	if strings.Contains(body, "Empty") {
		t.Fatalf("Empty should be filtered out with unread=1: %s", body)
	}
	if !strings.Contains(body, "Hot") {
		t.Fatalf("Hot should still be there: %s", body)
	}
	if !strings.Contains(body, "unread only (1) ×") {
		t.Fatalf("expected active filter pill: %s", body)
	}
}

func TestHomeUnreadFilterAllCaughtUp(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/empty", Title: "Empty"}}
	w := do(mux, req("GET", "/ui/?unread=1", tok, nil))
	if !strings.Contains(w.Body.String(), "all caught up") {
		t.Fatalf("expected caught-up empty state: %s", w.Body.String())
	}
}

// saveFeedErr seeds a per-feed state file marking the feed as failing.
func saveFeedErr(t *testing.T, st *store.Store, url string, count int, lastErr string, lastSuccess time.Time) {
	t.Helper()
	fh := store.FeedHash(url)
	if err := st.SaveFeedState(fh, store.FeedState{
		URL:         url,
		ErrorCount:  count,
		LastError:   lastErr,
		LastSuccess: lastSuccess,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHomeSyncFailureBanner covers the failing-feed surfacing on the
// home page: the top-of-page banner counts failing feeds, the per-row
// ⚠ badge carries the error detail in its tooltip, and a failing feed
// with zero unread still appears in the banner even under the default
// unread-only filter.
func TestHomeSyncFailureBanner(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://x/broken", Title: "Broken"},
		{XMLURL: "https://x/healthy", Title: "Healthy"},
	}
	// Broken feed: 3 consecutive errors, never succeeded, 0 unread.
	saveFeedErr(t, st, "https://x/broken", 3, "dial tcp: connection refused", time.Time{})

	// Default view is unread-only; Broken has no unread entries but must
	// still surface in the banner.
	w := do(mux, req("GET", "/ui/", tok, nil))
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if !strings.Contains(body, "1 feed failing to sync") {
		t.Fatalf("expected singular failing banner: %s", body)
	}
	if !strings.Contains(body, "sync-fail") {
		t.Fatalf("expected sync-fail banner block: %s", body)
	}
	// Banner lists the failing feed. In the default unread-only view the
	// Broken row itself is hidden (0 unread), so "Broken" only appears
	// inside the banner.
	bannerStart := strings.Index(body, "sync-fail-list")
	if bannerStart < 0 {
		t.Fatalf("missing banner list: %s", body)
	}
	bannerEnd := strings.Index(body[bannerStart:], "</ul>") + bannerStart
	banner := body[bannerStart:bannerEnd]
	if !strings.Contains(banner, ">Broken<") {
		t.Fatalf("banner should link to broken feed: %s", banner)
	}
	// Healthy feed never appears in the failing banner list.
	if strings.Contains(banner, "Healthy") {
		t.Fatalf("Healthy should not be in failing banner: %s", banner)
	}

	// With unread=0 the Broken feed row is visible and carries the ⚠
	// badge with the error detail + "never" last-success in the tooltip.
	w = do(mux, req("GET", "/ui/?unread=0", tok, nil))
	body = w.Body.String()
	if !strings.Contains(body, "sync-warn") {
		t.Fatalf("expected per-row sync-warn badge: %s", body)
	}
	if !strings.Contains(body, "connection refused") {
		t.Fatalf("expected last error in tooltip: %s", body)
	}
	if !strings.Contains(body, "last succeeded never") {
		t.Fatalf("expected 'never' last-success: %s", body)
	}
	if !strings.Contains(body, "feed-failing") {
		t.Fatalf("expected failing row class: %s", body)
	}
}

// TestHomeSyncFailurePlural pins the plural wording and that a feed with
// a real last-success time renders the relative timestamp.
func TestHomeSyncFailurePlural(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://x/a", Title: "AAA"},
		{XMLURL: "https://x/b", Title: "BBB"},
	}
	saveFeedErr(t, st, "https://x/a", 1, "http 500", time.Now().Add(-2*time.Hour))
	saveFeedErr(t, st, "https://x/b", 5, "parse error", time.Time{})
	w := do(mux, req("GET", "/ui/?unread=0", tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, "2 feeds failing to sync") {
		t.Fatalf("expected plural failing banner: %s", body)
	}
	// Feed a succeeded ~2h ago → relative "2h" in its tooltip.
	if !strings.Contains(body, "last succeeded 2h") {
		t.Fatalf("expected relative last-success: %s", body)
	}
	// Singular vs plural error-count wording.
	if !strings.Contains(body, "1 consecutive error;") {
		t.Fatalf("expected singular error count for feed a: %s", body)
	}
	if !strings.Contains(body, "5 consecutive errors;") {
		t.Fatalf("expected plural error count for feed b: %s", body)
	}
}

// TestHomeSyncFailureTagScoped checks the banner respects the active tag
// filter: a failing feed outside the filtered tag is not counted.
func TestHomeSyncFailureTagScoped(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://x/news", Title: "News", Tags: []string{"news"}},
		{XMLURL: "https://x/tech", Title: "Tech", Tags: []string{"tech"}},
	}
	saveFeedErr(t, st, "https://x/news", 2, "boom", time.Time{})
	saveFeedErr(t, st, "https://x/tech", 4, "kaboom", time.Time{})
	// Filter to tech: only the tech feed's failure counts.
	w := do(mux, req("GET", "/ui/?unread=0&tag=tech", tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, "1 feed failing to sync") {
		t.Fatalf("expected scoped failing count of 1: %s", body)
	}
	if strings.Contains(body, "News") {
		t.Fatalf("news feed should be out of scope under tag=tech: %s", body)
	}
}

// TestHomeNoFailureBanner confirms a healthy fleet renders no banner.
func TestHomeNoFailureBanner(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/ok", Title: "OK"}}
	w := do(mux, req("GET", "/ui/?unread=0", tok, nil))
	if strings.Contains(w.Body.String(), "sync-fail") {
		t.Fatalf("healthy fleet should render no failure banner: %s", w.Body.String())
	}
}

// TestHomeFeedStatesError drives the LoadFeedState error branch in
// handleHome by corrupting a feed's state file (valid entries, bad
// state JSON).
func TestHomeFeedStatesError(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	stateDir := filepath.Join(st.Dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fh := store.FeedHash("https://x/feed")
	if err := os.WriteFile(filepath.Join(stateDir, fh+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := do(mux, req("GET", "/ui/?unread=0", tok, nil))
	if w.Code != 500 {
		t.Fatalf("expected 500 on bad state JSON, got %d", w.Code)
	}
}

// TestFormatLastSuccess pins the small wrapper: zero → "never", else the
// relative formatter.
func TestFormatLastSuccess(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if got := formatLastSuccess(time.Time{}, now); got != "never" {
		t.Fatalf("zero=%q want never", got)
	}
	if got := formatLastSuccess(now.Add(-3*time.Hour), now); got != "3h" {
		t.Fatalf("3h=%q", got)
	}
}

// ---- relative-URL invariants -----------------------------------------
//
// These tests pin down the deployment-prefix-agnostic property: every
// URL the UI emits — Location headers, href/action attributes,
// hx-post/hx-get attributes, <link> / <script> src — is a relative
// reference (no leading slash). Browsers resolve relative references
// against the effective request URI (RFC 7231 §7.1.2, RFC 3986 §5),
// so the same handler set works whether mounted at "/" locally or
// under "/rss/" behind tailscale funnel --set-path=/rss.

// absURLRe matches src=, href= or action= attribute values that start
// with "/", and hx-post=/hx-get= forms too. The negative lookahead is
// not available in RE2; we instead allow scheme-prefixed absolute URLs
// (https://…) by inspecting the matched value below.
var absURLRe = regexp.MustCompile(`(?i)\b(?:src|href|action|hx-post|hx-get|hx-put|hx-delete)="(/[^"]*)"`)

func assertNoAbsoluteURLs(t *testing.T, where, body string) {
	t.Helper()
	for _, m := range absURLRe.FindAllStringSubmatch(body, -1) {
		// We allow nothing leading-slash. External absolute URLs
		// (http://, https://, mailto:) won't match this regex at all.
		t.Errorf("%s: absolute-path URL leaked into HTML: %q", where, m[1])
	}
}

// TestNoAbsolutePathsInRenderedHTML walks every page the UI renders
// and asserts none of them contain leading-slash href/action/src
// attributes. This is the regression check for the relative-URL
// invariant.
func TestNoAbsolutePathsInRenderedHTML(t *testing.T) {
	srv, mux, st, op, tok, _ := fixture(t)
	srv.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	u := seed(t, st, op, 2)
	es, _ := st.ListEntries(store.FeedHash(u))
	// Mark the feed as failing so the home page renders the sync-failure
	// banner — its feed link must also be a relative reference.
	saveFeedErr(t, st, u, 2, "boom", time.Time{})

	for _, p := range []string{
		"/ui/login",
		"/ui/",
		"/ui/?unread=1",
		"/ui/all",
		"/ui/starred",
		"/ui/feed?id=" + u,
		"/ui/feed?id=" + u + "&unread=1",
		"/ui/entry?id=" + es[0].Hash,
		"/ui/feed/new",
		"/ui/settings",
	} {
		w := do(mux, req("GET", p, tok, nil))
		if w.Code != 200 {
			t.Fatalf("%s code=%d", p, w.Code)
		}
		assertNoAbsoluteURLs(t, p, w.Body.String())
	}
}

// TestNoAbsolutePathsInLocationHeaders covers every UI handler that
// issues a 3xx — each one must emit a relative reference so the
// browser resolves it against its current URL (which may live under a
// deployment prefix).
func TestNoAbsolutePathsInLocationHeaders(t *testing.T) {
	srv, mux, st, op, tok, _ := fixture(t)
	// Wire up settings so /ui/settings/passwd is reachable.
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(cfgPath, config.Config{Auth: auth.Config{Username: "u", PasswordHash: testPwHash}}); err != nil {
		t.Fatal(err)
	}
	srv.ConfigPath = cfgPath
	u := seed(t, st, op, 1)

	cases := []struct {
		name, method, path string
		form               url.Values
		noAuth             bool
	}{
		{"requireSession-from-root", "GET", "/ui/", nil, true},
		{"requireSession-from-deep", "GET", "/ui/feed/new", nil, true},
		{"login-success", "POST", "/ui/login", url.Values{"username": {"u"}, "password": {"p"}}, true},
		{"logout", "POST", "/ui/logout", nil, false},
		{"feed-add", "POST", "/ui/feed/add", url.Values{"url": {"https://new.example/feed"}}, false},
		{"feed-remove", "POST", "/ui/feed/remove", url.Values{"url": {u}}, false},
		{"mark-all-read-feed", "POST", "/ui/mark-all-read?scope=feed&id=" + u, nil, false},
		{"mark-all-read-all", "POST", "/ui/mark-all-read?scope=all", nil, false},
		{"passwd-success", "POST", "/ui/settings/passwd", url.Values{"old": {"p"}, "new": {"newsecret"}, "confirm": {"newsecret"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useTok := tok
			if c.noAuth {
				useTok = ""
			}
			w := do(mux, req(c.method, c.path, useTok, c.form))
			if w.Code/100 != 3 {
				t.Fatalf("expected 3xx, got %d body=%s", w.Code, w.Body.String())
			}
			loc := w.Header().Get("Location")
			if loc == "" {
				t.Fatalf("empty Location")
			}
			if strings.HasPrefix(loc, "/") {
				t.Fatalf("absolute-path Location leaked: %q", loc)
			}
			if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
				t.Fatalf("absolute-URL Location leaked: %q", loc)
			}
		})
	}
}

// TestUIWorksUnderPrefix mounts the UI mux behind http.StripPrefix and
// walks the redirect chain manually, asserting at each hop that the
// (request-URI, Location) pair combine via net/url.ResolveReference
// into the expected next URI. This is the end-to-end proof that the
// app can be served under any path prefix without baking it into
// config.
func TestUIWorksUnderPrefix(t *testing.T) {
	_, mux, _, _, _, _ := fixture(t)
	const prefix = "/rss"
	root := http.NewServeMux()
	root.Handle(prefix+"/", http.StripPrefix(prefix, mux))
	srv := httptest.NewServer(root)
	defer srv.Close()

	// Don't auto-follow — we want to inspect each Location.
	cli := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resolve := func(t *testing.T, fromURI, loc string) *url.URL {
		t.Helper()
		base, err := url.Parse(fromURI)
		if err != nil {
			t.Fatalf("parse base %q: %v", fromURI, err)
		}
		ref, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("parse loc %q: %v", loc, err)
		}
		return base.ResolveReference(ref)
	}

	// Hop 1: GET /rss/ui/ → 303 → relative Location → /rss/ui/login
	got, err := cli.Get(srv.URL + "/rss/ui/")
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != http.StatusSeeOther {
		t.Fatalf("hop1 code=%d", got.StatusCode)
	}
	loc1 := got.Header.Get("Location")
	if loc1 == "" || strings.HasPrefix(loc1, "/") {
		t.Fatalf("hop1 expected relative Location, got %q", loc1)
	}
	next := resolve(t, srv.URL+"/rss/ui/", loc1)
	if next.Path != "/rss/ui/login" {
		t.Fatalf("hop1 resolved to %q, want /rss/ui/login", next.Path)
	}

	// Hop 2: GET /rss/ui/login → 200, body must reference no
	// absolute paths and must include a form whose action resolves
	// back to /rss/ui/login (so POSTing the form lands on the right
	// handler under the prefix).
	got, err = cli.Get(next.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != 200 {
		t.Fatalf("hop2 code=%d", got.StatusCode)
	}
	body, _ := readAll(got.Body)
	got.Body.Close()
	assertNoAbsoluteURLs(t, "/rss/ui/login", body)
	// Pull the login form action and verify it resolves to /rss/ui/login.
	formRe := regexp.MustCompile(`(?is)<form[^>]+action="([^"]+)"[^>]*>\s*<label>username`)
	m := formRe.FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatalf("login form not found in body:\n%s", body)
	}
	action := resolve(t, next.String(), m[1])
	if action.Path != "/rss/ui/login" {
		t.Fatalf("login form action resolves to %q, want /rss/ui/login", action.Path)
	}

	// Also: static asset URLs in the rendered page must resolve under
	// the /rss prefix too. Grab the stylesheet href and confirm.
	cssRe := regexp.MustCompile(`<link[^>]+rel="stylesheet"[^>]+href="([^"]+)"`)
	if cm := cssRe.FindStringSubmatch(body); len(cm) >= 2 {
		css := resolve(t, next.String(), cm[1])
		if !strings.HasPrefix(css.Path, "/rss/ui/static/") {
			t.Fatalf("style.css resolves to %q, want /rss/ui/static/ prefix", css.Path)
		}
		// And the asset itself must be reachable through the prefix.
		r, err := cli.Get(css.String())
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 200 {
			t.Fatalf("style.css through prefix: code=%d", r.StatusCode)
		}
		r.Body.Close()
	} else {
		t.Fatalf("stylesheet link not found in login page")
	}

	// Hop 3: POST /rss/ui/login (good creds) → 303 → relative Location → /rss/ui/
	form := url.Values{"username": {"u"}, "password": {"p"}}
	got, err = cli.PostForm(srv.URL+"/rss/ui/login", form)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != http.StatusSeeOther {
		t.Fatalf("hop3 code=%d body=%s", got.StatusCode, mustReadString(got.Body))
	}
	loc3 := got.Header.Get("Location")
	if loc3 == "" || strings.HasPrefix(loc3, "/") {
		t.Fatalf("hop3 expected relative Location, got %q", loc3)
	}
	if got := resolve(t, srv.URL+"/rss/ui/login", loc3).Path; got != "/rss/ui/" {
		t.Fatalf("hop3 resolves to %q, want /rss/ui/", got)
	}

	// Hop 4: GET /rss/ui/ with the new session cookie → 200, body still relative-only.
	cookie := got.Cookies()
	r4, _ := http.NewRequest("GET", srv.URL+"/rss/ui/", nil)
	for _, c := range cookie {
		r4.AddCookie(c)
	}
	got, err = cli.Do(r4)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != 200 {
		t.Fatalf("hop4 code=%d", got.StatusCode)
	}
	body4, _ := readAll(got.Body)
	got.Body.Close()
	assertNoAbsoluteURLs(t, "/rss/ui/", body4)
}

// readAll / mustReadString are tiny stdlib wrappers kept local so the
// import block stays narrow.
func readAll(r interface {
	Read(p []byte) (int, error)
}) (string, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return string(buf), nil
			}
			return string(buf), err
		}
	}
}

func mustReadString(r interface {
	Read(p []byte) (int, error)
}) string {
	s, _ := readAll(r)
	return s
}

// TestRelRedirectAbsolutePathPanics pins down the defence-in-depth
// panic in RelRedirect: callers must pass relative references. A
// leading-slash Location would re-introduce the absolute-path bug
// this package is built around avoiding, so we panic loud instead of
// silently emitting the wrong thing.
func TestRelRedirectAbsolutePathPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for absolute-path Location")
		}
	}()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ui/", nil)
	RelRedirect(w, r, "/ui/login", http.StatusSeeOther)
}

// TestUIWorksUnderPrefixFullRoundTrip is the browser-equivalent test
// the original /ui/ → 404-under-funnel bug would have caught: it spins
// up an httptest server with the UI mux mounted behind
// `http.StripPrefix("/rss", mux)` and the same root-catchall main.go
// wires, then drives a real `http.Client` with a cookie jar and
// auto-follow redirects through GET /rss/ → ... → /rss/ui/login →
// POST creds → /rss/ui/. If any URL the app emits escapes the /rss
// prefix, the client lands somewhere other than where this test
// expects and the assertion fires.
func TestUIWorksUnderPrefixFullRoundTrip(t *testing.T) {
	_, mux, st, op, _, _ := fixture(t)
	// Seed a feed with one unread entry — the default home view is
	// "show unread only", which would hide a feed with no entries.
	op.op.Feeds = []store.Feed{{XMLURL: "https://demo.example/feed", Title: "Demo"}}
	{
		fh := store.FeedHash("https://demo.example/feed")
		now := time.Now()
		if _, err := st.AppendEntries(fh, []store.Entry{
			{GUID: "g1", Title: "T", Published: now, FetchedAt: now},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Mirror cmd/harb/main.go: GET / under the UI mux redirects
	// to a relative "ui/" so the front-door redirect also rides any
	// external prefix.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			RelRedirect(w, r, "ui/", http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
	})

	const prefix = "/rss"
	root := http.NewServeMux()
	root.Handle(prefix+"/", http.StripPrefix(prefix, mux))
	ts := httptest.NewServer(root)
	defer ts.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	cli := &http.Client{Jar: jar} // default CheckRedirect → auto-follow.

	// Hop chain: GET /rss/ → 303 ui/ → /rss/ui/ → 303 login → /rss/ui/login
	// → 200 (login page).
	resp, err := cli.Get(ts.URL + prefix + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login page code=%d landed=%s body=%s", resp.StatusCode, resp.Request.URL.Path, body)
	}
	if resp.Request.URL.Path != "/rss/ui/login" {
		t.Fatalf("expected to land at /rss/ui/login, got %q", resp.Request.URL.Path)
	}
	if !strings.Contains(body, "sign in") {
		t.Fatalf("login page missing 'sign in' heading: %s", body)
	}
	// Form action is relative ('login'), so POSTing to the same URL
	// (the page the form is on) is what a real browser does.
	creds := url.Values{"username": {"u"}, "password": {"p"}}
	resp, err = cli.PostForm(resp.Request.URL.String(), creds)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = readAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("post-login code=%d landed=%s body=%s", resp.StatusCode, resp.Request.URL.Path, body)
	}
	if resp.Request.URL.Path != "/rss/ui/" {
		t.Fatalf("expected to land at /rss/ui/ after login, got %q", resp.Request.URL.Path)
	}
	// Home page must render the seeded feed and have no absolute URLs.
	if !strings.Contains(body, "Demo") {
		t.Fatalf("home page missing seeded feed: %s", body)
	}
	assertNoAbsoluteURLs(t, "/rss/ui/ (after login)", body)
}

func TestParseTagInput(t *testing.T) {
	if got := parseTagInput(""); got != nil {
		t.Fatalf("empty → %v", got)
	}
	got := parseTagInput("  c , a, b , , a ")
	want := []string{"a", "b", "c"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %v", got)
	}
}

func TestHomeSidebarAndTagFilter(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://a/feed", Title: "A", Tags: []string{"tech"}},
		{XMLURL: "https://b/feed", Title: "B", Tags: []string{"tech", "daily"}},
		{XMLURL: "https://c/feed", Title: "C"},
	}
	// Seed one unread entry per feed.
	for _, f := range op.op.Feeds {
		fh := store.FeedHash(f.XMLURL)
		st.AppendEntries(fh, []store.Entry{{GUID: "g-" + f.XMLURL, Link: f.XMLURL + "/1", Title: "T", Published: time.Now(), FetchedAt: time.Now()}})
	}
	// Default home → sidebar shows All / Untagged / tags.
	w := do(mux, req("GET", "/ui/", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"All", "Untagged", "tech", "daily", "A", "B", "C"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	// tag filter
	w = do(mux, req("GET", "/ui/?tag=tech", tok, nil))
	body = w.Body.String()
	if !strings.Contains(body, "A") || !strings.Contains(body, "B") || strings.Contains(body, ">C<") {
		t.Fatalf("tech filter body=%s", body)
	}
	// untagged filter → only C
	w = do(mux, req("GET", "/ui/?tag=__untagged__", tok, nil))
	body = w.Body.String()
	if !strings.Contains(body, ">C<") {
		t.Fatalf("untagged missing C: %s", body)
	}
	// unread-only + tag combo, all entries are unread so result is same.
	w = do(mux, req("GET", "/ui/?unread=1&tag=tech", tok, nil))
	if w.Code != 200 {
		t.Fatalf("unread+tag=%d", w.Code)
	}
	// Now mark every entry read so unread-filter empties C bucket.
	for _, f := range op.op.Feeds {
		fh := store.FeedHash(f.XMLURL)
		es, _ := st.ListEntries(fh)
		for _, e := range es {
			st.SetRead(e.Hash, true)
		}
	}
	// empty unread under tag filter → "all caught up — show all feeds"
	w = do(mux, req("GET", "/ui/?unread=1&tag=tech", tok, nil))
	if !strings.Contains(w.Body.String(), "all caught up") {
		t.Fatalf("expected caught-up empty state: %s", w.Body.String())
	}
	// empty tag filter (no matches) when tag missing. With
	// unread-only on, the "all caught up" branch wins, so test the
	// real no-match case with ?unread=0.
	w = do(mux, req("GET", "/ui/?unread=0&tag=zzz", tok, nil))
	if !strings.Contains(w.Body.String(), "no feeds in this view") {
		t.Fatalf("expected empty-tag-view: %s", w.Body.String())
	}
}

func TestHomeSidebarNoRealTags(t *testing.T) {
	// When no feed carries any tag, the sidebar must still render the
	// two pinned rows but skip the separator + real-tag block.
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	w := do(mux, req("GET", "/ui/", tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, "All") || !strings.Contains(body, "Untagged") {
		t.Fatalf("pinned rows missing: %s", body)
	}
	if strings.Contains(body, `class="sep"`) {
		t.Fatalf("stray separator with no real tags: %s", body)
	}
}

func TestHomeBadgesAreScopeAware(t *testing.T) {
	// With a tag filter active, "feeds (N unread)" and the
	// "show unread only (N)" pill must reflect the filtered scope,
	// not the global totals — otherwise the numbers confuse users.
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://a/feed", Title: "A", Tags: []string{"tech"}},
		{XMLURL: "https://b/feed", Title: "B"}, // untagged
	}
	for _, f := range op.op.Feeds {
		fh := store.FeedHash(f.XMLURL)
		st.AppendEntries(fh, []store.Entry{{GUID: "g-" + f.XMLURL, Link: f.XMLURL + "/1", Title: "T", Published: time.Now(), FetchedAt: time.Now()}})
	}
	// Default view: 2 unread total.
	w := do(mux, req("GET", "/ui/", tok, nil))
	if !strings.Contains(w.Body.String(), `id="count-total" class="count-inline">2</span> unread`) {
		t.Fatalf("global total wrong: %s", w.Body.String())
	}
	// Filter to ?tag=tech: only A is in scope → 1 unread.
	w = do(mux, req("GET", "/ui/?tag=tech", tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, `id="count-total" class="count-inline">1</span> unread`) {
		t.Fatalf("scoped total wrong: %s", body)
	}
	if !strings.Contains(body, "unread only (1)") {
		t.Fatalf("scoped pill wrong: %s", body)
	}
}

func TestFeedTagChip(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	// Method check
	if w := do(mux, req("GET", "/ui/feed/tag", tok, nil)); w.Code != 405 {
		t.Fatalf("method=%d", w.Code)
	}
	// Add tag
	form := url.Values{"url": {"https://x/feed"}, "add": {"news"}}
	w := do(mux, req("POST", "/ui/feed/tag", tok, form))
	if w.Code != 200 {
		t.Fatalf("add code=%d body=%s", w.Code, w.Body.String())
	}
	if !op.op.Feeds[0].HasTag("news") {
		t.Fatalf("not added: %v", op.op.Feeds[0].Tags)
	}
	if !strings.Contains(w.Body.String(), "news") {
		t.Fatalf("fragment missing tag: %s", w.Body.String())
	}
	// Remove tag
	form2 := url.Values{"url": {"https://x/feed"}, "remove": {"news"}}
	if w := do(mux, req("POST", "/ui/feed/tag", tok, form2)); w.Code != 200 {
		t.Fatalf("rem code=%d", w.Code)
	}
	if op.op.Feeds[0].HasTag("news") {
		t.Fatalf("not removed: %v", op.op.Feeds[0].Tags)
	}
	// Missing inputs
	if w := do(mux, req("POST", "/ui/feed/tag", tok, url.Values{"url": {"https://x/feed"}})); w.Code != 400 {
		t.Fatalf("missing add/rem=%d", w.Code)
	}
	// Unknown feed
	form3 := url.Values{"url": {"https://nope/feed"}, "add": {"y"}}
	if w := do(mux, req("POST", "/ui/feed/tag", tok, form3)); w.Code != 404 {
		t.Fatalf("nope=%d", w.Code)
	}
	// Load error
	op.loadErr = errBoom
	if w := do(mux, req("POST", "/ui/feed/tag", tok, form)); w.Code != 500 {
		t.Fatalf("load=%d", w.Code)
	}
	op.loadErr = nil
	op.saveErr = errBoom
	if w := do(mux, req("POST", "/ui/feed/tag", tok, form)); w.Code != 500 {
		t.Fatalf("save=%d", w.Code)
	}
}

func TestFeedRename(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "Old"}}
	// Method check
	if w := do(mux, req("GET", "/ui/feed/rename", tok, nil)); w.Code != 405 {
		t.Fatalf("method=%d", w.Code)
	}
	// Success: title is trimmed and persisted, redirect back to feed.
	form := url.Values{"url": {"https://x/feed"}, "title": {"  New Name  "}}
	w := do(mux, req("POST", "/ui/feed/rename", tok, form))
	if w.Code != 303 {
		t.Fatalf("rename code=%d body=%s", w.Code, w.Body.String())
	}
	if op.op.Feeds[0].Title != "New Name" {
		t.Fatalf("title not set: %q", op.op.Feeds[0].Title)
	}
	if loc := w.Header().Get("Location"); loc != "../feed?id="+url.QueryEscape("https://x/feed") {
		t.Fatalf("loc=%q", loc)
	}
	// Missing url
	if w := do(mux, req("POST", "/ui/feed/rename", tok, url.Values{"title": {"x"}})); w.Code != 400 {
		t.Fatalf("missing url=%d", w.Code)
	}
	// Empty (whitespace-only) title
	form2 := url.Values{"url": {"https://x/feed"}, "title": {"   "}}
	if w := do(mux, req("POST", "/ui/feed/rename", tok, form2)); w.Code != 400 {
		t.Fatalf("empty title=%d", w.Code)
	}
	// Unknown feed
	form3 := url.Values{"url": {"https://nope/feed"}, "title": {"y"}}
	if w := do(mux, req("POST", "/ui/feed/rename", tok, form3)); w.Code != 404 {
		t.Fatalf("nope=%d", w.Code)
	}
	// Save error
	op.saveErr = errBoom
	if w := do(mux, req("POST", "/ui/feed/rename", tok, form)); w.Code != 500 {
		t.Fatalf("save=%d", w.Code)
	}
	op.saveErr = nil
}

func TestFeedRenameParseFormErr(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	r := httptest.NewRequest("POST", "/ui/feed/rename", strings.NewReader("%ZZ"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestFeedViewIncludesRenameForm(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "Old"}}
	w := do(mux, req("GET", "/ui/feed?id=https://x/feed", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `action="feed/rename"`) {
		t.Fatalf("rename form missing: %s", w.Body.String())
	}
}

func TestFeedAddDropsReservedTag(t *testing.T) {
	// /ui/feed/add tags input must silently drop the reserved pseudo-
	// tag name so the home sidebar's __untagged__ bucket can't be
	// shadowed by a real tag of the same name.
	_, mux, _, op, tok, _ := fixture(t)
	form := url.Values{
		"url":  {"https://x/feed"},
		"tags": {"ok, " + store.ReservedTagUntagged},
	}
	if w := do(mux, req("POST", "/ui/feed/add", tok, form)); w.Code != 303 {
		t.Fatalf("code=%d", w.Code)
	}
	got := op.op.Feeds[0].Tags
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("got %v", got)
	}
}

func TestFeedTagChipDropsReserved(t *testing.T) {
	// /ui/feed/tag add=__untagged__ → 200 (silent drop), feed unchanged.
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	form := url.Values{"url": {"https://x/feed"}, "add": {store.ReservedTagUntagged}}
	if w := do(mux, req("POST", "/ui/feed/tag", tok, form)); w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if len(op.op.Feeds[0].Tags) != 0 {
		t.Fatalf("reserved leaked: %v", op.op.Feeds[0].Tags)
	}
}

func TestFeedTagChipExoticTagName(t *testing.T) {
	// Tag names containing quotes / backslashes used to break the
	// hx-vals JSON in the remove button. The chip is now a form, so a
	// tag with `"` survives a round-trip: chips render the tag inside
	// proper HTML-attribute-escaped form values, and the remove form
	// POSTs the literal value.
	_, mux, _, op, tok, _ := fixture(t)
	odd := `a"b\c`
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X", Tags: []string{odd}}}
	w := do(mux, req("GET", "/ui/feed?id=https://x/feed", tok, nil))
	body := w.Body.String()
	// Source must HTML-escape the tag inside the form's hidden input.
	if !strings.Contains(body, `value="a&#34;b\c"`) {
		t.Fatalf("expected escaped value attr: %s", body)
	}
	// Remove must succeed.
	form := url.Values{"url": {"https://x/feed"}, "remove": {odd}}
	if w := do(mux, req("POST", "/ui/feed/tag", tok, form)); w.Code != 200 {
		t.Fatalf("remove code=%d", w.Code)
	}
	if op.op.Feeds[0].HasTag(odd) {
		t.Fatalf("not removed: %v", op.op.Feeds[0].Tags)
	}
}

func TestFeedTagChipBadForm(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	// invalid percent escape → ParseForm error
	r := httptest.NewRequest("POST", "/ui/feed/tag", strings.NewReader("url=%ZZ"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestFeedViewIncludesTagChips(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X", Tags: []string{"news"}}}
	w := do(mux, req("GET", "/ui/feed?id=https://x/feed", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "tagchips") || !strings.Contains(w.Body.String(), "news") {
		t.Fatalf("missing tag chips: %s", w.Body.String())
	}
}

// ---- footer version --------------------------------------------------

func TestFooterShowsVersion(t *testing.T) {
	srv, mux, _, _, tok, _ := fixture(t)
	srv.Version = "1.2.3"
	// Login page (no auth required) — footer should still render.
	w := do(mux, req("GET", "/ui/login", "", nil))
	if w.Code != 200 {
		t.Fatalf("login code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `<footer class="site-footer">Harbour RSS <span class="version">1.2.3</span></footer>`) {
		t.Fatalf("login footer missing version: %s", w.Body.String())
	}
	// Authed page — same footer.
	w = do(mux, req("GET", "/ui/", tok, nil))
	if w.Code != 200 {
		t.Fatalf("home code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `>1.2.3<`) {
		t.Fatalf("home footer missing version: %s", w.Body.String())
	}
}

func TestFooterHiddenWhenVersionEmpty(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t) // fixture leaves Version == ""
	w := do(mux, req("GET", "/ui/", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if strings.Contains(w.Body.String(), "site-footer") {
		t.Fatalf("footer should be hidden when Version unset: %s", w.Body.String())
	}
}

// ---- new UI features (icons, persistent unread filter, tag groups,
//      hover-only chip remove, datalist suggestions) ------------------

// TestEntryRowUsesIconButtons confirms the read/star buttons render as
// icon glyphs with accessible labels rather than text words.
func TestEntryRowUsesIconButtons(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	w := do(mux, req("GET", "/ui/feed?id="+url.QueryEscape(u), tok, nil))
	body := w.Body.String()
	for _, want := range []string{
		`class="readbtn icon"`,
		`class="starbtn icon"`,
		`aria-label="mark read"`,
		`aria-label="star"`,
		`>●<`, // unread glyph
		`>☆<`, // not-starred glyph
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in feed page: %s", want, body)
		}
	}
}

// TestHomeDefaultsToUnreadOnly proves the default home view filters
// to unread feeds when neither ?unread= nor the cookie is set.
func TestHomeDefaultsToUnreadOnly(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://a/feed", Title: "Alpha"},
		{XMLURL: "https://b/feed", Title: "Bravo"},
	}
	// Only Alpha has an unread entry.
	now := time.Now()
	st.AppendEntries(store.FeedHash("https://a/feed"), []store.Entry{
		{GUID: "g1", Title: "T", Published: now, FetchedAt: now},
	})
	w := do(mux, req("GET", "/ui/", tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, "Alpha") {
		t.Fatalf("Alpha must be visible by default: %s", body)
	}
	if strings.Contains(body, "Bravo") {
		t.Fatalf("Bravo (no unread) must be hidden by default: %s", body)
	}
	if !strings.Contains(body, `class="filter active"`) {
		t.Fatalf("filter pill must render active by default: %s", body)
	}
}

// TestUnreadCookiePersistsAcrossPages confirms a click on the toggle
// pill (which carries ?unread=0/1) writes a cookie that subsequent
// plain navigations honour without the query param.
func TestUnreadCookiePersistsAcrossPages(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://a/feed", Title: "Alpha"},
		{XMLURL: "https://b/feed", Title: "Bravo"},
	}
	now := time.Now()
	st.AppendEntries(store.FeedHash("https://a/feed"), []store.Entry{
		{GUID: "g1", Title: "T", Published: now, FetchedAt: now},
	})
	// 1) Visit ?unread=0 to flip the choice off.
	w := do(mux, req("GET", "/ui/?unread=0", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	cookies := w.Result().Cookies()
	var pref *http.Cookie
	for _, c := range cookies {
		if c.Name == "h_unread" {
			pref = c
		}
	}
	if pref == nil || pref.Value != "0" {
		t.Fatalf("expected h_unread=0 cookie, got %v", cookies)
	}
	// 2) A plain navigation with the cookie attached must show all
	//    feeds (the persisted "0" wins over the default "1").
	r := req("GET", "/ui/", tok, nil)
	r.AddCookie(pref)
	w = do(mux, r)
	body := w.Body.String()
	if !strings.Contains(body, "Bravo") {
		t.Fatalf("cookie=0 should reveal Bravo: %s", body)
	}
	// 3) Per-feed page must also pick up the persisted choice (it
	//    means: show all entries in the feed by default).
	r = req("GET", "/ui/feed?id="+url.QueryEscape("https://a/feed"), tok, nil)
	r.AddCookie(pref)
	w = do(mux, r)
	body = w.Body.String()
	if !strings.Contains(body, "show unread only") {
		t.Fatalf("per-feed pill should be off (offer to turn on): %s", body)
	}
}

// TestHomeFeedGroupsByTag confirms feeds appear under each of their
// tags, with an Untagged trailing group and a per-group collapse
// toggle. Feeds with multiple tags duplicate across groups.
func TestHomeFeedGroupsByTag(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://a/feed", Title: "Alpha", Tags: []string{"tech"}},
		{XMLURL: "https://b/feed", Title: "Bravo", Tags: []string{"tech", "daily"}},
		{XMLURL: "https://c/feed", Title: "Charlie"}, // untagged
	}
	now := time.Now()
	for _, f := range op.op.Feeds {
		st.AppendEntries(store.FeedHash(f.XMLURL), []store.Entry{
			{GUID: "g-" + f.XMLURL, Title: "T", Published: now, FetchedAt: now},
		})
	}
	w := do(mux, req("GET", "/ui/", tok, nil))
	body := w.Body.String()
	// Three group headers.
	for _, want := range []string{
		`data-tag="tech"`,
		`data-tag="daily"`,
		`data-tag="__untagged__"`,
		`class="feed-group-toggle"`,
		`aria-controls="grp-tech"`,
		`aria-controls="grp-daily"`,
		`aria-controls="grp-untagged"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in grouped home: %s", want, body)
		}
	}
	// Bravo carries both tech and daily → must appear twice.
	if n := strings.Count(body, `>Bravo</a>`); n != 2 {
		t.Fatalf("Bravo should appear under both tags exactly twice, got %d: %s", n, body)
	}
	// Charlie only in Untagged.
	if n := strings.Count(body, `>Charlie</a>`); n != 1 {
		t.Fatalf("Charlie should appear exactly once (Untagged), got %d", n)
	}
}

// TestTagChipsHaveDatalistOfAllTags confirms the add-tag <input> is
// backed by a <datalist> populated from every tag the OPML knows
// about — so the user can autocomplete an existing tag or type a new
// one freely.
func TestTagChipsHaveDatalistOfAllTags(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{
		{XMLURL: "https://a/feed", Title: "Alpha", Tags: []string{"tech", "weekly"}},
		{XMLURL: "https://b/feed", Title: "Bravo", Tags: []string{"news"}},
	}
	w := do(mux, req("GET", "/ui/feed?id="+url.QueryEscape("https://a/feed"), tok, nil))
	body := w.Body.String()
	for _, want := range []string{
		`<datalist id="all-tags-https://a/feed">`,
		`<option value="tech">`,
		`<option value="weekly">`,
		`<option value="news">`,
		`list="all-tags-https://a/feed"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("datalist missing %q: %s", want, body)
		}
	}
	// Re-renders from POST /ui/feed/tag must also include the datalist
	// (so the add-tag field stays useful after edits).
	form := url.Values{"url": {"https://a/feed"}, "add": {"extra"}}
	r := req("POST", "/ui/feed/tag", tok, form)
	w = do(mux, r)
	body = w.Body.String()
	if !strings.Contains(body, `<datalist id="all-tags-https://a/feed">`) {
		t.Fatalf("tagchips POST response missing datalist: %s", body)
	}
	if !strings.Contains(body, `<option value="extra">`) {
		t.Fatalf("expected freshly-added tag in datalist: %s", body)
	}
}

// TestTagChipRemoveIsLessProminent confirms the per-chip × button
// carries the CSS hook (.tagchip-x) we rely on to hide-until-hover.
// The "less prominent" framing is enforced by style.css, but the
// markup contract is what tests can pin down.
func TestTagChipRemoveIsLessProminent(t *testing.T) {
	_, mux, _, op, tok, _ := fixture(t)
	op.op.Feeds = []store.Feed{{XMLURL: "https://a/feed", Title: "A", Tags: []string{"tech"}}}
	w := do(mux, req("GET", "/ui/feed?id="+url.QueryEscape("https://a/feed"), tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, `class="tagchip-x"`) {
		t.Fatalf("missing tagchip-x class hook: %s", body)
	}
	if !strings.Contains(body, `aria-label="remove tag tech"`) {
		t.Fatalf("missing accessible label on remove control: %s", body)
	}
}

// TestTagSlug pins the DOM-id slug derivation. The empty-string and
// fully-non-alnum cases are reachable from data sources outside the
// hot path (custom OPML, hypothetical empty tag) and must yield
// stable, addressable ids.
func TestTagSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "all"},
		{"tech", "tech"},
		{"News & Stuff", "News-Stuff"},
		{"a/b/c", "a-b-c"},
		{"--", "tag"},
		{"   ", "tag"},
	}
	for _, c := range cases {
		if got := tagSlug(c.in); got != c.want {
			t.Errorf("tagSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildFeedGroupsSortsUntaggedLast feeds the grouper a mix that
// forces both branches of the sort comparator to fire (untagged on
// either side of the comparison).
func TestBuildFeedGroupsSortsUntaggedLast(t *testing.T) {
	feeds := []homeFeed{
		{Title: "Mike", URL: "m"}, // untagged → inserted first
		{Title: "Zed", URL: "z", Tags: []string{"zeta"}},
		{Title: "Anne", URL: "a", Tags: []string{"alpha"}},
	}
	gs := buildFeedGroups(feeds, "")
	if len(gs) != 3 {
		t.Fatalf("want 3 groups, got %d", len(gs))
	}
	if gs[0].Label != "alpha" || gs[1].Label != "zeta" || gs[2].Label != "Untagged" {
		t.Fatalf("order wrong: %q %q %q", gs[0].Label, gs[1].Label, gs[2].Label)
	}
}

// TestWriteUnreadCookieNoopWhenAbsent confirms writeUnreadCookie is a
// no-op when the request URL carries no ?unread= param, so plain
// navigations don't overwrite the user's persisted choice.
func TestWriteUnreadCookieNoopWhenAbsent(t *testing.T) {
	r := httptest.NewRequest("GET", "/ui/", nil)
	w := httptest.NewRecorder()
	writeUnreadCookie(w, r, false)
	if cs := w.Result().Cookies(); len(cs) != 0 {
		t.Fatalf("expected no cookies set, got %v", cs)
	}
	// Bad value also a no-op.
	r = httptest.NewRequest("GET", "/ui/?unread=garbage", nil)
	w = httptest.NewRecorder()
	writeUnreadCookie(w, r, false)
	if cs := w.Result().Cookies(); len(cs) != 0 {
		t.Fatalf("garbage value should not set cookie, got %v", cs)
	}
}

// TestEntryPanelFragment verifies that /ui/entry?id=...&panel=1 returns
// just the entry-detail fragment (no <html>/<head>/<body> chrome). This
// is the endpoint the split-panel layout's hx-get on entry rows hits.
func TestEntryPanelFragment(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	es, _ := st.ListEntries(store.FeedHash(u))
	h := es[0].Hash
	w := do(mux, req("GET", "/ui/entry?id="+h+"&panel=1", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "entry-detail-"+h) {
		t.Fatalf("expected detail fragment id, got: %s", body)
	}
	// Fragment must NOT include the page chrome.
	if strings.Contains(body, "<html") || strings.Contains(body, "<header") {
		t.Fatalf("panel fragment leaked page chrome: %s", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type=%q", ct)
	}
}

// TestEntryPanelFragmentSummary covers the Content=="" branch of the
// panel renderer so the summary fallback runs.
func TestEntryPanelFragmentSummary(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := "https://demo.example/feed"
	op.op.Feeds = []store.Feed{{XMLURL: u, Title: "Demo"}}
	now := time.Now().UTC()
	st.AppendEntries(store.FeedHash(u), []store.Entry{{
		GUID: "g1", Link: "https://demo.example/p", Title: "T",
		Summary: "fallback-summary", Published: now, FetchedAt: now,
	}})
	es, _ := st.ListEntries(store.FeedHash(u))
	w := do(mux, req("GET", "/ui/entry?id="+es[0].Hash+"&panel=1", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "fallback-summary") {
		t.Fatalf("expected summary fallback, got: %s", w.Body.String())
	}
}

// TestSplitLayoutMarkup verifies the entry-list pages render the
// split-panel scaffolding (left list + right detail pane with hx-get
// rows gated by a media-query event filter).
func TestSplitLayoutMarkup(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	for _, p := range []string{
		"/ui/feed?id=" + u,
		"/ui/all",
		"/ui/starred",
	} {
		w := do(mux, req("GET", p, tok, nil))
		if w.Code != 200 {
			t.Fatalf("%s: code=%d", p, w.Code)
		}
		body := w.Body.String()
		// Wide main column on list pages so the split has room.
		if !strings.Contains(body, `<main class="wide">`) {
			t.Fatalf("%s: missing wide main: %s", p, body)
		}
		if !strings.Contains(body, `class="split"`) {
			t.Fatalf("%s: missing .split: %s", p, body)
		}
		if !strings.Contains(body, `id="detail-pane"`) {
			t.Fatalf("%s: missing #detail-pane: %s", p, body)
		}
		if !strings.Contains(body, `aria-live="polite"`) {
			t.Fatalf("%s: detail-pane missing aria-live: %s", p, body)
		}
	}
	// Entry rows on a populated feed carry a plain title link
	// (class="entry-link") pointing at the full entry page. keys.js
	// upgrades the click to a split-panel swap on wide screens and
	// leaves the native href intact on narrow screens — see openEntry
	// in keys.js for why this is JS-driven and not an hx-trigger media
	// filter (htmx preventDefaults <a href> before the filter runs,
	// which killed mobile taps entirely).
	w := do(mux, req("GET", "/ui/feed?id="+u, tok, nil))
	body := w.Body.String()
	if !strings.Contains(body, `class="entry-link"`) {
		t.Fatalf("missing entry-link class on row: %s", body)
	}
	if !strings.Contains(body, `<a href="entry?id=`) {
		t.Fatalf("missing native entry href on row: %s", body)
	}
	// The row must NOT carry an hx-trigger media filter — that was the
	// mobile-dead-tap regression we removed.
	if strings.Contains(body, "matchMedia") || strings.Contains(body, "hx-trigger") {
		t.Fatalf("entry row should not carry an htmx media-query trigger: %s", body)
	}
}

// TestSplitLayoutCSS — the bundled stylesheet must carry the rules the
// split layout depends on (the .detail-pane visibility flip lives in a
// media query; if it ever regresses, narrow screens get a broken pane
// and wide screens get no panel).
func TestSplitLayoutCSS(t *testing.T) {
	_, mux, _, _, _, _ := fixture(t)
	w := do(mux, req("GET", "/ui/static/style.css", "", nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	css := w.Body.String()
	for _, sel := range []string{".split", ".detail-pane", "main.wide", "min-width: 64em"} {
		if !strings.Contains(css, sel) {
			t.Fatalf("style.css missing %q — split layout will be broken", sel)
		}
	}
}

// TestEntryTitleEntityDecodeRender pins the title-entity-decode fix
// from the rendering side: once ingestion has decoded HTML entities in
// a title to plain unicode (curly quotes, ampersands, apostrophes),
// html/template must render those code points directly (or via the
// canonical minimal escape for & < > etc.) — never as the original
// numeric / named entity strings the user reported seeing in the UI.
func TestEntryTitleEntityDecodeRender(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := "https://demo.example/feed"
	op.op.Feeds = []store.Feed{{XMLURL: u, Title: "Demo", HTMLURL: "https://demo.example"}}
	// Title is what the poll-ingestion layer would have produced:
	// entities already decoded to plain unicode, including a bare &
	// which the template must re-escape on output.
	decoded := "Motorola says affiliate hijacking of Amazon app was \u2018unintended\u2019 & 'quote'"
	now := time.Now().UTC()
	if _, err := st.AppendEntries(store.FeedHash(u), []store.Entry{{
		GUID: "g1", Link: "https://demo.example/a", Title: decoded,
		Content: "<p>body</p>", Summary: "s", Published: now, FetchedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	w := do(mux, req("GET", "/ui/feed?id="+u, tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Curly quotes round-trip as their unicode code points — no
	// escaping required by html/template.
	if !strings.Contains(body, "\u2018unintended\u2019") {
		t.Fatalf("rendered HTML missing decoded curly quotes around 'unintended'")
	}
	// A bare & must come out as &amp; — that's html/template doing its
	// normal escape on plain text, NOT us shipping a literal entity.
	if !strings.Contains(body, "&amp;") {
		t.Fatalf("expected & to be escaped to &amp; in rendered HTML, got: %s", body)
	}
	// html/template canonically escapes apostrophe to &#39; in HTML
	// text context; that's "properly escaped by the template" and
	// distinct from the bug's leftover entity strings below.
	if !strings.Contains(body, "&#39;quote&#39;") {
		t.Fatalf("expected apostrophe to be canonically escaped to &#39; in rendered HTML, got: %s", body)
	}
	// Crucially: none of the original entity strings must survive into
	// the rendered HTML — that's exactly the bug.
	for _, ent := range []string{"&#8216;", "&#8217;", "&#x27;", "&amp;#8216;", "&amp;#8217;", "&amp;amp;"} {
		if strings.Contains(body, ent) {
			t.Fatalf("rendered HTML still contains raw entity %q", ent)
		}
	}
}

// TestLinksOpenInNewTab — links inside rendered entry content must
// open in a new browser tab so the reader doesn't lose their place in
// the feed list. The rewriter must add target="_blank" and the
// noopener noreferrer rel pair to every <a> that doesn't already
// carry a target attribute.
func TestLinksOpenInNewTab(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := "https://demo.example/feed"
	op.op.Feeds = []store.Feed{{XMLURL: u, Title: "Demo", HTMLURL: "https://demo.example"}}
	now := time.Now().UTC()
	e := store.Entry{
		GUID:      "g1",
		Link:      "https://demo.example/p1",
		Title:     "T",
		Content:   `<p>see <a href="https://example.com/a">A</a> and <A HREF="https://example.com/b">B</A>, plus <a href="https://example.com/c" target="_self">C</a>.</p>`,
		Summary:   "summary",
		Published: now, FetchedAt: now,
	}
	if _, err := st.AppendEntries(store.FeedHash(u), []store.Entry{e}); err != nil {
		t.Fatalf("append: %v", err)
	}
	es, _ := st.ListEntries(store.FeedHash(u))
	w := do(mux, req("GET", "/ui/entry?id="+es[0].Hash, tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	// Tags without an existing target= must be rewritten.
	if !strings.Contains(body, `<a href="https://example.com/a" target="_blank" rel="noopener noreferrer">`) {
		t.Fatalf("lowercase <a> not rewritten: %s", body)
	}
	if !strings.Contains(body, `<a href="https://example.com/b" target="_blank" rel="noopener noreferrer">`) {
		t.Fatalf("uppercase <A> not rewritten/normalised: %s", body)
	}
	// Tags with an existing target= must be left untouched (author
	// intent wins) — and crucially must NOT have a second target= or
	// rel= appended after it.
	if !strings.Contains(body, `<a href="https://example.com/c" target="_self">`) {
		t.Fatalf("explicit target=_self should be preserved verbatim: %s", body)
	}
	if strings.Contains(body, `target="_self" target="_blank"`) {
		t.Fatalf("double target= attribute appended: %s", body)
	}

	// Also exercise the toggleFlag detail-view branch, which renders
	// the same body through a separate handler path.
	wd := do(mux, req("POST", "/ui/entry/read?id="+es[0].Hash+"&state=1&view=detail", tok, nil))
	if wd.Code != 200 {
		t.Fatalf("detail code=%d", wd.Code)
	}
	if !strings.Contains(wd.Body.String(), `target="_blank" rel="noopener noreferrer"`) {
		t.Fatalf("detail view body not rewritten: %s", wd.Body.String())
	}

	// Body falling back to Summary path: empty Content, summary holds
	// the link.
	e2 := store.Entry{
		GUID: "g2", Link: "https://demo.example/p2", Title: "T2",
		Content: "", Summary: `<a href="https://example.com/s">S</a>`,
		Published: now, FetchedAt: now,
	}
	if _, err := st.AppendEntries(store.FeedHash(u), []store.Entry{e2}); err != nil {
		t.Fatalf("append2: %v", err)
	}
	es2, _ := st.ListEntries(store.FeedHash(u))
	var h2 string
	for _, x := range es2 {
		if x.GUID == "g2" {
			h2 = x.Hash
		}
	}
	w2 := do(mux, req("GET", "/ui/entry?id="+h2+"&panel=1", tok, nil))
	if w2.Code != 200 {
		t.Fatalf("panel code=%d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), `target="_blank" rel="noopener noreferrer"`) {
		t.Fatalf("panel/summary-fallback body not rewritten: %s", w2.Body.String())
	}
}

// TestEntryFeedRemovedReturns404 covers findEntry's "entry is still
// indexed but its owning feed was unsubscribed from the OPML" branch:
// the entry must be invisible (404) rather than rendered orphaned.
func TestEntryFeedRemovedReturns404(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	es := st.IndexedEntries(store.FeedHash(u))
	h := es[0].Hash
	// Unsubscribe the feed; the entry stays in the store index.
	op.op.Feeds = nil
	if w := do(mux, req("GET", "/ui/entry?id="+h, tok, nil)); w.Code != 404 {
		t.Fatalf("orphaned entry should 404, got %d", w.Code)
	}
}

// TestToggleFlagRequiresPost ensures the read/star toggle endpoints
// reject non-POST methods like the other mutators.
func TestToggleFlagRequiresPost(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 1)
	h := st.IndexedEntries(store.FeedHash(u))[0].Hash
	for _, p := range []string{"/ui/entry/read?id=" + h + "&state=1", "/ui/entry/star?id=" + h + "&state=1"} {
		if w := do(mux, req("GET", p, tok, nil)); w.Code != 405 {
			t.Fatalf("GET %s: want 405, got %d", p, w.Code)
		}
	}
}

// TestFeedPanel covers the panel=1 fragment branch of handleFeed used
// by the home master-detail layout: it renders just the feed's entry
// list (the "feedpane" partial) with no page chrome, so keys.js can
// swap it into #feed-pane via htmx when a feed is keyboard-selected.
func TestFeedPanel(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	u := seed(t, st, op, 2)
	w := do(mux, req("GET", "/ui/feed?id="+u+"&panel=1", tok, nil))
	if w.Code != 200 {
		t.Fatalf("panel code=%d", w.Code)
	}
	body := w.Body.String()
	// Fragment, not a full page: no doctype / base chrome.
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "<main") {
		t.Fatalf("panel fragment should carry no page chrome: %s", body)
	}
	// Carries the feed-pane heading (linking to the full feed view) and
	// at least one entry row.
	if !strings.Contains(body, "feed-pane-head") {
		t.Fatalf("panel missing feed-pane-head: %s", body)
	}
	if !strings.Contains(body, `class="entry`) {
		t.Fatalf("panel missing entry rows: %s", body)
	}
	// Empty feed (no entries) still renders the fragment with a
	// placeholder rather than erroring.
	empty := "https://empty.example/feed"
	op.op.Feeds = append(op.op.Feeds, store.Feed{XMLURL: empty, Title: "Empty"})
	we := do(mux, req("GET", "/ui/feed?id="+empty+"&panel=1&unread=0", tok, nil))
	if we.Code != 200 || !strings.Contains(we.Body.String(), "detail-placeholder") {
		t.Fatalf("empty panel: code=%d body=%s", we.Code, we.Body.String())
	}
}

// TestHomeMasterDetailMarkup pins the home page's master-detail layout
// shell: a wide main column, the .split grid, and the #feed-pane the
// keyboard nav previews selected feeds into. Narrow-screen collapse is
// handled entirely in CSS (covered by TestSplitLayoutCSS).
func TestHomeMasterDetailMarkup(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)
	seed(t, st, op, 1)
	w := do(mux, req("GET", "/ui/", tok, nil))
	if w.Code != 200 {
		t.Fatalf("home code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`<main class="wide">`, `class="split home-split"`, `id="feed-pane"`, `aria-live="polite"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("home master-detail missing %q: %s", want, body)
		}
	}
}
func TestVersionEndpoint(t *testing.T) {
	_, mux, st, op, tok, _ := fixture(t)

	// Unauthenticated → redirect to login (session-protected like the
	// rest of /ui).
	if w := do(mux, req("GET", "/ui/version", "", nil)); w.Code != http.StatusSeeOther {
		t.Fatalf("unauth code=%d, want 303", w.Code)
	}

	// 200 carries the StateVersion as a decimal body + matching ETag.
	w := do(mux, req("GET", "/ui/version", tok, nil))
	if w.Code != 200 {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	etag := w.Header().Get("ETag")
	if etag != `"`+body+`"` {
		t.Fatalf("etag %q does not wrap body %q", etag, body)
	}
	if _, err := strconv.ParseInt(body, 10, 64); err != nil {
		t.Fatalf("body %q not a decimal: %v", body, err)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Fatalf("cache-control=%q", cc)
	}

	// If-None-Match with the current ETag → 304, no body.
	r := req("GET", "/ui/version", tok, nil)
	r.Header.Set("If-None-Match", etag)
	w304 := do(mux, r)
	if w304.Code != http.StatusNotModified {
		t.Fatalf("INM match code=%d, want 304", w304.Code)
	}
	if w304.Body.Len() != 0 {
		t.Fatalf("304 had body %q", w304.Body.String())
	}
	if w304.Header().Get("ETag") != etag {
		t.Fatalf("304 etag=%q, want %q", w304.Header().Get("ETag"), etag)
	}

	// A content mutation (new entries) must advance the value so the
	// same INM now gets a fresh 200.
	u := seed(t, st, op, 1)
	_ = u
	r2 := req("GET", "/ui/version", tok, nil)
	r2.Header.Set("If-None-Match", etag)
	w2 := do(mux, r2)
	if w2.Code != 200 {
		t.Fatalf("after append code=%d, want 200", w2.Code)
	}
	if w2.Body.String() == body {
		t.Fatalf("version did not change after AppendEntries: %q", body)
	}

	// A read-state mutation advances it again (SetRead bumps
	// StateVersion).
	prev := w2.Body.String()
	prevEtag := w2.Header().Get("ETag")
	e := st.IndexedEntries(store.FeedHash(u))[0]
	if err := st.SetRead(e.Hash, true); err != nil {
		t.Fatal(err)
	}
	r3 := req("GET", "/ui/version", tok, nil)
	r3.Header.Set("If-None-Match", prevEtag)
	w3 := do(mux, r3)
	if w3.Code != 200 || w3.Body.String() == prev {
		t.Fatalf("after SetRead code=%d body=%q (prev %q)", w3.Code, w3.Body.String(), prev)
	}
}

func TestBaseStateVerRendered(t *testing.T) {
	_, mux, _, _, tok, _ := fixture(t)
	w := do(mux, req("GET", "/ui/?unread=0", tok, nil))
	if w.Code != 200 {
		t.Fatalf("home code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "data-state-ver=") {
		t.Fatalf("home page missing data-state-ver attribute")
	}
	// Login page (unauthenticated) must NOT carry the attribute.
	wl := do(mux, req("GET", "/ui/login", "", nil))
	if strings.Contains(wl.Body.String(), "data-state-ver=") {
		t.Fatalf("login page should not carry data-state-ver")
	}
}
