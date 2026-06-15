// Package reader implements the Google Reader API subset spoken by
// FreshRSS-compatible clients (Reeder Classic, NetNewsWire, etc).
//
// All `/reader/*` endpoints require an API token issued by
// `/accounts/ClientLogin`. Tokens are obtained via `Authorization:
// GoogleLogin auth=<token>` or `T=<token>` form value.
package reader

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kfet/harb/internal/auth"
	"github.com/kfet/harb/internal/store"
)

// OPMLProvider lets the Reader server fetch (and atomically replace) the
// subscriptions OPML. The host (cmd/harb) supplies this so the Reader
// package stays decoupled from the file layout.
type OPMLProvider interface {
	Load() (*store.OPML, error)
	Save(*store.OPML) error
	// Update performs a serialized read-modify-write across load→mutate
	// →save, holding the provider's lock so UI and Reader mutations
	// can't clobber each other. All OPML mutators route through this.
	Update(func(*store.OPML) error) error
}

// Server is the Reader API HTTP surface. Construct via New and mount with
// Routes(mux).
type Server struct {
	Store   *store.Store
	Auth    *auth.Store
	OPML    OPMLProvider
	Now     func() time.Time
	MaxPage int

	// Version is the harb build version surfaced via /status and
	// the `harbVersion` extension field on user-info responses.
	// Empty when unset; main.go wires this to harb.Version.
	Version   string
	Commit    string
	BuildDate string
}

// errAbortUpdate is a sentinel returned from an OPML.Update closure to
// abort the read-modify-write without persisting. The handler that set
// it communicates the user-facing reason out-of-band (e.g. a 400), so
// the sentinel itself is never surfaced.
var errAbortUpdate = errors.New("reader: abort opml update")

// New returns a Server with sensible defaults. All three fields are
// required.
func New(s *store.Store, a *auth.Store, opml OPMLProvider) *Server {
	return &Server{Store: s, Auth: a, OPML: opml, Now: time.Now, MaxPage: 100}
}

// Routes registers every Reader endpoint on mux. The returned handler
// wraps mux to bypass `http.ServeMux`'s URL cleaning for the
// stream/contents endpoint (whose suffix is a raw feed URL that may
// legitimately contain `//`).
func (s *Server) Routes(mux *http.ServeMux) http.Handler {
	mux.HandleFunc("/accounts/ClientLogin", s.handleClientLogin)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/reader/api/0/token", s.requireAuth(s.handleToken))
	mux.HandleFunc("/reader/api/0/user-info", s.requireAuth(s.handleUserInfo))
	mux.HandleFunc("/reader/api/0/subscription/list", s.requireAuth(s.handleSubscriptionList))
	mux.HandleFunc("/reader/api/0/subscription/edit", s.requireAuth(s.handleSubscriptionEdit))
	mux.HandleFunc("/reader/api/0/subscription/quickadd", s.requireAuth(s.handleQuickAdd))
	mux.HandleFunc("/reader/api/0/tag/list", s.requireAuth(s.handleTagList))
	mux.HandleFunc("/reader/api/0/rename-tag", s.requireAuth(s.handleRenameTag))
	mux.HandleFunc("/reader/api/0/disable-tag", s.requireAuth(s.handleDisableTag))
	mux.HandleFunc("/reader/api/0/stream/items/ids", s.requireAuth(s.handleItemsIDs))
	mux.HandleFunc("/reader/api/0/stream/items/contents", s.requireAuth(s.handleItemsContents))
	mux.HandleFunc("/reader/api/0/edit-tag", s.requireAuth(s.handleEditTag))
	mux.HandleFunc("/reader/api/0/mark-all-as-read", s.requireAuth(s.handleMarkAllRead))
	mux.HandleFunc("/reader/api/0/unread-count", s.requireAuth(s.handleUnreadCount))

	streamContents := s.requireAuth(s.handleStreamContents)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intercept stream/contents directly with the raw, un-normalised
		// path so feed-URLs with `//` survive.
		if strings.HasPrefix(r.URL.Path, "/reader/api/0/stream/contents/") {
			streamContents(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return gzipMiddleware(h)
}

// requireAuth wraps a handler with ClientLogin token verification.
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Most clients use POST with form params; ensure form is parsed.
		_ = r.ParseForm()
		tok := auth.ExtractAPIToken(r)
		if !s.Auth.CheckAPIToken(tok) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// --- handlers ---

func (s *Server) handleClientLogin(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	email := r.FormValue("Email")
	if email == "" {
		email = r.FormValue("email")
	}
	pass := r.FormValue("Passwd")
	if pass == "" {
		pass = r.FormValue("passwd")
	}
	tok, err := s.Auth.IssueAPIToken(email, pass)
	if err != nil {
		http.Error(w, "BadAuthentication", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "SID=%s\nLSID=%s\nAuth=%s\n", tok, tok, tok)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Echo the same token; some clients PUT this into T= subsequently.
	w.Write([]byte(auth.ExtractAPIToken(r)))
}

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"userId":        "1",
		"userName":      s.Auth.Cfg.Username,
		"userEmail":     s.Auth.Cfg.Username,
		"userProfileId": "1",
		"harbVersion":   s.Version,
	})
}

// handleStatus returns a small unauthenticated JSON document identifying
// the running binary. Useful for monitoring, version-pinning scripts,
// and clients that want to know who they're talking to before going
// through the ClientLogin dance. Vendor-prefixed field names keep the
// payload out of any namespace collision with the Reader API.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"name":      "Harbour RSS",
		"product":   "harb",
		"version":   s.Version,
		"commit":    s.Commit,
		"buildDate": s.BuildDate,
	})
}

// subItem is the JSON shape of one entry in subscription/list.
type subItem struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Categories []subCategory `json:"categories"`
	URL        string        `json:"url"`
	HTMLURL    string        `json:"htmlUrl,omitempty"`
	IconURL    string        `json:"iconUrl,omitempty"`
	SortID     string        `json:"sortid,omitempty"`
}
type subCategory struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func feedStreamID(url string) string   { return "feed/" + url }
func labelStreamID(name string) string { return "user/-/label/" + name }

const (
	streamReadingList = "user/-/state/com.google/reading-list"
	streamStarred     = "user/-/state/com.google/starred"
	streamRead        = "user/-/state/com.google/read"
	stateReadID       = "user/-/state/com.google/read"
	stateStarredID    = "user/-/state/com.google/starred"
)

func (s *Server) handleSubscriptionList(w http.ResponseWriter, r *http.Request) {
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := struct {
		Subscriptions []subItem `json:"subscriptions"`
	}{}
	for _, f := range op.Feeds {
		item := subItem{
			ID:      feedStreamID(f.XMLURL),
			Title:   f.Title,
			URL:     f.XMLURL,
			HTMLURL: f.HTMLURL,
		}
		for _, t := range f.Tags {
			item.Categories = append(item.Categories, subCategory{ID: labelStreamID(t), Label: t})
		}
		out.Subscriptions = append(out.Subscriptions, item)
	}
	s.serveConditionalJSON(w, r, etagOPML(op), out)
}

func (s *Server) handleSubscriptionEdit(w http.ResponseWriter, r *http.Request) {
	streamID := r.FormValue("s")
	url := strings.TrimPrefix(streamID, "feed/")
	if url == "" {
		http.Error(w, "missing s", http.StatusBadRequest)
		return
	}
	ac := r.FormValue("ac")
	stripLabels := func(vals []string) []string {
		out := make([]string, 0, len(vals))
		for _, v := range vals {
			v = strings.TrimPrefix(v, "user/-/label/")
			if v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	var badReq string
	err := s.OPML.Update(func(op *store.OPML) error {
		switch ac {
		case "subscribe":
			title := r.FormValue("t")
			if title == "" {
				title = url
			}
			tags := stripLabels(r.Form["a"])
			op.Add(store.Feed{XMLURL: url, Title: title, Tags: tags})
		case "unsubscribe":
			op.Remove(url)
		case "edit":
			f := op.Find(url)
			if f == nil {
				badReq = "not found"
				return errAbortUpdate
			}
			if t := r.FormValue("t"); t != "" {
				f.Title = t
			}
			for _, t := range stripLabels(r.Form["a"]) {
				f.AddTag(t)
			}
			for _, t := range stripLabels(r.Form["r"]) {
				f.RemoveTag(t)
			}
		default:
			badReq = "bad ac"
			return errAbortUpdate
		}
		return nil
	})
	if badReq != "" {
		http.Error(w, badReq, http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeText(w, "OK")
}

func (s *Server) handleQuickAdd(w http.ResponseWriter, r *http.Request) {
	url := r.FormValue("quickadd")
	if url == "" {
		http.Error(w, "missing quickadd", http.StatusBadRequest)
		return
	}
	if err := s.OPML.Update(func(op *store.OPML) error {
		op.Add(store.Feed{XMLURL: url, Title: url})
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"query":      url,
		"numResults": 1,
		"streamId":   feedStreamID(url),
	})
}

func (s *Server) handleTagList(w http.ResponseWriter, r *http.Request) {
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type tag struct {
		ID   string `json:"id"`
		Type string `json:"type,omitempty"`
	}
	out := struct {
		Tags []tag `json:"tags"`
	}{
		Tags: []tag{
			{ID: stateStarredID},
			{ID: stateReadID},
		},
	}
	for _, n := range op.AllTags() {
		out.Tags = append(out.Tags, tag{ID: labelStreamID(n), Type: "folder"})
	}
	s.serveConditionalJSON(w, r, etagOPML(op), out)
}

// handleRenameTag implements `/reader/api/0/rename-tag`: rewrites every
// feed's Tags so that `s=user/-/label/<old>` becomes
// `dest=user/-/label/<new>`. Missing or malformed params → 400; an
// unknown source tag is a no-op + 200 (idempotent rename semantics).
func (s *Server) handleRenameTag(w http.ResponseWriter, r *http.Request) {
	oldName := strings.TrimPrefix(r.FormValue("s"), "user/-/label/")
	newName := strings.TrimPrefix(r.FormValue("dest"), "user/-/label/")
	if oldName == "" || newName == "" {
		http.Error(w, "missing s/dest", http.StatusBadRequest)
		return
	}
	if store.IsReservedTag(newName) {
		http.Error(w, "reserved tag name", http.StatusBadRequest)
		return
	}
	if err := s.OPML.Update(func(op *store.OPML) error {
		op.RenameTag(oldName, newName)
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeText(w, "OK")
}

// handleDisableTag implements `/reader/api/0/disable-tag`: drops the
// given tag from every feed. The feeds themselves remain subscribed.
func (s *Server) handleDisableTag(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.FormValue("s"), "user/-/label/")
	if name == "" {
		http.Error(w, "missing s", http.StatusBadRequest)
		return
	}
	if err := s.OPML.Update(func(op *store.OPML) error {
		op.DisableTag(name)
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeText(w, "OK")
}

// streamItem is one item in a stream/contents response.
type streamItem struct {
	ID            string        `json:"id"`
	LongID        string        `json:"longId"`
	Categories    []string      `json:"categories"`
	Title         string        `json:"title"`
	Published     int64         `json:"published"`
	Updated       int64         `json:"updated"`
	CrawlTimeMsec string        `json:"crawlTimeMsec"`
	TimestampUsec string        `json:"timestampUsec"`
	Author        string        `json:"author,omitempty"`
	Alternate     []streamLink  `json:"alternate"`
	Summary       streamContent `json:"summary"`
	Origin        streamOrigin  `json:"origin"`
}
type streamLink struct {
	HREF string `json:"href"`
	Type string `json:"type,omitempty"`
}
type streamContent struct {
	Direction string `json:"direction,omitempty"`
	Content   string `json:"content"`
}
type streamOrigin struct {
	StreamID string `json:"streamId"`
	Title    string `json:"title,omitempty"`
	HTMLURL  string `json:"htmlUrl,omitempty"`
}
type streamResponse struct {
	ID           string       `json:"id"`
	Updated      int64        `json:"updated"`
	Title        string       `json:"title,omitempty"`
	Items        []streamItem `json:"items"`
	Continuation string       `json:"continuation,omitempty"`
}

func itemID(hash string) string { return "tag:google.com,2005:reader/item/" + readerItemHex(hash) }

func itemLongID(hash string) string {
	h := readerItemHex(hash)
	n, err := strconv.ParseUint(h, 16, 64)
	if err != nil {
		return "0"
	}
	// Google Reader item ids are unsigned 64-bit decimals on the wire.
	// Reeder (and likely other strict clients) silently drop items whose
	// `id` / `longId` parse as negative signed decimals, which is why a
	// roughly-half-of-items display gap appears: feeds whose sha1-derived
	// 16-hex hashes start with 8..f map to negative int64 and get culled.
	return strconv.FormatUint(n, 10)
}

func readerItemHex(hash string) string { return store.CanonicalEntryHash(hash) }

func itemIDToHash(id string) string {
	if strings.HasPrefix(id, "tag:google.com,2005:reader/item/") {
		id = strings.TrimPrefix(id, "tag:google.com,2005:reader/item/")
	}
	// Decimal forms: unsigned (current; emitted by itemLongID) or signed
	// (legacy / older Reader clients). Try unsigned first, then signed.
	if n, err := strconv.ParseUint(id, 10, 64); err == nil {
		return fmt.Sprintf("%016x", n)
	}
	if n, err := strconv.ParseInt(id, 10, 64); err == nil {
		return fmt.Sprintf("%016x", uint64(n))
	}
	return store.CanonicalEntryHash(id)
}

func (s *Server) handleStreamContents(w http.ResponseWriter, r *http.Request) {
	streamID := strings.TrimPrefix(r.URL.Path, "/reader/api/0/stream/contents/")
	items, err := s.collectStream(streamID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items = s.applyRequestFilters(items, r, streamID)
	s.sortForRequest(items, r)
	s.writeStreamPage(w, streamID, items, r)
}

func (s *Server) handleItemsContents(w http.ResponseWriter, r *http.Request) {
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	wantOrder := []string{}
	want := map[string]bool{}
	for _, id := range r.Form["i"] {
		h := itemIDToHash(id)
		if h == "" || want[h] {
			continue
		}
		want[h] = true
		wantOrder = append(wantOrder, h)
	}
	if len(wantOrder) == 0 {
		writeJSON(w, streamResponse{
			ID:      streamReadingList,
			Updated: s.Now().Unix(),
			Items:   []streamItem{},
		})
		return
	}
	found := map[string]store.Entry{}
	for h := range want {
		if e, ok := s.Store.EntryByHash(h); ok {
			found[h] = e
		}
	}
	entries := make([]store.Entry, 0, len(wantOrder))
	for _, h := range wantOrder {
		if e, ok := found[h]; ok {
			entries = append(entries, e)
		}
	}
	writeJSON(w, streamResponse{
		ID:      streamReadingList,
		Updated: s.Now().Unix(),
		Items:   s.toStreamItems(entries, op),
	})
}

// collectStream gathers entries for a stream id. Only the common stream
// kinds are supported; unknown ids resolve to "all entries".
func (s *Server) collectStream(streamID string) ([]store.Entry, error) {
	op, err := s.OPML.Load()
	if err != nil {
		return nil, err
	}
	var entries []store.Entry
	gather := func(filter func(store.Feed) bool) {
		for _, f := range op.Feeds {
			if !filter(f) {
				continue
			}
			fh := store.FeedHash(f.XMLURL)
			entries = append(entries, s.Store.IndexedEntries(fh)...)
		}
	}
	switch {
	case strings.HasPrefix(streamID, "feed/"):
		url := strings.TrimPrefix(streamID, "feed/")
		gather(func(f store.Feed) bool { return f.XMLURL == url })
	case strings.HasPrefix(streamID, "user/-/label/"):
		tag := strings.TrimPrefix(streamID, "user/-/label/")
		gather(func(f store.Feed) bool { return f.HasTag(tag) })
	case streamID == streamStarred:
		gather(func(store.Feed) bool { return true })
		entries = filterEntries(entries, func(e store.Entry) bool {
			return s.Store.EntryState(e.Hash).Starred
		})
	case streamID == streamRead:
		gather(func(store.Feed) bool { return true })
		entries = filterEntries(entries, func(e store.Entry) bool {
			return s.Store.EntryState(e.Hash).Read
		})
	default:
		// reading-list and everything else: all entries. Unread-only
		// views are expressed as reading-list/feed streams plus
		// xt=user/-/state/com.google/read. Reeder relies on this Google
		// Reader convention when seeding a fresh account.
		gather(func(store.Feed) bool { return true })
	}
	sort.Slice(entries, func(i, j int) bool {
		return entrySyncLess(entries[j], entries[i])
	})
	return entries, nil
}

func filterEntries(es []store.Entry, ok func(store.Entry) bool) []store.Entry {
	out := es[:0]
	for _, e := range es {
		if ok(e) {
			out = append(out, e)
		}
	}
	return out
}

func (s *Server) applyRequestFilters(es []store.Entry, r *http.Request, streamID string) []store.Entry {
	includes := r.Form["it"]
	excludes := r.Form["xt"]
	var after, before time.Time
	if v := r.FormValue("ot"); v != "" {
		after = parseReaderUnixTime(v)
	}
	if v := r.FormValue("nt"); v != "" {
		before = parseReaderUnixTime(v)
	}
	if len(includes) == 0 && len(excludes) == 0 && after.IsZero() && before.IsZero() {
		return es
	}
	// For state streams (read / starred) the GReader contract for ot/nt
	// is "items whose READ STATE changed in the window", not "items
	// fetched in the window". Reeder relies on this to incrementally
	// sync read flags — comparing against entry fetch/publish time
	// re-streams every recently-polled item on every sync and clobbers
	// the client's own unread display. For content streams the
	// publish/fetch time semantics are correct and unchanged.
	timeOf := entrySyncTime
	if streamID == streamRead || streamID == streamStarred {
		timeOf = func(e store.Entry) time.Time {
			return s.Store.EntryState(e.Hash).UpdatedAt
		}
	}
	return filterEntries(es, func(e store.Entry) bool {
		for _, inc := range includes {
			if !s.entryHasState(e, inc) {
				return false
			}
		}
		for _, exc := range excludes {
			if s.entryHasState(e, exc) {
				return false
			}
		}
		t := timeOf(e)
		if !after.IsZero() && !t.After(after) {
			return false
		}
		if !before.IsZero() && !t.Before(before) {
			return false
		}
		return true
	})
}

// entrySyncTime is the GReader stream cursor / sort key: the time the item
// became visible to a syncing client (max of fetch and publish). Using fetch
// time for backdated items keeps timestampUsec monotonic with arrival so
// incremental clients (Reeder) see late-fetched, old-dated items as new.
func entrySyncTime(e store.Entry) time.Time {
	if e.FetchedAt.After(e.Published) {
		return e.FetchedAt
	}
	return e.Published
}

// entrySyncLess orders entries for GReader streams: primary key is the sync
// time (arrival), with Published then Hash as deterministic tiebreakers so
// pagination offsets stay stable when many items share one poll-cycle fetch
// time. Newest-first callers pass arguments reversed.
func entrySyncLess(a, b store.Entry) bool {
	sa, sb := entrySyncTime(a), entrySyncTime(b)
	if !sa.Equal(sb) {
		return sa.Before(sb)
	}
	if !a.Published.Equal(b.Published) {
		return a.Published.Before(b.Published)
	}
	return a.Hash < b.Hash
}

func entryDisplayTime(e store.Entry) time.Time {
	if !e.Published.IsZero() {
		return e.Published
	}
	return e.FetchedAt
}

func parseReaderUnixTime(v string) time.Time {
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil || i <= 0 {
		return time.Time{}
	}
	// Google Reader docs specify seconds for ot/nt, but some clients use
	// millisecond or microsecond epoch values on adjacent endpoints. Accept
	// all three so a timestamp unit mismatch cannot make Reeder's read-state
	// delta query return an empty or wildly over-broad page.
	switch {
	case i >= 1_000_000_000_000_000:
		return time.UnixMicro(i).UTC()
	case i >= 1_000_000_000_000:
		return time.UnixMilli(i).UTC()
	default:
		return time.Unix(i, 0).UTC()
	}
}

func (s *Server) entryHasState(e store.Entry, state string) bool {
	switch state {
	case streamReadingList:
		return true
	case stateReadID:
		return s.Store.EntryState(e.Hash).Read
	case stateStarredID:
		return s.Store.EntryState(e.Hash).Starred
	default:
		return false
	}
}

func (s *Server) sortForRequest(es []store.Entry, r *http.Request) {
	if r.FormValue("r") != "o" {
		return
	}
	sort.Slice(es, func(i, j int) bool {
		return entrySyncLess(es[i], es[j])
	})
}

func directStreamsByFeed(op *store.OPML) map[string][]string {
	out := map[string][]string{}
	for _, f := range op.Feeds {
		ids := []string{feedStreamID(f.XMLURL)}
		for _, tag := range f.Tags {
			ids = append(ids, labelStreamID(tag))
		}
		out[store.FeedHash(f.XMLURL)] = ids
	}
	return out
}

// writeStreamPage paginates entries and writes a streamResponse.
func (s *Server) writeStreamPage(w http.ResponseWriter, streamID string, entries []store.Entry, r *http.Request) {
	op, _ := s.OPML.Load()
	n := s.MaxPage
	if v := r.FormValue("n"); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 && i < s.MaxPage {
			n = i
		}
	}
	offset := 0
	if c := r.FormValue("c"); c != "" {
		if dec, err := base64.RawURLEncoding.DecodeString(c); err == nil {
			var st struct {
				Offset int `json:"o"`
			}
			if json.Unmarshal(dec, &st) == nil && st.Offset > 0 {
				offset = st.Offset
			}
		}
	}
	if offset > len(entries) {
		offset = len(entries)
	}
	hi := offset + n
	cont := ""
	if hi < len(entries) {
		raw, _ := json.Marshal(struct {
			Offset int `json:"o"`
		}{Offset: hi})
		cont = base64.RawURLEncoding.EncodeToString(raw)
	} else if hi > len(entries) {
		hi = len(entries)
	}
	page := entries[offset:hi]
	resp := streamResponse{
		ID:           streamID,
		Updated:      s.Now().Unix(),
		Items:        s.toStreamItems(page, op),
		Continuation: cont,
	}
	writeJSON(w, resp)
}

func (s *Server) toStreamItems(es []store.Entry, op *store.OPML) []streamItem {
	out := make([]streamItem, 0, len(es))
	feedTitle := map[string]string{}
	feedHTML := map[string]string{}
	feedURL := map[string]string{}
	feedTags := map[string][]string{}
	if op != nil {
		for _, f := range op.Feeds {
			fh := store.FeedHash(f.XMLURL)
			feedTitle[fh] = f.Title
			feedHTML[fh] = f.HTMLURL
			feedURL[fh] = f.XMLURL
			feedTags[fh] = append([]string{}, f.Tags...)
		}
	}
	for _, e := range es {
		st := s.Store.EntryState(e.Hash)
		cats := []string{streamReadingList}
		for _, tag := range feedTags[e.FeedHash] {
			cats = append(cats, labelStreamID(tag))
		}
		if st.Read {
			cats = append(cats, stateReadID)
		}
		if st.Starred {
			cats = append(cats, stateStarredID)
		}
		displayTime := entryDisplayTime(e)
		ts := displayTime.Unix()
		body := e.Content
		if body == "" {
			body = e.Summary
		}
		out = append(out, streamItem{
			ID:            itemID(e.Hash),
			LongID:        itemLongID(e.Hash),
			Categories:    cats,
			Title:         e.Title,
			Published:     ts,
			Updated:       ts,
			CrawlTimeMsec: strconv.FormatInt(e.FetchedAt.UnixMilli(), 10),
			TimestampUsec: strconv.FormatInt(entrySyncTime(e).UnixMicro(), 10),
			Author:        e.Author,
			Alternate:     []streamLink{{HREF: e.Link, Type: "text/html"}},
			Summary:       streamContent{Content: body},
			Origin: streamOrigin{
				StreamID: feedStreamID(feedURL[e.FeedHash]),
				Title:    feedTitle[e.FeedHash],
				HTMLURL:  feedHTML[e.FeedHash],
			},
		})
	}
	return out
}

// handleItemsIDs returns just the item ids for a stream (used to seed
// "what's new" queries). Supports the `c=` continuation token in the same
// shape as writeStreamPage, so clients with more than MaxPage unread items
// can walk the full set.
func (s *Server) handleItemsIDs(w http.ResponseWriter, r *http.Request) {
	streamID := r.FormValue("s")
	if streamID == "" {
		streamID = streamReadingList
	}
	entries, err := s.collectStream(streamID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	entries = s.applyRequestFilters(entries, r, streamID)
	s.sortForRequest(entries, r)
	directStreams := map[string][]string{}
	if op, err := s.OPML.Load(); err == nil {
		directStreams = directStreamsByFeed(op)
	}
	n := s.MaxPage
	if v := r.FormValue("n"); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			if i > s.MaxPage {
				i = s.MaxPage
			}
			n = i
		}
	}
	offset := 0
	if c := r.FormValue("c"); c != "" {
		if dec, err := base64.RawURLEncoding.DecodeString(c); err == nil {
			var st struct {
				Offset int `json:"o"`
			}
			if json.Unmarshal(dec, &st) == nil && st.Offset > 0 {
				offset = st.Offset
			}
		}
	}
	if offset > len(entries) {
		offset = len(entries)
	}
	hi := offset + n
	cont := ""
	if hi < len(entries) {
		raw, _ := json.Marshal(struct {
			Offset int `json:"o"`
		}{Offset: hi})
		cont = base64.RawURLEncoding.EncodeToString(raw)
	} else if hi > len(entries) {
		hi = len(entries)
	}
	type ref struct {
		ID            string   `json:"id"`
		LongID        string   `json:"longId"`
		DirectStreams []string `json:"directStreamIds,omitempty"`
		TimestampUsec string   `json:"timestampUsec"`
	}
	out := struct {
		ItemRefs     []ref  `json:"itemRefs"`
		Continuation string `json:"continuation,omitempty"`
	}{ItemRefs: []ref{}, Continuation: cont}
	for _, e := range entries[offset:hi] {
		longID := itemLongID(e.Hash)
		out.ItemRefs = append(out.ItemRefs, ref{
			// The Google Reader stream/items/ids contract uses the signed
			// decimal item id here. Reeder matches these refs against the
			// later stream/items/contents payload via longId / parsed item
			// id; returning tag-form ids here makes the client lose unread
			// membership and show freshly fetched entries as already read.
			ID:            longID,
			LongID:        longID,
			DirectStreams: directStreams[e.FeedHash],
			TimestampUsec: strconv.FormatInt(entrySyncTime(e).UnixMicro(), 10),
		})
	}
	writeJSON(w, out)
}

// handleEditTag toggles read / starred on items.
func (s *Server) handleEditTag(w http.ResponseWriter, r *http.Request) {
	ids := r.Form["i"]
	if len(ids) == 0 {
		http.Error(w, "missing i", http.StatusBadRequest)
		return
	}
	add := r.Form["a"]
	rem := r.Form["r"]
	apply := func(state string, on bool) error {
		for _, id := range ids {
			h := itemIDToHash(id)
			switch state {
			case stateReadID:
				if err := s.Store.SetRead(h, on); err != nil {
					return err
				}
			case stateStarredID:
				if err := s.Store.SetStarred(h, on); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, a := range add {
		if err := apply(a, true); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	for _, rr := range rem {
		if err := apply(rr, false); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeText(w, "OK")
}

// handleMarkAllRead marks every entry older-than-ts in a stream as read.
func (s *Server) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	streamID := r.FormValue("s")
	if streamID == "" {
		http.Error(w, "missing s", http.StatusBadRequest)
		return
	}
	var cutoff time.Time
	if ts := r.FormValue("ts"); ts != "" {
		if usec, err := strconv.ParseInt(ts, 10, 64); err == nil {
			cutoff = time.UnixMicro(usec).UTC()
		}
	}
	entries, err := s.collectStream(streamID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, e := range entries {
		if !cutoff.IsZero() && entrySyncTime(e).After(cutoff) {
			continue
		}
		if err := s.Store.SetRead(e.Hash, true); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeText(w, "OK")
}

// handleUnreadCount returns per-feed unread counts + an overall reading-
// list total.
func (s *Server) handleUnreadCount(w http.ResponseWriter, r *http.Request) {
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Early INM check — skip the full per-feed unread scan on 304.
	if s.applyETag(w, r, etagOPMLState(op, s.Store.StateVersion())) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	type uc struct {
		ID                      string `json:"id"`
		Count                   int    `json:"count"`
		NewestItemTimestampUsec string `json:"newestItemTimestampUsec"`
	}
	out := struct {
		Max          int  `json:"max"`
		UnreadCounts []uc `json:"unreadcounts"`
	}{Max: 1000}
	total := 0
	var globalNewest int64
	for _, f := range op.Feeds {
		fh := store.FeedHash(f.XMLURL)
		es := s.Store.IndexedEntries(fh)
		count := 0
		var newest int64
		for _, e := range es {
			if s.Store.EntryState(e.Hash).Read {
				continue
			}
			count++
			if ts := e.FetchedAt.UnixMicro(); ts > newest {
				newest = ts
			}
		}
		if newest > globalNewest {
			globalNewest = newest
		}
		total += count
		out.UnreadCounts = append(out.UnreadCounts, uc{
			ID:                      feedStreamID(f.XMLURL),
			Count:                   count,
			NewestItemTimestampUsec: strconv.FormatInt(newest, 10),
		})
	}
	out.UnreadCounts = append(out.UnreadCounts, uc{
		ID:                      streamReadingList,
		Count:                   total,
		NewestItemTimestampUsec: strconv.FormatInt(globalNewest, 10),
	})
	writeJSON(w, out)
}

// --- helpers ---

// opmlFingerprint returns a short, deterministic fingerprint of the
// OPML's logical content. Used as the validator for state-independent
// reader endpoints (`subscription/list`, `tag/list`). OPML.Marshal is
// deterministic (feeds sorted by title then URL), so equal OPML →
// equal fingerprint across processes.
func opmlFingerprint(o *store.OPML) string {
	b, err := o.Marshal()
	if err != nil {
		// Marshal only fails on encoding bugs; degrade to a unique
		// per-call value so the ETag never matches (forces 200).
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// etagOPML returns the ETag value (already quoted) for an OPML-only
// validator.
func etagOPML(o *store.OPML) string {
	fp := opmlFingerprint(o)
	if fp == "" {
		return ""
	}
	return `"` + fp + `"`
}

// etagOPMLState returns the ETag value for endpoints whose payload
// depends on both OPML and entry-state. The state-version is encoded
// as a Unix-microsecond decimal so equal-state → equal-ETag across
// processes (StateVersion is rebuilt from on-disk logs on Open).
func etagOPMLState(o *store.OPML, sv time.Time) string {
	fp := opmlFingerprint(o)
	if fp == "" {
		return ""
	}
	return `"` + fp + "." + strconv.FormatInt(sv.UnixMicro(), 10) + `"`
}

// applyETag sets the ETag/Cache-Control/Vary headers and returns true
// if the request's If-None-Match matches — in which case the caller
// should `w.WriteHeader(http.StatusNotModified)` and return. Empty
// etag is a no-op (sets no headers, returns false) so the caller
// falls through to a normal 200.
func (s *Server) applyETag(w http.ResponseWriter, r *http.Request, etag string) bool {
	if etag == "" {
		return false
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Authorization")
	return matchesINM(r.Header.Get("If-None-Match"), etag)
}

// serveConditionalJSON writes v as JSON, or returns 304 Not Modified
// if the request's If-None-Match matches the supplied etag. The etag
// must already be quoted ("..."); pass an empty string to skip ETag
// handling entirely. See applyETag for the header contract.
func (s *Server) serveConditionalJSON(w http.ResponseWriter, r *http.Request, etag string, v any) {
	if s.applyETag(w, r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, v)
}

// matchesINM does a textually-strict comparison of If-None-Match.
// Per RFC 7232 the value is a comma-separated list of opaque ETags
// or the bare `*`. We accept any list member that exactly equals the
// supplied etag string (the etag we emit is always strong + quoted;
// we never produce W/ "weak" etags, but accept them on input by
// stripping the prefix so a client that re-quotes an etag with W/
// still gets a 304).
func matchesINM(inm, etag string) bool {
	if inm == "" || etag == "" {
		return false
	}
	if inm == "*" {
		return true
	}
	for _, tok := range strings.Split(inm, ",") {
		tok = strings.TrimSpace(tok)
		tok = strings.TrimPrefix(tok, "W/")
		if tok == etag {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		// Tail-of-response error — log silently; nothing left to do.
		_ = err
	}
}

func writeText(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(body))
}

// Ensure error import isn't pruned by goimports during refactors.
var _ = errors.New
