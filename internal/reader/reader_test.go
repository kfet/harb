package reader

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/harb/internal/auth"
	"github.com/kfet/harb/internal/store"
)

// memOPML is an in-memory OPMLProvider. Failure modes can be injected via
// loadErr / saveErr.
type memOPML struct {
	mu      sync.Mutex
	opml    store.OPML
	loadErr error
	saveErr error
}

func (m *memOPML) Load() (*store.OPML, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	cp := m.opml
	cp.Feeds = append([]store.Feed{}, m.opml.Feeds...)
	return &cp, nil
}
func (m *memOPML) Save(o *store.OPML) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.opml = *o
	m.opml.Feeds = append([]store.Feed{}, o.Feeds...)
	return nil
}

// Update mirrors config.FileOPML.Update — it holds the mock's lock
// across the whole load→mutate→save cycle so concurrent mutators are
// serialized (exercised by the lost-update concurrency test).
func (m *memOPML) Update(fn func(*store.OPML) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return m.loadErr
	}
	cp := m.opml
	cp.Feeds = append([]store.Feed{}, m.opml.Feeds...)
	if err := fn(&cp); err != nil {
		return err
	}
	if m.saveErr != nil {
		return m.saveErr
	}
	m.opml = cp
	m.opml.Feeds = append([]store.Feed{}, cp.Feeds...)
	return nil
}

var testPwHash = mustHashPw()

func mustHashPw() string {
	h, err := auth.HashPassword("p")
	if err != nil {
		panic(err)
	}
	return h
}

// fixture builds a Server + ServerMux + valid API token + memOPML.
func fixture(t *testing.T) (*Server, http.Handler, string, *memOPML, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := auth.Config{Username: "u", PasswordHash: testPwHash}
	as, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"), cfg)
	tok, _ := as.IssueAPIToken("u", "p")
	op := &memOPML{}
	s := New(st, as, op)
	mux := http.NewServeMux()
	handler := s.Routes(mux)
	return s, handler, tok, op, st
}

func do(t *testing.T, handler http.Handler, method, path, tok string, body url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if tok != "" {
		r.Header.Set("Authorization", "GoogleLogin auth="+tok)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestClientLogin(t *testing.T) {
	_, mux, _, _, _ := fixture(t)
	body := url.Values{"Email": {"u"}, "Passwd": {"p"}}
	w := do(t, mux, "POST", "/accounts/ClientLogin", "", body)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Auth=") {
		t.Fatal("missing Auth")
	}
	// lowercase params accepted
	body2 := url.Values{"email": {"u"}, "passwd": {"p"}}
	if w := do(t, mux, "POST", "/accounts/ClientLogin", "", body2); w.Code != 200 {
		t.Fatalf("lower code=%d", w.Code)
	}
	// Bad creds → 401
	bad := url.Values{"Email": {"u"}, "Passwd": {"wrong"}}
	if w := do(t, mux, "POST", "/accounts/ClientLogin", "", bad); w.Code != 401 {
		t.Fatalf("bad code=%d", w.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	_, mux, _, _, _ := fixture(t)
	if w := do(t, mux, "GET", "/reader/api/0/user-info", "", nil); w.Code != 401 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestToken(t *testing.T) {
	_, mux, tok, _, _ := fixture(t)
	w := do(t, mux, "GET", "/reader/api/0/token", tok, nil)
	if w.Code != 200 || w.Body.String() != tok {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

func TestUserInfo(t *testing.T) {
	srv, mux, tok, _, _ := fixture(t)
	srv.Version = "9.9.9"
	w := do(t, mux, "GET", "/reader/api/0/user-info", tok, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"userName":"u"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"harbVersion":"9.9.9"`) {
		t.Fatalf("missing harbVersion: %s", w.Body.String())
	}
}

func TestStatus(t *testing.T) {
	srv, mux, _, _, _ := fixture(t)
	srv.Version = "1.2.3"
	srv.Commit = "abc1234"
	srv.BuildDate = "2026-01-02T03:04:05Z"
	// Unauthenticated.
	w := do(t, mux, "GET", "/status", "", nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	for k, want := range map[string]string{
		"name":      "Harbour RSS",
		"product":   "harb",
		"version":   "1.2.3",
		"commit":    "abc1234",
		"buildDate": "2026-01-02T03:04:05Z",
	} {
		if got[k] != want {
			t.Errorf("status[%q]=%q want %q", k, got[k], want)
		}
	}
}

func TestSubscriptionListAndTagList(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{
		{XMLURL: "https://a/feed", Title: "A", Tags: []string{"News"}, HTMLURL: "https://a"},
		{XMLURL: "https://b/feed", Title: "B"},
	}
	w := do(t, mux, "GET", "/reader/api/0/subscription/list", tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"feed/https://a/feed"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
	w2 := do(t, mux, "GET", "/reader/api/0/tag/list", tok, nil)
	if !strings.Contains(w2.Body.String(), "user/-/label/News") {
		t.Fatalf("tag body=%s", w2.Body.String())
	}
}

func TestSubscriptionListLoadErr(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.loadErr = errBoom
	for _, p := range []string{
		"/reader/api/0/subscription/list",
		"/reader/api/0/tag/list",
		"/reader/api/0/unread-count",
	} {
		w := do(t, mux, "GET", p, tok, nil)
		if w.Code != 500 {
			t.Fatalf("%s code=%d", p, w.Code)
		}
	}
}

func TestSubscriptionEdit(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	body := url.Values{"ac": {"subscribe"}, "s": {"feed/https://x/y"}, "t": {"X"}, "a": {"user/-/label/F"}}
	w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, body)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if len(op.opml.Feeds) != 1 || !op.opml.Feeds[0].HasTag("F") {
		t.Fatalf("opml=%+v", op.opml.Feeds)
	}
	// subscribe without title falls back to url
	body2 := url.Values{"ac": {"subscribe"}, "s": {"feed/https://y/y"}}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, body2); w.Code != 200 {
		t.Fatalf("w=%d", w.Code)
	}
	// edit
	edit := url.Values{"ac": {"edit"}, "s": {"feed/https://x/y"}, "t": {"NewTitle"}, "a": {"user/-/label/F2"}}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, edit); w.Code != 200 {
		t.Fatalf("w=%d", w.Code)
	}
	if op.opml.Feeds[0].Title != "NewTitle" || !op.opml.Feeds[0].HasTag("F2") {
		t.Fatalf("not edited: %+v", op.opml.Feeds[0])
	}
	// edit with r removing a tag (now: just F2; F remains)
	edit2 := url.Values{"ac": {"edit"}, "s": {"feed/https://x/y"}, "r": {"user/-/label/F2"}}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, edit2); w.Code != 200 {
		t.Fatalf("w=%d", w.Code)
	}
	if op.opml.Feeds[0].HasTag("F2") || !op.opml.Feeds[0].HasTag("F") {
		t.Fatalf("tag not cleared: %+v", op.opml.Feeds[0])
	}
	// edit with multiple a + multiple r in one request
	edit3 := url.Values{
		"ac": {"edit"}, "s": {"feed/https://x/y"},
		"a": {"user/-/label/X", "user/-/label/Y"},
		"r": {"user/-/label/F"},
	}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, edit3); w.Code != 200 {
		t.Fatalf("multi a/r w=%d", w.Code)
	}
	if !op.opml.Feeds[0].HasTag("X") || !op.opml.Feeds[0].HasTag("Y") || op.opml.Feeds[0].HasTag("F") {
		t.Fatalf("multi a/r unexpected: %+v", op.opml.Feeds[0].Tags)
	}
	// edit on missing
	miss := url.Values{"ac": {"edit"}, "s": {"feed/missing"}}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, miss); w.Code != 400 {
		t.Fatalf("miss code=%d", w.Code)
	}
	// unsubscribe
	un := url.Values{"ac": {"unsubscribe"}, "s": {"feed/https://x/y"}}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, un); w.Code != 200 {
		t.Fatalf("un=%d", w.Code)
	}
	// missing s
	miss2 := url.Values{"ac": {"subscribe"}, "s": {""}}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, miss2); w.Code != 400 {
		t.Fatalf("missing s code=%d", w.Code)
	}
	// bad ac
	bad := url.Values{"ac": {"nope"}, "s": {"feed/x"}}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, bad); w.Code != 400 {
		t.Fatalf("bad ac=%d", w.Code)
	}
}

func TestSubscriptionEditLoadAndSaveErr(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.loadErr = errBoom
	body := url.Values{"ac": {"subscribe"}, "s": {"feed/x"}}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, body); w.Code != 500 {
		t.Fatalf("load err code=%d", w.Code)
	}
	op.loadErr = nil
	op.saveErr = errBoom
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, body); w.Code != 500 {
		t.Fatalf("save err code=%d", w.Code)
	}
}

func TestQuickAdd(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	body := url.Values{"quickadd": {"https://q/feed"}}
	w := do(t, mux, "POST", "/reader/api/0/subscription/quickadd", tok, body)
	if w.Code != 200 || len(op.opml.Feeds) != 1 {
		t.Fatalf("code=%d feeds=%+v body=%s", w.Code, op.opml.Feeds, w.Body.String())
	}
	// missing
	if w := do(t, mux, "POST", "/reader/api/0/subscription/quickadd", tok, url.Values{}); w.Code != 400 {
		t.Fatalf("missing=%d", w.Code)
	}
	// load err
	op.loadErr = errBoom
	if w := do(t, mux, "POST", "/reader/api/0/subscription/quickadd", tok, body); w.Code != 500 {
		t.Fatalf("load err=%d", w.Code)
	}
	op.loadErr = nil
	op.saveErr = errBoom
	if w := do(t, mux, "POST", "/reader/api/0/subscription/quickadd", tok, body); w.Code != 500 {
		t.Fatalf("save err=%d", w.Code)
	}
}

// seedFeed populates one feed with N entries in the store, registers it
// in the OPML and returns the feed url.
func seedFeed(t *testing.T, op *memOPML, st *store.Store, count int, folder string) string {
	t.Helper()
	u := "https://feed.example/" + folder
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{XMLURL: u, Title: "F", Tags: []string{folder}, HTMLURL: "https://feed.example"})
	fh := store.FeedHash(u)
	now := time.Now().UTC()
	es := make([]store.Entry, count)
	for i := 0; i < count; i++ {
		es[i] = store.Entry{
			GUID:      "g" + strconv.Itoa(i),
			Link:      "https://feed.example/" + strconv.Itoa(i),
			Title:     "Item " + strconv.Itoa(i),
			Content:   "content " + strconv.Itoa(i),
			Summary:   "summ " + strconv.Itoa(i),
			Published: now.Add(time.Duration(-i) * time.Minute),
			FetchedAt: now,
		}
	}
	if _, err := st.AppendEntries(fh, es); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestStreamContentsAndPagination(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.MaxPage = 20
	url := seedFeed(t, op, st, 10, "F")
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/"+("feed/"+url)+"?n=2", tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp streamResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 2 || resp.Continuation == "" {
		t.Fatalf("got items=%d cont=%q", len(resp.Items), resp.Continuation)
	}
	// follow continuation to last page
	w2 := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+url+"?n=15&c="+resp.Continuation, tok, nil)
	var r2 streamResponse
	json.Unmarshal(w2.Body.Bytes(), &r2)
	if r2.Continuation != "" {
		t.Fatalf("expected last page, got cont %q", r2.Continuation)
	}
}

func TestStreamLabelStarredReadAndDefault(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.MaxPage = 50
	u := seedFeed(t, op, st, 5, "Cat")
	// Star one, read another.
	es, _ := st.ListEntries(store.FeedHash(u))
	st.SetStarred(es[0].Hash, true)
	st.SetRead(es[1].Hash, true)
	// label/Cat → all
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/user/-/label/Cat", tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	// starred → 1
	w = do(t, mux, "GET", "/reader/api/0/stream/contents/user/-/state/com.google/starred", tok, nil)
	var resp streamResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("starred items=%d", len(resp.Items))
	}
	// read → 1
	w = do(t, mux, "GET", "/reader/api/0/stream/contents/user/-/state/com.google/read", tok, nil)
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("read items=%d", len(resp.Items))
	}
	// reading-list (default) → all items; unread-only is reading-list + xt=read.
	w = do(t, mux, "GET", "/reader/api/0/stream/contents/user/-/state/com.google/reading-list", tok, nil)
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 5 {
		t.Fatalf("reading-list items=%d", len(resp.Items))
	}
	w = do(t, mux, "GET", "/reader/api/0/stream/contents/user/-/state/com.google/reading-list?xt="+url.QueryEscape(stateReadID), tok, nil)
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 4 {
		t.Fatalf("unread=%d", len(resp.Items))
	}
}

func TestStreamContentsLoadErr(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.loadErr = errBoom
	for _, p := range []string{
		"/reader/api/0/stream/contents/feed/x",
		"/reader/api/0/stream/items/ids",
		"/reader/api/0/stream/items/contents",
	} {
		w := do(t, mux, "POST", p, tok, url.Values{"i": {"abc"}})
		if w.Code != 500 {
			t.Fatalf("%s code=%d", p, w.Code)
		}
	}
}

func TestStreamContentsBadContinuation(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 2, "F")
	bad := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
	if w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+u+"?c="+bad, tok, nil); w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+u+"?c=$$notb64$$", tok, nil); w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	// n overflow → clamped
	if w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+u+"?n=10000", tok, nil); w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	// continuation beyond the end → empty page, not a panic
	huge, _ := json.Marshal(struct {
		Offset int `json:"o"`
	}{Offset: 999})
	hugeC := base64.RawURLEncoding.EncodeToString(huge)
	if w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+u+"?c="+hugeC, tok, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("huge continuation code=%d body=%s", w.Code, w.Body.String())
	}
	// n garbage
	if w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+u+"?n=oops", tok, nil); w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestItemsIDs(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.MaxPage = 3
	seedFeed(t, op, st, 5, "F")
	w := do(t, mux, "GET", "/reader/api/0/stream/items/ids", tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	// explicit n
	w = do(t, mux, "GET", "/reader/api/0/stream/items/ids?s="+streamReadingList+"&n=2", tok, nil)
	if !strings.Contains(w.Body.String(), `"itemRefs"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
	// bad n + huge n
	do(t, mux, "GET", "/reader/api/0/stream/items/ids?n=oops", tok, nil)
	do(t, mux, "GET", "/reader/api/0/stream/items/ids?n=99999", tok, nil)
}

func TestItemsContents(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 2, "F")
	es, _ := st.ListEntries(store.FeedHash(u))
	body := url.Values{"i": {itemID(es[0].Hash), itemID(es[1].Hash)}}
	w := do(t, mux, "POST", "/reader/api/0/stream/items/contents", tok, body)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp streamResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 2 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	// empty list → empty items
	if w := do(t, mux, "POST", "/reader/api/0/stream/items/contents", tok, url.Values{}); w.Code != 200 {
		t.Fatalf("empty code=%d", w.Code)
	}
	_ = srv
}

func TestEditTag(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 2, "F")
	es, _ := st.ListEntries(store.FeedHash(u))
	body := url.Values{
		"i": {itemID(es[0].Hash)},
		"a": {stateReadID, stateStarredID},
	}
	w := do(t, mux, "POST", "/reader/api/0/edit-tag", tok, body)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if !st.EntryState(es[0].Hash).Read || !st.EntryState(es[0].Hash).Starred {
		t.Fatal("flags not set")
	}
	// remove
	body2 := url.Values{"i": {itemID(es[0].Hash)}, "r": {stateReadID, stateStarredID}}
	if w := do(t, mux, "POST", "/reader/api/0/edit-tag", tok, body2); w.Code != 200 {
		t.Fatalf("rem code=%d", w.Code)
	}
	// missing i
	if w := do(t, mux, "POST", "/reader/api/0/edit-tag", tok, url.Values{}); w.Code != 400 {
		t.Fatalf("missing=%d", w.Code)
	}
	// unknown state → no-op, still 200
	body3 := url.Values{"i": {itemID(es[0].Hash)}, "a": {"user/-/label/F"}}
	if w := do(t, mux, "POST", "/reader/api/0/edit-tag", tok, body3); w.Code != 200 {
		t.Fatalf("unknown=%d", w.Code)
	}
}

func TestMarkAllRead(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 3, "F")
	body := url.Values{"s": {"feed/" + u}}
	if w := do(t, mux, "POST", "/reader/api/0/mark-all-as-read", tok, body); w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	es, _ := st.ListEntries(store.FeedHash(u))
	for _, e := range es {
		if !st.EntryState(e.Hash).Read {
			t.Fatalf("entry %s not read", e.Hash)
		}
	}
	// ts upper bound future → marks (already marked, idempotent).
	tsBody := url.Values{
		"s":  {"feed/" + u},
		"ts": {strconv.FormatInt(time.Now().Add(time.Hour).UnixMicro(), 10)},
	}
	if w := do(t, mux, "POST", "/reader/api/0/mark-all-as-read", tok, tsBody); w.Code != 200 {
		t.Fatalf("ts=%d", w.Code)
	}
	// ts way in the past → skips most
	tsBody2 := url.Values{
		"s":  {"feed/" + u},
		"ts": {strconv.FormatInt(time.Now().Add(-365*24*time.Hour).UnixMicro(), 10)},
	}
	if w := do(t, mux, "POST", "/reader/api/0/mark-all-as-read", tok, tsBody2); w.Code != 200 {
		t.Fatalf("ts2=%d", w.Code)
	}
	// missing s
	if w := do(t, mux, "POST", "/reader/api/0/mark-all-as-read", tok, url.Values{}); w.Code != 400 {
		t.Fatalf("missing=%d", w.Code)
	}
	// load err
	op.loadErr = errBoom
	body2 := url.Values{"s": {"feed/" + u}}
	if w := do(t, mux, "POST", "/reader/api/0/mark-all-as-read", tok, body2); w.Code != 500 {
		t.Fatalf("load=%d", w.Code)
	}
}

func TestUnreadCount(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	seedFeed(t, op, st, 4, "F")
	w := do(t, mux, "GET", "/reader/api/0/unread-count", tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"count":4`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestItemIDRoundtrip(t *testing.T) {
	h := "abcdef0123456789abcd"
	if got := itemIDToHash(itemID(h)); got != "abcdef0123456789" {
		t.Fatalf("got %q", got)
	}
	// long-form decimal — accepts and returns the 16-hex two's-complement form.
	dec := strconv.FormatInt(1234567890123, 10)
	if got := itemIDToHash(dec); got != "0000011f71fb04cb" {
		t.Fatalf("dec got %q", got)
	}
	// otherwise: raw passthrough
	if got := itemIDToHash("plainhash"); got != "plainhash" {
		t.Fatalf("got %q", got)
	}
}

func TestReaderTimeHelpers(t *testing.T) {
	pub := time.Unix(100, 0).UTC()
	fetched := time.Unix(200, 0).UTC()
	if got := entrySyncTime(store.Entry{Published: pub, FetchedAt: fetched}); !got.Equal(fetched) {
		t.Fatalf("fetched-wins got %v want %v", got, fetched)
	}
	if got := entrySyncTime(store.Entry{Published: fetched, FetchedAt: pub}); !got.Equal(fetched) {
		t.Fatalf("published-wins got %v want %v", got, fetched)
	}
	if got := entryDisplayTime(store.Entry{Published: pub, FetchedAt: fetched}); !got.Equal(pub) {
		t.Fatalf("display published got %v want %v", got, pub)
	}
	if got := entryDisplayTime(store.Entry{FetchedAt: fetched}); !got.Equal(fetched) {
		t.Fatalf("display fallback got %v want %v", got, fetched)
	}
	if got := parseReaderUnixTime("bad"); !got.IsZero() {
		t.Fatalf("bad parse got %v", got)
	}
	if got := parseReaderUnixTime("0"); !got.IsZero() {
		t.Fatalf("zero parse got %v", got)
	}
	sec := time.Unix(1_700_000_000, 0).UTC()
	if got := parseReaderUnixTime("1700000000"); !got.Equal(sec) {
		t.Fatalf("seconds got %v want %v", got, sec)
	}
	ms := time.UnixMilli(1_700_000_000_123).UTC()
	if got := parseReaderUnixTime("1700000000123"); !got.Equal(ms) {
		t.Fatalf("millis got %v want %v", got, ms)
	}
	us := time.UnixMicro(1_700_000_000_123_456).UTC()
	if got := parseReaderUnixTime("1700000000123456"); !got.Equal(us) {
		t.Fatalf("micros got %v want %v", got, us)
	}
}

func TestUnknownFeedHashStillRenders(t *testing.T) {
	srv, _, _, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{{XMLURL: "https://known/feed"}}
	o, _ := op.Load()
	items := srv.toStreamItems([]store.Entry{
		{Hash: "h1", FeedHash: "unknown", Title: "T"},
	}, o)
	if len(items) != 1 {
		t.Fatal(items)
	}
	// Unknown-hash entry gets an empty origin streamID.
	if items[0].Origin.StreamID != "feed/" {
		t.Fatalf("origin=%q", items[0].Origin.StreamID)
	}
}

func TestToStreamItemsNilOPML(t *testing.T) {
	srv, _, _, _, _ := fixture(t)
	items := srv.toStreamItems([]store.Entry{{Hash: "h", Title: "T"}}, nil)
	if len(items) != 1 {
		t.Fatal(items)
	}
}

// brokenResponseWriter fails on Write.
type brokenResponseWriter struct{ h http.Header }

func (b *brokenResponseWriter) Header() http.Header { return b.h }
func (b *brokenResponseWriter) Write(p []byte) (int, error) {
	return 0, &fakeErr{}
}
func (b *brokenResponseWriter) WriteHeader(int) {}

type fakeErr struct{}

func (fakeErr) Error() string { return "fake" }

func TestWriteJSONEncodeError(t *testing.T) {
	w := &brokenResponseWriter{h: http.Header{}}
	writeJSON(w, map[string]any{"x": 1})
}

func TestCollectStreamEmpty(t *testing.T) {
	srv, _, _, op, _ := fixture(t)
	es, err := srv.collectStream("feed/none")
	if err != nil || len(es) != 0 {
		t.Fatalf("es=%v err=%v", es, err)
	}
	_ = op
}

func TestStreamLabelFilterSkip(t *testing.T) {
	// Two feeds, one in folder Cat and one in folder Dog. Listing stream
	// label/Cat must skip the Dog feed (covers the `if !filter(f)` body).
	_, mux, tok, op, st := fixture(t)
	seedFeed(t, op, st, 1, "Cat")
	seedFeed(t, op, st, 1, "Dog")
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/user/-/label/Cat", tok, nil)
	var resp streamResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d", len(resp.Items))
	}
}

func TestUnreadCountSomeRead(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 3, "F")
	es, _ := st.ListEntries(store.FeedHash(u))
	st.SetRead(es[0].Hash, true)
	w := do(t, mux, "GET", "/reader/api/0/unread-count", tok, nil)
	if !strings.Contains(w.Body.String(), `"count":2`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestItemsIDsClampN(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	seedFeed(t, op, st, 2, "F")
	// n > entries → clamped down to len.
	w := do(t, mux, "GET", "/reader/api/0/stream/items/ids?n=50", tok, nil)
	if !strings.Contains(w.Body.String(), `"itemRefs"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestSummaryFallbackWhenContentEmpty(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := "https://no-content.example/feed"
	op.opml.Feeds = []store.Feed{{XMLURL: u, Title: "X"}}
	fh := store.FeedHash(u)
	now := time.Now().UTC()
	st.AppendEntries(fh, []store.Entry{
		{GUID: "g", Link: "https://no-content.example/1", Title: "T", Summary: "summary-only", Published: now, FetchedAt: now},
	})
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+u, tok, nil)
	if !strings.Contains(w.Body.String(), "summary-only") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

// Force SetRead/SetStarred to fail by chmod 0500 on data dir.
func TestEditTagAndMarkAllSetReadFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 1, "F")
	es, _ := st.ListEntries(store.FeedHash(u))
	// Seed in-memory flags so the "remove" path has something to clear.
	st.SetRead(es[0].Hash, true)
	// Make read.log + starred.log read-only so subsequent appends fail.
	if err := os.Chmod(filepath.Join(st.Dir, "read.log"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(st.Dir, "read.log"), 0o644) })
	if err := os.Chmod(st.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(st.Dir, 0o755) })
	// edit-tag: add starred → SetStarred fails (starred.log doesn't exist,
	// O_CREATE in chmod-0500 dir → EACCES).
	body2 := url.Values{"i": {itemID(es[0].Hash)}, "a": {stateStarredID}}
	if w := do(t, mux, "POST", "/reader/api/0/edit-tag", tok, body2); w.Code != 500 {
		t.Fatalf("add star code=%d", w.Code)
	}
	// edit-tag: remove read (state is currently Read=true; SetRead(false)
	// tries to append to read.log which is 0444 → EACCES).
	body3 := url.Values{"i": {itemID(es[0].Hash)}, "r": {stateReadID}}
	if w := do(t, mux, "POST", "/reader/api/0/edit-tag", tok, body3); w.Code != 500 {
		t.Fatalf("rem read code=%d", w.Code)
	}
	// edit-tag: add read on a *different* item.
	body1 := url.Values{"i": {itemID("freshhash000000000000")}, "a": {stateReadID}}
	if w := do(t, mux, "POST", "/reader/api/0/edit-tag", tok, body1); w.Code != 500 {
		t.Fatalf("add read code=%d", w.Code)
	}
	// mark-all-as-read against the (sole) feed: its lone entry is already
	// read in memory, so we use a fresh feed with a fresh entry.
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{XMLURL: "https://other.example/feed", Title: "Other"})
	// Can't AppendEntries here — chmod blocks file creation. Use a hash
	// directly: pretend to mark-all by hitting an empty stream which now
	// fails inside ListEntries (no current.ndjson in a dir we can't
	// create). Easier: just hit the existing feed and exercise the
	// already-read continue path → returns 200.
	_ = u
}

func TestMarkAllAsReadSetReadFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 1, "F")
	if err := os.Chmod(st.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(st.Dir, 0o755) })
	body := url.Values{"s": {"feed/" + u}}
	if w := do(t, mux, "POST", "/reader/api/0/mark-all-as-read", tok, body); w.Code != 500 {
		t.Fatalf("mark-all code=%d", w.Code)
	}
}

// --- helpers ---

var errBoom = boom("boom")

type boom string

func (b boom) Error() string { return string(b) }

func TestRenameTagRejectsReservedDest(t *testing.T) {
	// rename-tag with dest=user/-/label/__untagged__ must 400 — the
	// user-visible rename op deserves feedback rather than silent drop.
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{{XMLURL: "a", Tags: []string{"old"}}}
	body := url.Values{"s": {"user/-/label/old"}, "dest": {"user/-/label/" + store.ReservedTagUntagged}}
	if w := do(t, mux, "POST", "/reader/api/0/rename-tag", tok, body); w.Code != 400 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	// Tag must not be renamed.
	if !op.opml.Feeds[0].HasTag("old") {
		t.Fatalf("rename happened anyway: %v", op.opml.Feeds[0].Tags)
	}
}

func TestSubscriptionEditDropsReserved(t *testing.T) {
	// subscribe + edit must silently drop reserved names from a= so
	// they never enter the OPML.
	_, mux, tok, op, _ := fixture(t)
	body := url.Values{
		"ac": {"subscribe"}, "s": {"feed/https://r/y"},
		"a": {"user/-/label/ok", "user/-/label/" + store.ReservedTagUntagged},
	}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, body); w.Code != 200 {
		t.Fatalf("subscribe=%d", w.Code)
	}
	if !op.opml.Feeds[0].HasTag("ok") || op.opml.Feeds[0].HasTag(store.ReservedTagUntagged) {
		t.Fatalf("reserved leaked: %v", op.opml.Feeds[0].Tags)
	}
	// Edit add path must also drop.
	edit := url.Values{
		"ac": {"edit"}, "s": {"feed/https://r/y"},
		"a": {"user/-/label/" + store.ReservedTagUntagged},
	}
	if w := do(t, mux, "POST", "/reader/api/0/subscription/edit", tok, edit); w.Code != 200 {
		t.Fatalf("edit=%d", w.Code)
	}
	if op.opml.Feeds[0].HasTag(store.ReservedTagUntagged) {
		t.Fatalf("reserved leaked on edit: %v", op.opml.Feeds[0].Tags)
	}
}

// writeFileAtomically replaces the file directly (test helper).
func writeFileAtomically(t *testing.T, p, content string) error {
	t.Helper()
	return os.WriteFile(p, []byte(content), 0o644)
}

func TestSubscriptionListMultipleTags(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X", Tags: []string{"tech", "daily"}}}
	w := do(t, mux, "GET", "/reader/api/0/subscription/list", tok, nil)
	body := w.Body.String()
	if !strings.Contains(body, "user/-/label/daily") || !strings.Contains(body, "user/-/label/tech") {
		t.Fatalf("missing categories: %s", body)
	}
}

func TestTagListUnion(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{
		{XMLURL: "a", Tags: []string{"x", "y"}},
		{XMLURL: "b", Tags: []string{"y"}},
		{XMLURL: "c"},
	}
	w := do(t, mux, "GET", "/reader/api/0/tag/list", tok, nil)
	if !strings.Contains(w.Body.String(), "user/-/label/x") || !strings.Contains(w.Body.String(), "user/-/label/y") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestRenameTag(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{{XMLURL: "a", Tags: []string{"old"}}}
	// missing both s and dest → 400
	if w := do(t, mux, "POST", "/reader/api/0/rename-tag", tok, url.Values{}); w.Code != 400 {
		t.Fatalf("missing=%d", w.Code)
	}
	// missing dest only → 400
	if w := do(t, mux, "POST", "/reader/api/0/rename-tag", tok, url.Values{"s": {"user/-/label/old"}}); w.Code != 400 {
		t.Fatalf("missing dest=%d", w.Code)
	}
	body := url.Values{"s": {"user/-/label/old"}, "dest": {"user/-/label/new"}}
	if w := do(t, mux, "POST", "/reader/api/0/rename-tag", tok, body); w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !op.opml.Feeds[0].HasTag("new") || op.opml.Feeds[0].HasTag("old") {
		t.Fatalf("not renamed: %v", op.opml.Feeds[0].Tags)
	}
	// load error
	op.loadErr = errBoom
	if w := do(t, mux, "POST", "/reader/api/0/rename-tag", tok, body); w.Code != 500 {
		t.Fatalf("load err=%d", w.Code)
	}
	op.loadErr = nil
	op.saveErr = errBoom
	if w := do(t, mux, "POST", "/reader/api/0/rename-tag", tok, body); w.Code != 500 {
		t.Fatalf("save err=%d", w.Code)
	}
}

func TestDisableTag(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{{XMLURL: "a", Tags: []string{"x", "y"}}}
	if w := do(t, mux, "POST", "/reader/api/0/disable-tag", tok, url.Values{}); w.Code != 400 {
		t.Fatalf("missing=%d", w.Code)
	}
	body := url.Values{"s": {"user/-/label/x"}}
	if w := do(t, mux, "POST", "/reader/api/0/disable-tag", tok, body); w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if op.opml.Feeds[0].HasTag("x") {
		t.Fatalf("still has x: %v", op.opml.Feeds[0].Tags)
	}
	op.loadErr = errBoom
	if w := do(t, mux, "POST", "/reader/api/0/disable-tag", tok, body); w.Code != 500 {
		t.Fatalf("load=%d", w.Code)
	}
	op.loadErr = nil
	op.saveErr = errBoom
	if w := do(t, mux, "POST", "/reader/api/0/disable-tag", tok, body); w.Code != 500 {
		t.Fatalf("save=%d", w.Code)
	}
}

// Verify a feed with multiple tags is matched by stream/contents on any
// of its labels (multi-tag membership).
func TestStreamLabelMultiTagMembership(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.MaxPage = 50
	u := seedFeed(t, op, st, 2, "primary")
	// Add a second tag to the seeded feed.
	op.opml.Feeds[0].Tags = []string{"primary", "extra"}
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/user/-/label/extra", tok, nil)
	var resp streamResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 2 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	_ = u
}

// seedFeedWithFetchedAt is like seedFeed but lets the caller control the
// FetchedAt timestamps so we can assert per-entry newestItemTimestampUsec.
// Returns the feed url and the entries (post-store, so .Hash is populated).
func seedFeedWithFetchedAt(t *testing.T, op *memOPML, st *store.Store, name string, fetched []time.Time) (string, []store.Entry) {
	t.Helper()
	u := "https://feed.example/" + name
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{XMLURL: u, Title: name, Tags: []string{name}, HTMLURL: "https://feed.example"})
	fh := store.FeedHash(u)
	es := make([]store.Entry, len(fetched))
	base := time.Now().UTC()
	for i, ft := range fetched {
		es[i] = store.Entry{
			GUID:      name + "-g" + strconv.Itoa(i),
			Link:      "https://feed.example/" + name + "/" + strconv.Itoa(i),
			Title:     name + " " + strconv.Itoa(i),
			Content:   "c",
			Summary:   "s",
			Published: base.Add(time.Duration(-i) * time.Minute),
			FetchedAt: ft,
		}
	}
	if _, err := st.AppendEntries(fh, es); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListEntries(fh)
	if err != nil {
		t.Fatal(err)
	}
	return u, got
}

// Bug fix: /reader/api/0/unread-count must emit newestItemTimestampUsec on
// every row (per-feed and the reading-list aggregate). Reeder iOS uses the
// field to decide whether its local cache is stale; without it the client
// silently shows zero unread and never calls stream/items/ids.
func TestUnreadCountIncludesNewestItemTimestampUsec(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	// Feed A: 3 entries with increasing FetchedAt.
	urlA, _ := seedFeedWithFetchedAt(t, op, st, "A", []time.Time{
		base, base.Add(1 * time.Hour), base.Add(2 * time.Hour),
	})
	// Feed B: one entry, later than any in A → should drive the global.
	urlB, _ := seedFeedWithFetchedAt(t, op, st, "B", []time.Time{
		base.Add(10 * time.Hour),
	})
	wantA := strconv.FormatInt(base.Add(2*time.Hour).UnixMicro(), 10)
	wantB := strconv.FormatInt(base.Add(10*time.Hour).UnixMicro(), 10)
	wantGlobal := wantB

	w := do(t, mux, "GET", "/reader/api/0/unread-count?output=json", tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		UnreadCounts []struct {
			ID                      string `json:"id"`
			Count                   int    `json:"count"`
			NewestItemTimestampUsec string `json:"newestItemTimestampUsec"`
		} `json:"unreadcounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	got := map[string]string{}
	gotCount := map[string]int{}
	for _, r := range resp.UnreadCounts {
		got[r.ID] = r.NewestItemTimestampUsec
		gotCount[r.ID] = r.Count
		if r.NewestItemTimestampUsec == "" {
			t.Errorf("row %q has empty newestItemTimestampUsec", r.ID)
		}
		for _, c := range r.NewestItemTimestampUsec {
			if c < '0' || c > '9' {
				t.Errorf("row %q newestItemTimestampUsec=%q not all digits", r.ID, r.NewestItemTimestampUsec)
				break
			}
		}
	}
	if got["feed/"+urlA] != wantA {
		t.Errorf("feed A: got %q want %q", got["feed/"+urlA], wantA)
	}
	if got["feed/"+urlB] != wantB {
		t.Errorf("feed B: got %q want %q", got["feed/"+urlB], wantB)
	}
	if got[streamReadingList] != wantGlobal {
		t.Errorf("reading-list: got %q want %q", got[streamReadingList], wantGlobal)
	}
	if gotCount[streamReadingList] != 4 {
		t.Errorf("reading-list count: got %d want 4", gotCount[streamReadingList])
	}
}

// Empty-feed case: a feed with no entries must still emit the field (value
// "0") so the JSON shape matches what FreshRSS-compatible clients expect.
func TestUnreadCountEmptyFeedEmitsZeroTimestamp(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{XMLURL: "https://empty.example/", Title: "E"})
	w := do(t, mux, "GET", "/reader/api/0/unread-count", tok, nil)
	body := w.Body.String()
	if !strings.Contains(body, `"newestItemTimestampUsec":"0"`) {
		t.Fatalf("expected zero-valued newestItemTimestampUsec; body=%s", body)
	}
}

// Reeder/FreshRSS clients use item categories to associate content with
// the reading list and labels; unread items should omit only the read state.
func TestStreamItemsCarryReaderCategories(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 1, "F")
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/user/-/state/com.google/reading-list", tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp streamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	cats := strings.Join(resp.Items[0].Categories, "\n")
	for _, want := range []string{streamReadingList, labelStreamID("F")} {
		if !strings.Contains(cats, want) {
			t.Fatalf("categories %v missing %q", resp.Items[0].Categories, want)
		}
	}
	for _, cat := range resp.Items[0].Categories {
		if cat == stateReadID {
			t.Fatalf("unread item should not carry read state: %v", resp.Items[0].Categories)
		}
	}
	_ = u
}

func TestItemsIDsFiltersAndDirectStreams(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 3, "F")
	es, err := st.ListEntries(store.FeedHash(u))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRead(es[0].Hash, true); err != nil {
		t.Fatal(err)
	}
	path := "/reader/api/0/stream/items/ids?s=" + url.QueryEscape("feed/"+u) + "&xt=" + url.QueryEscape(stateReadID) + "&n=10"
	w := do(t, mux, "GET", path, tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ItemRefs []struct {
			ID              string   `json:"id"`
			DirectStreamIDs []string `json:"directStreamIds"`
		} `json:"itemRefs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.ItemRefs) != 2 {
		t.Fatalf("itemRefs=%d body=%s", len(resp.ItemRefs), w.Body.String())
	}
	for _, ref := range resp.ItemRefs {
		if ref.ID == itemLongID(es[0].Hash) {
			t.Fatalf("read item leaked through xt=read filter: %s", ref.ID)
		}
		streams := strings.Join(ref.DirectStreamIDs, "\n")
		for _, want := range []string{feedStreamID(u), labelStreamID("F")} {
			if !strings.Contains(streams, want) {
				t.Fatalf("directStreamIds %v missing %q", ref.DirectStreamIDs, want)
			}
		}
	}
}

func TestItemsIDsIncludeFiltersAndOldestFirst(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 3, "F")
	es, err := st.ListEntries(store.FeedHash(u))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetStarred(es[2].Hash, true); err != nil {
		t.Fatal(err)
	}
	base := "/reader/api/0/stream/items/ids?s=" + url.QueryEscape("feed/"+u)

	starred := do(t, mux, "GET", base+"&it="+url.QueryEscape(stateStarredID), tok, nil)
	var one struct {
		ItemRefs []struct{ ID string } `json:"itemRefs"`
	}
	if err := json.Unmarshal(starred.Body.Bytes(), &one); err != nil {
		t.Fatalf("unmarshal starred: %v", err)
	}
	wantStarredID := itemLongID(es[2].Hash)
	if len(one.ItemRefs) != 1 || one.ItemRefs[0].ID != wantStarredID {
		t.Fatalf("starred refs=%+v want only %s", one.ItemRefs, wantStarredID)
	}

	oldest := do(t, mux, "GET", base+"&it="+url.QueryEscape(streamReadingList)+"&r=o&n=10", tok, nil)
	var page struct {
		ItemRefs []struct{ ID string } `json:"itemRefs"`
	}
	if err := json.Unmarshal(oldest.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal oldest: %v", err)
	}
	wantOrder := []string{itemLongID(es[2].Hash), itemLongID(es[1].Hash), itemLongID(es[0].Hash)}
	if len(page.ItemRefs) != len(wantOrder) {
		t.Fatalf("oldest refs=%d", len(page.ItemRefs))
	}
	for i, ref := range page.ItemRefs {
		if ref.ID != wantOrder[i] {
			t.Fatalf("oldest ref[%d]=%q want %q", i, ref.ID, wantOrder[i])
		}
	}

	unknown := do(t, mux, "GET", base+"&it="+url.QueryEscape("user/-/state/com.google/unknown"), tok, nil)
	var empty struct {
		ItemRefs []struct{ ID string } `json:"itemRefs"`
	}
	if err := json.Unmarshal(unknown.Body.Bytes(), &empty); err != nil {
		t.Fatalf("unmarshal unknown: %v", err)
	}
	if len(empty.ItemRefs) != 0 {
		t.Fatalf("unknown state should match nothing: %+v", empty.ItemRefs)
	}
}

func TestItemsContentsPreservesRequestOrder(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 3, "F")
	es, err := st.ListEntries(store.FeedHash(u))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{itemID(es[2].Hash), itemID(es[0].Hash), itemID(es[1].Hash)}
	body := url.Values{}
	body.Add("i", "")
	for _, id := range want {
		body.Add("i", id)
	}
	body.Add("i", want[1])
	w := do(t, mux, "POST", "/reader/api/0/stream/items/contents", tok, body)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp streamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != len(want) {
		t.Fatalf("items=%d want %d", len(resp.Items), len(want))
	}
	for i, item := range resp.Items {
		if item.ID != want[i] {
			t.Fatalf("item[%d]=%q want %q", i, item.ID, want[i])
		}
	}
}

// Bug fix: /reader/api/0/stream/items/ids must paginate via the `c=`
// continuation token. Previously the handler silently capped at MaxPage,
// stranding clients with >MaxPage unread items.
func TestItemsIDsPagination(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.MaxPage = 10
	const total = 25
	base := time.Unix(1_700_000_000, 0).UTC()
	fetched := make([]time.Time, total)
	for i := range fetched {
		fetched[i] = base.Add(time.Duration(i) * time.Minute)
	}
	seedFeedWithFetchedAt(t, op, st, "F", fetched)

	type ref struct {
		ID            string `json:"id"`
		TimestampUsec string `json:"timestampUsec"`
	}
	type page struct {
		ItemRefs     []ref  `json:"itemRefs"`
		Continuation string `json:"continuation,omitempty"`
	}

	seen := map[string]bool{}
	cont := ""
	pages := 0
	for {
		path := "/reader/api/0/stream/items/ids?s=" + streamReadingList + "&n=10"
		if cont != "" {
			path += "&c=" + cont
		}
		w := do(t, mux, "GET", path, tok, nil)
		if w.Code != 200 {
			t.Fatalf("page %d code=%d body=%s", pages, w.Code, w.Body.String())
		}
		var p page
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, r := range p.ItemRefs {
			if seen[r.ID] {
				t.Fatalf("duplicate id %s on page %d", r.ID, pages)
			}
			seen[r.ID] = true
		}
		pages++
		switch pages {
		case 1, 2:
			if len(p.ItemRefs) != 10 {
				t.Fatalf("page %d itemRefs=%d, want 10", pages, len(p.ItemRefs))
			}
			if p.Continuation == "" {
				t.Fatalf("page %d expected continuation", pages)
			}
			// Decode and sanity-check the offset.
			dec, err := base64.RawURLEncoding.DecodeString(p.Continuation)
			if err != nil {
				t.Fatalf("decode cont: %v", err)
			}
			var st struct {
				Offset int `json:"o"`
			}
			if err := json.Unmarshal(dec, &st); err != nil {
				t.Fatalf("decode cont json: %v", err)
			}
			if st.Offset != pages*10 {
				t.Fatalf("page %d offset=%d, want %d", pages, st.Offset, pages*10)
			}
		case 3:
			if len(p.ItemRefs) != 5 {
				t.Fatalf("final page itemRefs=%d, want 5", len(p.ItemRefs))
			}
			if p.Continuation != "" {
				t.Fatalf("final page should have empty continuation, got %q", p.Continuation)
			}
		}
		if p.Continuation == "" {
			break
		}
		cont = p.Continuation
		if pages > 10 {
			t.Fatal("walked too many pages — infinite loop?")
		}
	}
	if pages != 3 {
		t.Fatalf("pages=%d, want 3", pages)
	}
	if len(seen) != total {
		t.Fatalf("saw %d distinct ids, want %d", len(seen), total)
	}
}

// A `c=` token whose offset has run past the end (because entries were
// marked read between requests, say) must return empty refs and no
// continuation rather than panic on the slice.
func TestItemsIDsContinuationPastEnd(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.MaxPage = 10
	seedFeed(t, op, st, 3, "F")
	raw, _ := json.Marshal(struct {
		Offset int `json:"o"`
	}{Offset: 99})
	cont := base64.RawURLEncoding.EncodeToString(raw)
	w := do(t, mux, "GET", "/reader/api/0/stream/items/ids?s="+streamReadingList+"&c="+cont, tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var p struct {
		ItemRefs     []any  `json:"itemRefs"`
		Continuation string `json:"continuation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(p.ItemRefs) != 0 || p.Continuation != "" {
		t.Fatalf("got refs=%d cont=%q", len(p.ItemRefs), p.Continuation)
	}
}

// Asking for n bigger than MaxPage must clamp to MaxPage, not silently
// fall through to the default.
func TestItemsIDsClampsLargeN(t *testing.T) {
	srv, mux, tok, op, st := fixture(t)
	srv.MaxPage = 5
	seedFeed(t, op, st, 20, "F")
	w := do(t, mux, "GET", "/reader/api/0/stream/items/ids?s="+streamReadingList+"&n=9999", tok, nil)
	var p struct {
		ItemRefs []struct {
			ID string `json:"id"`
		} `json:"itemRefs"`
		Continuation string `json:"continuation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(p.ItemRefs) != 5 {
		t.Fatalf("got %d refs, want 5 (MaxPage)", len(p.ItemRefs))
	}
	if p.Continuation == "" {
		t.Fatal("expected continuation for clamped page")
	}
}

func TestReaderItemIDsAre16HexAndLongIDRoundTrip(t *testing.T) {
	if got := itemID("ba7fcb8d8885006e1250"); got != "tag:google.com,2005:reader/item/ba7fcb8d8885006e" {
		t.Fatalf("itemID legacy=%q", got)
	}
	if got := itemIDToHash("tag:google.com,2005:reader/item/ba7fcb8d8885006e1250"); got != "ba7fcb8d8885006e" {
		t.Fatalf("legacy tag id -> hash %q", got)
	}
	if got := itemIDToHash("tag:google.com,2005:reader/item/ba7fcb8d8885006e"); got != "ba7fcb8d8885006e" {
		t.Fatalf("current tag id -> hash %q", got)
	}
	long := itemLongID("ba7fcb8d8885006e")
	if long != "13438683621838094446" { // unsigned int64 decimal of 0xba7fcb8d8885006e
		t.Fatalf("longId=%q", long)
	}
	if got := itemIDToHash(long); got != "ba7fcb8d8885006e" {
		t.Fatalf("longId -> hash %q", got)
	}
	// Legacy clients may POST back the signed-int64 decimal form,
	// emitted by harb ≤ v0.4.12 via FormatInt. Round-trip parity.
	if got := itemIDToHash("-5008060451871457170"); got != "ba7fcb8d8885006e" {
		t.Fatalf("legacy signed longId -> hash %q", got)
	}

	_, mux, tok, op, st := fixture(t)
	u := "https://feed.example/highbit"
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{XMLURL: u, Title: "F", Tags: []string{"F"}, HTMLURL: "https://feed.example"})
	entry := store.Entry{
		Hash:      "ba7fcb8d8885006e", // top bit set; can occur only via legacy/external input now that EntryHash masks it
		FeedHash:  store.FeedHash(u),
		GUID:      "g",
		Link:      "https://feed.example/highbit/1",
		Title:     "high bit",
		Content:   "content",
		Published: time.Unix(100, 0),
		FetchedAt: time.Unix(101, 0),
	}
	if _, err := st.AppendEntries(store.FeedHash(u), []store.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	// Storage masks the top bit (StoreEntryHash), so the entry lands under
	// the masked hash and the Reader API emits the masked longId.
	storedLong := itemLongID(store.StoreEntryHash("ba7fcb8d8885006e"))
	w := do(t, mux, "GET", "/reader/api/0/stream/contents/feed/"+u, tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []struct {
			ID     string `json:"id"`
			LongID string `json:"longId"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	hexID := strings.TrimPrefix(resp.Items[0].ID, "tag:google.com,2005:reader/item/")
	if len(hexID) != store.EntryHashLen {
		t.Fatalf("id=%q hexlen=%d", resp.Items[0].ID, len(hexID))
	}
	if resp.Items[0].LongID != storedLong {
		t.Fatalf("longId=%q want %q", resp.Items[0].LongID, storedLong)
	}

	body := url.Values{"i": {storedLong}, "a": {stateReadID}}
	w = do(t, mux, "POST", "/reader/api/0/edit-tag", tok, body)
	if w.Code != 200 {
		t.Fatalf("edit long id code=%d body=%s", w.Code, w.Body.String())
	}
	if !st.EntryState("ba7fcb8d8885006e").Read {
		t.Fatal("longId edit-tag did not update canonical hash")
	}
}

// TestApplyRequestFiltersStateStreamOT pins the bug-fix behaviour: on
// state streams (s=read / s=starred) the ot/nt filter compares against
// EntryState.UpdatedAt, not entry fetch/publish time. The full client-
// shape contract lives in internal/reedercompat; this is the in-package
// unit-coverage anchor for the closure that swaps the time source.
//
// To DIFFERENTIATE the buggy "use FetchedAt" filter from the correct
// "use UpdatedAt" filter, this test deliberately constructs entries
// whose FetchedAt is FAR IN THE FUTURE relative to wall-clock now. The
// entries are then marked read (UpdatedAt ≈ now). A query with
// ot=now+30min must EXCLUDE them (UpdatedAt < ot) — but with the old
// FetchedAt-based filter would INCLUDE them (FetchedAt = now+1h > ot).
// Symmetrically a query with ot=now-30min must INCLUDE them under both
// filters; the differentiating case is the "after the mark" check.
func TestApplyRequestFiltersStateStreamOT(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := "https://feed.example/RS"
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{
		XMLURL: u, Title: "RS", Tags: []string{"RS"}, HTMLURL: "https://feed.example",
	})
	fh := store.FeedHash(u)
	now := time.Now().UTC()
	farFutureFetch := now.Add(time.Hour)
	es := []store.Entry{{
		GUID: "rs-1", Link: u + "/1", Title: "rs1",
		Published: now, FetchedAt: farFutureFetch,
	}, {
		GUID: "rs-2", Link: u + "/2", Title: "rs2",
		Published: now, FetchedAt: farFutureFetch,
	}}
	if _, err := st.AppendEntries(fh, es); err != nil {
		t.Fatal(err)
	}
	listed, err := st.ListEntries(fh)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range listed {
		if err := st.SetRead(e.Hash, true); err != nil {
			t.Fatal(err)
		}
	}
	// Pick any entry's UpdatedAt as the reference mark time.
	tMark := st.EntryState(listed[0].Hash).UpdatedAt
	// ot=after the mark but well before FetchedAt: under the fix the
	// entries are excluded (UpdatedAt < ot). Under the old fetch-time
	// filter they leak through (FetchedAt = now+1h > ot). This is the
	// regression assertion.
	after := strconv.FormatInt(tMark.Add(30*time.Minute).Unix(), 10)
	w := do(t, mux, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(streamRead)+"&ot="+after, tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"itemRefs":[]`) {
		t.Fatalf("ot=after mark on s=read must exclude entries whose state did not mutate after ot (even when FetchedAt is well after ot); body=%s", w.Body.String())
	}
	// ot=before the mark: included (state mutated after ot). Sanity.
	before := strconv.FormatInt(tMark.Add(-30*time.Minute).Unix(), 10)
	w = do(t, mux, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(streamRead)+"&ot="+before, tok, nil)
	if !strings.Contains(w.Body.String(), `"itemRefs":[{`) {
		t.Fatalf("ot=before mark must include the entries; body=%s", w.Body.String())
	}
	// nt assertions (covers the nt parse + before-check branches with
	// the state-stream time source). nt is exclusive upper bound on
	// the time-source: nt=after-mark INCLUDES (UpdatedAt < nt);
	// nt=before-mark EXCLUDES.
	ntAfter := strconv.FormatInt(tMark.Add(30*time.Minute).Unix(), 10)
	w = do(t, mux, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(streamRead)+"&nt="+ntAfter, tok, nil)
	if !strings.Contains(w.Body.String(), `"itemRefs":[{`) {
		t.Fatalf("nt=after mark must include on state stream; body=%s", w.Body.String())
	}
	ntBefore := strconv.FormatInt(tMark.Add(-30*time.Minute).Unix(), 10)
	w = do(t, mux, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(streamRead)+"&nt="+ntBefore, tok, nil)
	if !strings.Contains(w.Body.String(), `"itemRefs":[]`) {
		t.Fatalf("nt=before mark must exclude on state stream; body=%s", w.Body.String())
	}
}

// --- ETag / If-None-Match tests ---

// doWithHeaders is `do` with an extra header set step for ETag tests.
func doWithHeaders(t *testing.T, handler http.Handler, method, path, tok string, hdrs map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if tok != "" {
		r.Header.Set("Authorization", "GoogleLogin auth="+tok)
	}
	for k, v := range hdrs {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestSubscriptionListETag(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X", Tags: []string{"t"}}}
	w := do(t, mux, "GET", "/reader/api/0/subscription/list", tok, nil)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if w.Header().Get("Cache-Control") == "" {
		t.Error("no Cache-Control")
	}
	if w.Header().Get("Vary") == "" {
		t.Error("no Vary")
	}
	// INM matches → 304, empty body.
	w2 := doWithHeaders(t, mux, "GET", "/reader/api/0/subscription/list", tok, map[string]string{"If-None-Match": etag})
	if w2.Code != http.StatusNotModified {
		t.Errorf("INM match code=%d want 304; body=%s", w2.Code, w2.Body.String())
	}
	if w2.Body.Len() != 0 {
		t.Errorf("304 body must be empty, got %d bytes", w2.Body.Len())
	}
	if w2.Header().Get("ETag") != etag {
		t.Errorf("304 ETag=%q want %q", w2.Header().Get("ETag"), etag)
	}
	// OPML change → ETag changes.
	op.opml.Feeds = append(op.opml.Feeds, store.Feed{XMLURL: "https://y/feed", Title: "Y"})
	w3 := doWithHeaders(t, mux, "GET", "/reader/api/0/subscription/list", tok, map[string]string{"If-None-Match": etag})
	if w3.Code != 200 {
		t.Errorf("after OPML change code=%d want 200", w3.Code)
	}
	if got := w3.Header().Get("ETag"); got == "" || got == etag {
		t.Errorf("ETag did not change across OPML mutation: was=%q now=%q", etag, got)
	}
	// Wildcard INM → 304.
	w4 := doWithHeaders(t, mux, "GET", "/reader/api/0/subscription/list", tok, map[string]string{"If-None-Match": "*"})
	if w4.Code != http.StatusNotModified {
		t.Errorf("wildcard INM code=%d want 304", w4.Code)
	}
	// Weak ETag prefix on INM → still 304 (we accept W/ on input).
	w5 := doWithHeaders(t, mux, "GET", "/reader/api/0/subscription/list", tok, map[string]string{"If-None-Match": "W/" + w3.Header().Get("ETag")})
	if w5.Code != http.StatusNotModified {
		t.Errorf("W/ INM code=%d want 304", w5.Code)
	}
	// Multiple ETags in INM (one matches) → 304.
	w6 := doWithHeaders(t, mux, "GET", "/reader/api/0/subscription/list", tok, map[string]string{"If-None-Match": `"other", ` + w3.Header().Get("ETag")})
	if w6.Code != http.StatusNotModified {
		t.Errorf("multi-INM code=%d want 304", w6.Code)
	}
	// Non-matching INM → 200.
	w7 := doWithHeaders(t, mux, "GET", "/reader/api/0/subscription/list", tok, map[string]string{"If-None-Match": `"nonsense"`})
	if w7.Code != 200 {
		t.Errorf("non-match INM code=%d want 200", w7.Code)
	}
}

func TestTagListETag(t *testing.T) {
	_, mux, tok, op, _ := fixture(t)
	op.opml.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X", Tags: []string{"alpha"}}}
	w := do(t, mux, "GET", "/reader/api/0/tag/list", tok, nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	w2 := doWithHeaders(t, mux, "GET", "/reader/api/0/tag/list", tok, map[string]string{"If-None-Match": etag})
	if w2.Code != http.StatusNotModified {
		t.Errorf("INM match code=%d want 304", w2.Code)
	}
}

func TestUnreadCountETag(t *testing.T) {
	_, mux, tok, op, st := fixture(t)
	u := seedFeed(t, op, st, 3, "F")
	w := do(t, mux, "GET", "/reader/api/0/unread-count?output=json", tok, nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	// Re-GET → 304.
	w2 := doWithHeaders(t, mux, "GET", "/reader/api/0/unread-count?output=json", tok, map[string]string{"If-None-Match": etag})
	if w2.Code != http.StatusNotModified {
		t.Errorf("re-GET code=%d want 304", w2.Code)
	}
	// SetRead → ETag changes.
	es, _ := st.ListEntries(store.FeedHash(u))
	if err := st.SetRead(es[0].Hash, true); err != nil {
		t.Fatal(err)
	}
	w3 := doWithHeaders(t, mux, "GET", "/reader/api/0/unread-count?output=json", tok, map[string]string{"If-None-Match": etag})
	if w3.Code != 200 {
		t.Errorf("after SetRead code=%d want 200", w3.Code)
	}
	if got := w3.Header().Get("ETag"); got == "" || got == etag {
		t.Errorf("ETag did not change across SetRead: was=%q now=%q", etag, got)
	}
}

func TestUnreadCountETagLoadErr(t *testing.T) {
	// OPML.Load failure → 500, not a stray 304/200 with a half-built ETag.
	_, mux, tok, op, _ := fixture(t)
	op.loadErr = errLoad
	w := do(t, mux, "GET", "/reader/api/0/unread-count?output=json", tok, nil)
	if w.Code != 500 {
		t.Errorf("code=%d want 500", w.Code)
	}
}

var errLoad = errOPMLLoad("boom")

type errOPMLLoad string

func (e errOPMLLoad) Error() string { return string(e) }

func TestMatchesINMEdgeCases(t *testing.T) {
	// Empty INM or empty etag → false; both branches separately.
	if matchesINM("", `"a"`) {
		t.Error("empty INM should not match")
	}
	if matchesINM(`"a"`, "") {
		t.Error("empty etag should not match")
	}
}

func TestETagOPMLOnEmpty(t *testing.T) {
	// etagOPML on a fresh OPML still returns a non-empty quoted value
	// (Marshal produces a deterministic minimal document).
	op := &store.OPML{}
	if etagOPML(op) == "" {
		t.Error("empty OPML should still yield an ETag")
	}
}

func TestRequireAuthBadTokenHeader(t *testing.T) {
	_, mux, tok, _, _ := fixture(t)

	// Invalid/missing token → 401 with the bad-token header set.
	w := do(t, mux, "GET", "/reader/api/0/user-info", "bogus", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", w.Code)
	}
	if got := w.Header().Get("X-Reader-Google-Bad-Token"); got != "true" {
		t.Fatalf("X-Reader-Google-Bad-Token=%q, want %q", got, "true")
	}

	// Valid token → no bad-token header.
	w = do(t, mux, "GET", "/reader/api/0/user-info", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Reader-Google-Bad-Token"); got != "" {
		t.Fatalf("valid token carried X-Reader-Google-Bad-Token=%q, want empty", got)
	}
}
