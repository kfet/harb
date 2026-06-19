// Package ui implements the embedded htmx web UI.
//
// Templates are embedded via `//go:embed templates/*.html`. On startup,
// any matching files in `<configDir>/overrides/templates/*.html` are
// parsed *after* the embedded set, so user files shadow embedded ones
// by name. Likewise, `<configDir>/overrides/theme.css` (if present) is
// served at `/ui/static/theme.css` and loaded after the bundled style.
package ui

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kfet/harb/internal/auth"
	"github.com/kfet/harb/internal/config"
	"github.com/kfet/harb/internal/passkey"
	"github.com/kfet/harb/internal/store"
)

//go:embed templates/*.html
var embeddedTemplates embed.FS

//go:embed static/*
var embeddedStatic embed.FS

// OPMLProvider is the same shape used by internal/reader.
type OPMLProvider interface {
	Load() (*store.OPML, error)
	Save(*store.OPML) error
	// Update performs a serialized read-modify-write: it holds the
	// provider's lock across load→mutate→save so concurrent UI/Reader
	// mutations can't lose each other's edits. All OPML mutators go
	// through this.
	Update(func(*store.OPML) error) error
}

// Server is the htmx UI HTTP surface. Construct via New and mount with
// Routes(mux).
type Server struct {
	Store     *store.Store
	Auth      *auth.Store
	OPML      OPMLProvider
	Theme     string
	Overrides string // base config dir; "overrides/" is expected underneath
	Secure    bool   // set Secure flag on session cookies (https deployments)

	// Passkey, when non-nil and enabled, adds WebAuthn passkey login to
	// the UI alongside the password. nil disables all passkey routes and
	// hides the UI affordances.
	Passkey *passkey.Manager

	// StaticVer is appended to bundled-static URLs as a cache-busting
	// query string (?v=...). Set this to the binary's commit / version
	// at construction time so a binary upgrade automatically forces
	// browsers to re-fetch CSS / JS without users having to hard-reload.
	StaticVer string

	// Version is the harb build version. Rendered unobtrusively in
	// the base layout footer; empty hides the footer line.
	Version string

	// ConfigPath is the on-disk path to config.json. When set, the
	// UI exposes /ui/settings with a change-password form. When empty,
	// settings is hidden and the route returns 404.
	ConfigPath string

	// Previewer fetches+parses a feed for the add-feed preview page.
	// Production callers point this at a poll.Poller-backed adapter.
	// When nil, /ui/feed/new renders without preview support.
	Previewer FeedPreviewer

	// pages maps page name -> a fully-parsed template tree rooted at
	// `base`. Per-page trees keep title/content blocks isolated.
	pages map[string]*template.Template
}

// New builds a UI server. Theme defaults to "auto".
func New(s *store.Store, a *auth.Store, o OPMLProvider, theme, overrides string) (*Server, error) {
	if theme == "" {
		theme = "auto"
	}
	srv := &Server{Store: s, Auth: a, OPML: o, Theme: theme, Overrides: overrides}
	if err := srv.loadTemplates(); err != nil {
		return nil, err
	}
	return srv, nil
}

// pageNames are the per-page template files we expect to find under
// templates/ (besides base.html, which is the shared layout).
var pageNames = []string{"login", "home", "feed", "entry", "settings", "newfeed"}

// pageExtra files (beyond base.html) parsed into each page. Override in
// tests to trigger ParseFS error paths.
var pageExtra = func(name string) []string {
	return []string{"templates/base.html", "templates/" + name + ".html"}
}

func (s *Server) loadTemplates() error {
	// Gather override files (if any).
	var overrideFiles []string
	if s.Overrides != "" {
		dir := filepath.Join(s.Overrides, "overrides", "templates")
		matches, err := filepath.Glob(filepath.Join(dir, "*.html"))
		if err != nil {
			return err
		}
		overrideFiles = matches
	}
	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New(name).ParseFS(embeddedTemplates, pageExtra(name)...)
		if err != nil {
			return err
		}
		// Apply override files in stable order. ParseFiles re-parses
		// definitions on the same set, so user files shadow embedded
		// ones by name.
		if len(overrideFiles) > 0 {
			if _, err := t.ParseFiles(overrideFiles...); err != nil {
				return err
			}
		}
		pages[name] = t
	}
	// entryrow is a fragment used by toggle handlers; it lives in feed.html.
	s.pages = pages
	return nil
}

// Routes registers UI endpoints on mux and returns the same mux for
// chaining.
func (s *Server) Routes(mux *http.ServeMux) *http.ServeMux {
	mux.HandleFunc("/ui/login", s.handleLogin)
	mux.HandleFunc("/ui/logout", s.handleLogout)
	mux.HandleFunc("/ui/static/", s.handleStatic)
	mux.HandleFunc("/ui/", s.requireSession(s.handleHome))
	mux.HandleFunc("/ui/feed", s.requireSession(s.handleFeed))
	mux.HandleFunc("/ui/feed/new", s.requireSession(s.handleFeedNew))
	mux.HandleFunc("/ui/feed/add", s.requireSession(s.handleFeedAdd))
	mux.HandleFunc("/ui/feed/remove", s.requireSession(s.handleFeedRemove))
	mux.HandleFunc("/ui/feed/tag", s.requireSession(s.handleFeedTag))
	mux.HandleFunc("/ui/feed/rename", s.requireSession(s.handleFeedRename))
	mux.HandleFunc("/ui/all", s.requireSession(s.handleAllUnread))
	mux.HandleFunc("/ui/starred", s.requireSession(s.handleStarred))
	mux.HandleFunc("/ui/entry", s.requireSession(s.handleEntry))
	mux.HandleFunc("/ui/entry/read", s.requireSession(s.handleSetRead))
	mux.HandleFunc("/ui/entry/star", s.requireSession(s.handleSetStarred))
	mux.HandleFunc("/ui/mark-all-read", s.requireSession(s.handleMarkAllRead))
	mux.HandleFunc("/ui/version", s.requireSession(s.handleVersion))
	mux.HandleFunc("/ui/settings", s.requireSession(s.handleSettings))
	mux.HandleFunc("/ui/settings/passwd", s.requireSession(s.handlePasswd))
	if s.passkeyOn() {
		// Login ceremony — reachable without a session (it *is* login).
		mux.HandleFunc("/ui/webauthn/login/begin", s.handlePasskeyLoginBegin)
		mux.HandleFunc("/ui/webauthn/login/finish", s.handlePasskeyLoginFinish)
		// Registration + removal — only while authenticated.
		mux.HandleFunc("/ui/webauthn/register/begin", s.requireSession(s.handlePasskeyRegisterBegin))
		mux.HandleFunc("/ui/webauthn/register/finish", s.requireSession(s.handlePasskeyRegisterFinish))
		mux.HandleFunc("/ui/settings/passkey/remove", s.requireSession(s.handlePasskeyRemove))
	}
	return mux
}

// passkeyOn reports whether passkey support is wired and enabled.
func (s *Server) passkeyOn() bool {
	return s.Passkey != nil
}

func (s *Server) requireSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth.CheckSession(auth.SessionFromRequest(r)) {
			RelRedirect(w, r, uiBase(r)+"login", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

// uiBase returns the relative URL prefix that, when resolved against
// the effective request URI, points back at the /ui/ root. We pick
// relative references over absolute paths so the app can be served
// under any external prefix (e.g. tailscale funnel --set-path=/rss)
// without baking a base-path config knob into the server.
//
// Precondition: r.URL.Path is under /ui/. Every call site is a UI
// handler reached via Routes(), and Go's http.ServeMux redirects
// "/ui" → "/ui/" before any handler runs, so we never see a path
// without the trailing slash. Callers outside this package would
// produce nonsense ("../../" for /foo/bar) — don't.
//
// Examples (request → returned prefix → resolves to):
//
//	/ui/             → "./"   → /ui/
//	/ui/login        → "./"   → /ui/   (login is a sibling of /ui/)
//	/ui/feed/new     → "../"  → /ui/
//	/ui/settings/passwd → "../" → /ui/
//
// The prefix always ends in "/" so callers can append a path segment
// directly: uiBase(r) + "login".
func uiBase(r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.Path, "/ui/")
	depth := strings.Count(rest, "/")
	if depth == 0 {
		return "./"
	}
	return strings.Repeat("../", depth)
}

// RelRedirect writes a 3xx response whose Location header is the
// verbatim relative reference loc — leading slash forbidden. We can't
// use net/http.Redirect here because it eagerly resolves any relative
// reference against r.URL into an absolute path, which is exactly the
// rewriting we are trying to avoid. Browsers resolve a relative
// Location against the effective request URI per RFC 7231 §7.1.2, and
// that is what makes the UI work under an arbitrary path prefix.
func RelRedirect(w http.ResponseWriter, r *http.Request, loc string, code int) {
	if strings.HasPrefix(loc, "/") {
		// Defence in depth: a leading slash here would re-introduce
		// the very absolute-path bug this function exists to prevent.
		panic("ui: RelRedirect called with absolute-path Location: " + loc)
	}
	h := w.Header()
	h.Set("Location", loc)
	if _, hasType := h["Content-Type"]; !hasType && r.Method == http.MethodGet {
		h.Set("Content-Type", "text/html; charset=utf-8")
	}
	w.WriteHeader(code)
	// Match http.Redirect's tiny body for GET so curl -i users see
	// something. The href is HTML-escaped to keep template-injection-
	// flavoured payloads inert; callers only ever pass static strings
	// today, but defence is cheap.
	if r.Method == http.MethodGet {
		fmt.Fprintf(w, "<a href=\"%s\">%s</a>.\n",
			template.HTMLEscapeString(loc), http.StatusText(code))
	}
}

// --- pages ---

type baseData struct {
	Theme         string
	User          string
	ExtraCSS      string
	Error         string
	StaticVer     string // "?v=<commit>" suffix used in base.html
	Version       string // shown in the base.html footer; empty hides it
	Settings      bool   // true when /ui/settings is available
	PasswdChanged bool   // set on login page after a successful change
	Passkey       bool   // true when passkey (WebAuthn) login is enabled

	// Base is the relative URL prefix from the effective request URI
	// back up to the /ui/ root, always ending in "/". Templates prefix
	// every internal href/action/hx-* with {{.Base}} so the rendered
	// markup contains no leading-slash URLs.
	Base string

	// MainClass is an optional class attached to <main>. Used to widen
	// the main column on the entry-list pages (home, feed, all, starred)
	// so the split-panel detail view has room on wide screens.
	MainClass string

	// StateVer is the Store.StateVersion() captured at render time, as a
	// Unix-microsecond decimal. Rendered into <html data-state-ver> so
	// the client auto-refresh poll can detect when new entries or state
	// changes have landed since this page was served.
	StateVer string
}

func (s *Server) base(r *http.Request) baseData {
	d := baseData{Theme: s.Theme, StaticVer: s.StaticVer, Version: s.Version, Settings: s.ConfigPath != "", Base: uiBase(r), Passkey: s.passkeyOn(), StateVer: stateToken(s.Store.StateVersion())}
	if s.Auth.CheckSession(auth.SessionFromRequest(r)) {
		d.User = s.Auth.Cfg.Username
	}
	if s.Overrides != "" {
		if _, err := os.Stat(filepath.Join(s.Overrides, "overrides", "theme.css")); err == nil {
			d.ExtraCSS = d.Base + "static/theme.css"
		}
	}
	return d
}

// wideBase is base() with MainClass set so the entry-list pages render
// inside the wider <main> column, giving the split-panel detail view
// room on big screens.
func (s *Server) wideBase(r *http.Request) baseData {
	d := s.base(r)
	d.MainClass = "wide"
	return d
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d := s.base(r)
		d.PasswdChanged = r.URL.Query().Get("passwd") == "1"
		s.render(w, "login", struct{ baseData }{d})
	case http.MethodPost:
		_ = r.ParseForm()
		u, p := r.FormValue("username"), r.FormValue("password")
		tok, err := s.Auth.IssueSession(u, p)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			d := s.base(r)
			d.Error = "invalid credentials"
			s.render(w, "login", struct{ baseData }{d})
			return
		}
		auth.SetSessionCookie(w, tok, s.Secure)
		RelRedirect(w, r, "./", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	tok := auth.SessionFromRequest(r)
	if tok != "" {
		_ = s.Auth.RevokeSession(tok)
	}
	auth.ClearSessionCookie(w)
	RelRedirect(w, r, "login", http.StatusSeeOther)
}

// unreadCookieName persists the "show unread only" choice across feed
// pages, the home list and navigation. The value is "1" (default) or
// "0" (show all). The cookie is set by writeUnreadCookie when the
// user clicks an explicit ?unread=0/1 link; subsequent pages without
// the query param read the cookie via effectiveUnreadOnly.
const unreadCookieName = "h_unread"

// effectiveUnreadOnly returns the user's current "unread only" choice
// for list pages. Precedence: explicit ?unread=… query param first,
// then the persisted cookie, then the default (true — start filtered).
func effectiveUnreadOnly(r *http.Request) bool {
	switch r.URL.Query().Get("unread") {
	case "1":
		return true
	case "0":
		return false
	}
	if c, err := r.Cookie(unreadCookieName); err == nil && c.Value == "0" {
		return false
	}
	return true
}

// writeUnreadCookie persists the user's choice. Called from handlers
// when the request URL carries an explicit ?unread=0/1, so that the
// next plain navigation (e.g. clicking a feed in the sidebar, hitting
// `u` from an entry) keeps the same filter without a query param.
func writeUnreadCookie(w http.ResponseWriter, r *http.Request, secure bool) {
	q := r.URL.Query().Get("unread")
	if q != "0" && q != "1" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     unreadCookieName,
		Value:    q,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// tagSlug derives a stable, DOM-id-friendly slug from a tag name. Used
// for the collapse-target id (#grp-<slug>). Non-alnum runs collapse to
// '-'; the empty tag (untagged sentinel) maps to "untagged".
func tagSlug(name string) string {
	if name == "" {
		return "all"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "tag"
	}
	return s
}

type homeFeed struct {
	Title  string
	URL    string
	Hash   string // store.FeedHash(URL); stable DOM-id suffix for count-feed-<hash>
	Unread int
	Tags   []string

	// Failing is true when the feed's most recent poll left it with a
	// non-zero consecutive error count. ErrorCount / LastError /
	// LastSuccessFmt carry the detail rendered in the row's tooltip.
	Failing        bool
	ErrorCount     int
	LastError      string
	LastSuccessFmt string // formatPublished(LastSuccess) or "never"
}

// failingFeed is one row in the home-page sync-failure banner.
type failingFeed struct {
	Title string
	URL   string
}

// feedGroup is one tag-bucket rendered on the home page. A single feed
// appears in every group whose tag it carries; feeds with no tags land
// in the "Untagged" group. When a tag filter is active we still emit
// the same shape — there's just one group.
type feedGroup struct {
	Name   string // tag name; "" for untagged sentinel display
	Label  string
	Slug   string // stable CSS-id slug for the collapsible target
	Unread int
	Feeds  []homeFeed
}

// tagCount is one row in the home sidebar.
type tagCount struct {
	Name   string // tag name, or "" for "All", or store.ReservedTagUntagged for the no-tags bucket
	Label  string // display label
	Slug   string // stable DOM-id slug: "all" / "untagged" (count-side-<slug>)
	Unread int
}

// unreadCounts walks every unread entry once and returns:
//   - per-feed unread counts (keyed by feed XML URL)
//   - special bucket "" → all-unread total
//   - special bucket store.ReservedTagUntagged → untagged-feed unread total
//   - one bucket per tag name
//
// A single tag is counted multiple times for a feed only if that feed's
// Tags list contains duplicates — NormalizeTags prevents that, so this
// is a strict O(unread * tags) pass.
func (s *Server) unreadCounts(op *store.OPML) (perFeed map[string]int, buckets map[string]int) {
	perFeed = map[string]int{}
	buckets = map[string]int{}
	for _, f := range op.Feeds {
		es := s.Store.IndexedEntries(store.FeedHash(f.XMLURL))
		count := 0
		for _, e := range es {
			if !s.Store.EntryState(e.Hash).Read {
				count++
			}
		}
		perFeed[f.XMLURL] = count
		buckets[""] += count
		if len(f.Tags) == 0 {
			buckets[store.ReservedTagUntagged] += count
		}
		for _, t := range f.Tags {
			buckets[t] += count
		}
	}
	return perFeed, buckets
}

// feedStates loads the per-feed conditional-GET state for every feed in
// op, keyed by feed XML URL. Used by the home page to flag feeds whose
// most recent poll failed. A missing state file folds to the zero value
// (no error), so a never-polled feed reads as healthy.
func (s *Server) feedStates(op *store.OPML) (map[string]store.FeedState, error) {
	out := make(map[string]store.FeedState, len(op.Feeds))
	for _, f := range op.Feeds {
		fs, err := s.Store.LoadFeedState(store.FeedHash(f.XMLURL))
		if err != nil {
			return nil, err
		}
		out[f.XMLURL] = fs
	}
	return out, nil
}

// formatLastSuccess renders a feed's last successful sync time for the
// failure tooltip. Zero (never synced, or legacy state predating the
// field) renders as "never"; otherwise it reuses the relative formatter
// used on entry rows.
func formatLastSuccess(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return formatPublished(t, now)
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	// Only the exact /ui/ path is the home; reject everything else under
	// /ui/ that isn't handled explicitly.
	if r.URL.Path != "/ui/" {
		http.NotFound(w, r)
		return
	}
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	unreadOnly := effectiveUnreadOnly(r)
	writeUnreadCookie(w, r, s.Secure)
	tagFilter := r.URL.Query().Get("tag") // "" → all; store.ReservedTagUntagged → untagged
	perFeed, buckets := s.unreadCounts(op)
	states, err := s.feedStates(op)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	feeds := make([]homeFeed, 0, len(op.Feeds))
	var failing []failingFeed
	withUnread := 0
	scopedTotal := 0
	for _, f := range op.Feeds {
		count := perFeed[f.XMLURL]
		if tagFilter == store.ReservedTagUntagged {
			if len(f.Tags) != 0 {
				continue
			}
		} else if tagFilter != "" {
			if !f.HasTag(tagFilter) {
				continue
			}
		}
		// In scope: contribute to the scope-aware totals.
		scopedTotal += count
		if count > 0 {
			withUnread++
		}
		fs := states[f.XMLURL]
		failingNow := fs.ErrorCount > 0
		// Failing feeds surface in the banner regardless of the
		// unread-only filter — a feed can be 0-unread yet broken.
		if failingNow {
			failing = append(failing, failingFeed{Title: f.Title, URL: f.XMLURL})
		}
		if unreadOnly && count == 0 {
			continue
		}
		hf := homeFeed{Title: f.Title, URL: f.XMLURL, Hash: store.FeedHash(f.XMLURL), Unread: count, Tags: f.Tags}
		if failingNow {
			hf.Failing = true
			hf.ErrorCount = fs.ErrorCount
			hf.LastError = fs.LastError
			hf.LastSuccessFmt = formatLastSuccess(fs.LastSuccess, time.Now())
		}
		feeds = append(feeds, hf)
	}
	pinned := []tagCount{
		{Name: "", Label: "All", Slug: "all", Unread: buckets[""]},
		{Name: store.ReservedTagUntagged, Label: "Untagged", Slug: "untagged", Unread: buckets[store.ReservedTagUntagged]},
	}
	groups := buildFeedGroups(feeds, tagFilter)
	data := struct {
		baseData
		Feeds      []homeFeed
		Total      int
		WithUnread int
		UnreadOnly bool
		Sidebar    []tagCount // pinned rows (All / Untagged)
		TagFilter  string
		Groups     []feedGroup
		Failing    []failingFeed
	}{s.wideBase(r), feeds, scopedTotal, withUnread, unreadOnly, pinned, tagFilter, groups, failing}
	s.render(w, "home", data)
}

// buildFeedGroups buckets feeds by tag for the home page. Each feed
// shows under every tag it carries; feeds with no tags land in the
// trailing "Untagged" group. When tagFilter is set, every feed in the
// input already matches that filter, so we emit a single group with
// the filter's label.
func buildFeedGroups(feeds []homeFeed, tagFilter string) []feedGroup {
	if len(feeds) == 0 {
		return nil
	}
	if tagFilter != "" {
		label := tagFilter
		if tagFilter == store.ReservedTagUntagged {
			label = "Untagged"
		}
		g := feedGroup{Name: tagFilter, Label: label, Slug: tagSlug(label)}
		for _, f := range feeds {
			g.Feeds = append(g.Feeds, f)
			g.Unread += f.Unread
		}
		return []feedGroup{g}
	}
	// No filter: bucket by tag, with an Untagged trailer.
	byTag := map[string]*feedGroup{}
	var order []string
	for _, f := range feeds {
		if len(f.Tags) == 0 {
			g, ok := byTag[""]
			if !ok {
				g = &feedGroup{Name: store.ReservedTagUntagged, Label: "Untagged", Slug: "untagged"}
				byTag[""] = g
				order = append(order, "")
			}
			g.Feeds = append(g.Feeds, f)
			g.Unread += f.Unread
			continue
		}
		for _, t := range f.Tags {
			g, ok := byTag[t]
			if !ok {
				g = &feedGroup{Name: t, Label: t, Slug: tagSlug(t)}
				byTag[t] = g
				order = append(order, t)
			}
			g.Feeds = append(g.Feeds, f)
			g.Unread += f.Unread
		}
	}
	// Sort: real tags alpha, Untagged always last.
	sort.SliceStable(order, func(i, j int) bool {
		if order[i] == "" {
			return false
		}
		if order[j] == "" {
			return true
		}
		return strings.ToLower(order[i]) < strings.ToLower(order[j])
	})
	out := make([]feedGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *byTag[k])
	}
	return out
}

type feedEntry struct {
	Hash         string
	Title        string
	Read         bool
	Starred      bool
	FeedTitle    string // only set on cross-feed views
	Published    time.Time
	PublishedFmt string // pre-formatted for display
	// OOB marks this row to be rendered with hx-swap-oob="true" so a
	// detail-view toggle response can patch the matching list row in
	// the same swap. Zero value on normal list renders.
	OOB bool
}

// rowFor builds a feedEntry for the current state of e. Used by both
// the standalone row-toggle handler and the detail-view OOB row patch.
func rowFor(e store.Entry, st store.EntryState) feedEntry {
	return feedEntry{
		Hash:         e.Hash,
		Title:        entryTitle(e),
		Read:         st.Read,
		Starred:      st.Starred,
		Published:    e.Published,
		PublishedFmt: formatPublished(e.Published, time.Now()),
	}
}

type entryListData struct {
	baseData
	Heading     string
	Entries     []feedEntry
	ShowMarkAll bool
	Scope       string // "feed" | "all" | "starred"
	ScopeID     string // feed URL when Scope == "feed"
	UnreadOnly  bool   // when true, Entries has been filtered to unread
	UnreadCount int    // unread count for this scope (pre-filter)
	FeedTags    []string
	AllTags     []string // every tag across all feeds, for the add-tag datalist
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	urlS := r.URL.Query().Get("id")
	if urlS == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	feed := op.Find(urlS)
	if feed == nil {
		http.NotFound(w, r)
		return
	}
	es := s.Store.IndexedEntries(store.FeedHash(urlS))
	unreadOnly := effectiveUnreadOnly(r)
	writeUnreadCookie(w, r, s.Secure)
	entries := make([]feedEntry, 0, len(es))
	unreadCount := 0
	for _, e := range es {
		st := s.Store.EntryState(e.Hash)
		if !st.Read {
			unreadCount++
		}
		if unreadOnly && st.Read {
			continue
		}
		entries = append(entries, feedEntry{
			Hash:         e.Hash,
			Title:        entryTitle(e),
			Read:         st.Read,
			Starred:      st.Starred,
			Published:    e.Published,
			PublishedFmt: formatPublished(e.Published, time.Now()),
		})
	}
	data := entryListData{
		baseData:    s.wideBase(r),
		Heading:     feed.Title,
		Entries:     entries,
		ShowMarkAll: true,
		Scope:       "feed",
		ScopeID:     urlS,
		UnreadOnly:  unreadOnly,
		UnreadCount: unreadCount,
		FeedTags:    feed.Tags,
		AllTags:     op.AllTags(),
	}
	// panel=1 — render just the feed's entry-list fragment, no chrome.
	// Used by the home master-detail layout (keys.js previews the
	// selected feed's entries into #feed-pane via an htmx.ajax GET with
	// panel=1), analogous to the entry-detail panel=1 path on /ui/entry.
	if r.URL.Query().Get("panel") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = s.pages["feed"].ExecuteTemplate(w, "feedpane", data)
		return
	}
	s.render(w, "feed", data)
}

// handleAllUnread renders every unread entry across every feed, newest
// first, with a small feed-title tag on each row.
func (s *Server) handleAllUnread(w http.ResponseWriter, r *http.Request) {
	s.crossFeed(w, r, "unread", "all", func(st store.EntryState) bool { return !st.Read })
}

// handleStarred renders every starred entry across every feed.
func (s *Server) handleStarred(w http.ResponseWriter, r *http.Request) {
	s.crossFeed(w, r, "starred", "starred", func(st store.EntryState) bool { return st.Starred })
}

type entryWithFeed struct {
	entry     store.Entry
	feedTitle string
}

func (s *Server) crossFeed(w http.ResponseWriter, r *http.Request, heading, scope string, keep func(store.EntryState) bool) {
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var all []entryWithFeed
	for _, f := range op.Feeds {
		es := s.Store.IndexedEntries(store.FeedHash(f.XMLURL))
		for _, e := range es {
			if keep(s.Store.EntryState(e.Hash)) {
				all = append(all, entryWithFeed{entry: e, feedTitle: f.Title})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].entry.Published.After(all[j].entry.Published)
	})
	entries := make([]feedEntry, 0, len(all))
	for _, x := range all {
		st := s.Store.EntryState(x.entry.Hash)
		entries = append(entries, feedEntry{
			Hash:         x.entry.Hash,
			Title:        entryTitle(x.entry),
			Read:         st.Read,
			Starred:      st.Starred,
			FeedTitle:    x.feedTitle,
			Published:    x.entry.Published,
			PublishedFmt: formatPublished(x.entry.Published, time.Now()),
		})
	}
	s.render(w, "feed", entryListData{
		baseData:    s.wideBase(r),
		Heading:     heading,
		Entries:     entries,
		ShowMarkAll: scope == "all",
		Scope:       scope,
	})
}

// errAbortOPMLUpdate is a sentinel returned from an OPML.Update closure
// to abort the read-modify-write without persisting (e.g. the target
// feed was not found). The handler signals the user-facing outcome
// out-of-band, so this error is never rendered.
var errAbortOPMLUpdate = errors.New("ui: abort opml update")

// handleFeedAdd subscribes to a new feed.
func (s *Server) handleFeedAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u := strings.TrimSpace(r.FormValue("url"))
	if u == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	tags := parseTagInput(r.FormValue("tags"))
	if title == "" {
		title = u
	}
	if err := s.OPML.Update(func(op *store.OPML) error {
		op.Add(store.Feed{XMLURL: u, Title: title, Tags: tags})
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	RelRedirect(w, r, "../", http.StatusSeeOther)
}

// handleFeedTag adds or removes a single tag on a feed. POST with form
// `url` + (`add` or `remove`). Re-renders the tag-chip fragment so the
// caller (an hx-post button) can swap it in-place.
func (s *Server) handleFeedTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u := strings.TrimSpace(r.FormValue("url"))
	add := strings.TrimSpace(r.FormValue("add"))
	rem := strings.TrimSpace(r.FormValue("remove"))
	if u == "" || (add == "" && rem == "") {
		http.Error(w, "missing url and add/remove", http.StatusBadRequest)
		return
	}
	var feedTags, allTags []string
	notFound := false
	err := s.OPML.Update(func(op *store.OPML) error {
		f := op.Find(u)
		if f == nil {
			notFound = true
			return errAbortOPMLUpdate
		}
		if add != "" {
			for _, t := range parseTagInput(add) {
				f.AddTag(t)
			}
		}
		if rem != "" {
			f.RemoveTag(rem)
		}
		// Capture render inputs while we still hold the lock, from the
		// post-mutation state that is about to be persisted.
		feedTags = append([]string(nil), f.Tags...)
		allTags = op.AllTags()
		return nil
	})
	if notFound {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = s.pages["feed"].ExecuteTemplate(w, "tagchips", struct {
		ScopeID  string
		FeedTags []string
		AllTags  []string
	}{u, feedTags, allTags})
}

// handleFeedRename changes the display title of an already-subscribed
// feed. POST with form `url` + `title`. The trimmed title must be
// non-empty (400 otherwise, mirroring the tag handler). On success the
// new title is persisted to the OPML and the user is redirected back to
// the feed page so the heading — and the home feed list on its next
// load — reflect the new name.
func (s *Server) handleFeedRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u := strings.TrimSpace(r.FormValue("url"))
	title := strings.TrimSpace(r.FormValue("title"))
	if u == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	if title == "" {
		http.Error(w, "missing title", http.StatusBadRequest)
		return
	}
	notFound := false
	err := s.OPML.Update(func(op *store.OPML) error {
		f := op.Find(u)
		if f == nil {
			notFound = true
			return errAbortOPMLUpdate
		}
		f.Title = title
		return nil
	})
	if notFound {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Land back on the feed page so the renamed heading renders.
	RelRedirect(w, r, "../feed?id="+url.QueryEscape(u), http.StatusSeeOther)
}

// handleFeedRemove unsubscribes.
func (s *Server) handleFeedRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u := strings.TrimSpace(r.FormValue("url"))
	if u == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	if err := s.OPML.Update(func(op *store.OPML) error {
		op.Remove(u)
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	RelRedirect(w, r, "../", http.StatusSeeOther)
}

// handleMarkAllRead marks every entry in a given scope as read. Scopes:
// "feed" (requires id=feed-url), "all" (every unread entry across all
// feeds).
func (s *Server) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope := r.URL.Query().Get("scope")
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	mark := func(feedURL string) error {
		for _, e := range s.Store.IndexedEntries(store.FeedHash(feedURL)) {
			if s.Store.EntryState(e.Hash).Read {
				continue
			}
			if err := s.Store.SetRead(e.Hash, true); err != nil {
				return err
			}
		}
		return nil
	}
	switch scope {
	case "feed":
		u := r.URL.Query().Get("id")
		if u == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if err := mark(u); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// oob=1 (home master-detail "r" key): the caller marks the
		// feed read in place and wants the affected count badges back
		// as OOB swaps instead of a navigation, so the home page's
		// feed/group/sidebar/total counts stay consistent without a
		// reload. The pane's entry rows are refreshed by the caller.
		if r.URL.Query().Get("oob") == "1" {
			if f := op.Find(u); f != nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				s.writeCountsOOB(w, op, *f)
			}
			return
		}
		// Land back on the feed and let the user's persisted
		// "unread only" choice (or the default) decide the view.
		RelRedirect(w, r, "./", http.StatusSeeOther)
	case "all":
		for _, f := range op.Feeds {
			if err := mark(f.XMLURL); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		RelRedirect(w, r, "all", http.StatusSeeOther)
	default:
		http.Error(w, "bad scope", http.StatusBadRequest)
	}
}

// stateToken encodes a StateVersion as a Unix-microsecond decimal —
// the same encoding the Reader API uses in etagOPMLState. Equal state
// → equal token across processes (StateVersion is rebuilt from on-disk
// logs on Open). A zero StateVersion yields a large negative constant,
// which is a perfectly valid stable opaque token.
func stateToken(sv time.Time) string {
	return strconv.FormatInt(sv.UnixMicro(), 10)
}

// handleVersion is the hot path for the UI auto-refresh poll. It
// reports the current Store.StateVersion() as a Unix-microsecond
// decimal, both in the body and as the (quoted) ETag, and honours
// If-None-Match with a 304. It does no OPML load, no rendering and no
// disk reads, so clients can poll it cheaply: a matching INM short-
// circuits before the body write. We deliberately use a pure state-
// version token (not the OPML-folded reader etag) — OPML edits happen
// via full-page navs here, so the only thing the poll needs to notice
// is new entries / read-state changes, which all bump StateVersion.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	tok := stateToken(s.Store.StateVersion())
	etag := `"` + tok + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if inm := r.Header.Get("If-None-Match"); inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, tok)
}

// findEntry resolves the entry with the given hash via the in-memory
// index and pairs it with its owning feed in the current OPML. Returns
// ok=true only when both the entry is indexed AND its feed is still
// subscribed; if the entry's feed was removed from the OPML between
// fetch and now, this returns ok=false (the entry is invisible to the
// UI by design).
func (s *Server) findEntry(op *store.OPML, hash string) (store.Entry, store.Feed, bool) {
	e, ok := s.Store.EntryByHash(hash)
	if !ok {
		return store.Entry{}, store.Feed{}, false
	}
	for _, f := range op.Feeds {
		if store.FeedHash(f.XMLURL) == e.FeedHash {
			return e, f, true
		}
	}
	return store.Entry{}, store.Feed{}, false
}

// entryBody resolves the displayable HTML body of e (Content, falling
// back to Summary) and sanitizes it for safe rendering in the web UI.
//
// Feed content is attacker-controlled, so it is run through
// sanitizeHTML (an allow-list sanitizer built on golang.org/x/net/html)
// before being marked template.HTML. sanitizeHTML also reproduces the
// previous open-links-in-a-new-tab behaviour: every surviving <a> gets
// target="_blank" rel="noopener noreferrer" unless the author already
// set a target.
func entryBody(e store.Entry) template.HTML {
	body := e.Content
	// Some feeds (e.g. WordPress sites that publish a bare "Source" link
	// as content:encoded) carry no real article body in Content — just a
	// link, whitespace, or nothing. The entry view already renders a
	// separate source link, so such Content is useless as a preview; fall
	// back to the Summary, which holds the actual excerpt.
	if isLinkOnly(body) {
		body = e.Summary
	}
	return template.HTML(sanitizeHTML(body))
}

// entryDetailData is the render input for the entry-detail fragment,
// shared by the full entry page, the panel=1 split-view render, and the
// OOB patch emitted by writeEntryStateOOB. OOB marks the article with
// hx-swap-oob="true" so a toggle response can patch an open detail pane
// in place; htmx silently drops it when #entry-detail-<hash> is absent.
type entryDetailData struct {
	Entry      store.Entry
	Body       template.HTML
	Title      string
	SourceLink template.URL
	State      store.EntryState
	FeedURL    string
	FeedTitle  string
	OOB        bool
}

func (s *Server) handleEntry(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("id")
	if hash == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	e, f, ok := s.findEntry(op, hash)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dd := entryDetailData{
		Entry: e, Body: entryBody(e), Title: entryTitle(e),
		SourceLink: LinkURL(e.Link), State: s.Store.EntryState(e.Hash),
		FeedURL: f.XMLURL, FeedTitle: f.Title,
	}
	// panel=1 — render just the entry-detail fragment, no chrome.
	// Used by the split-panel layout (keys.js openEntry issues an
	// htmx.ajax GET with panel=1) so the right pane swaps in the entry
	// view without a full page load.
	if r.URL.Query().Get("panel") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.pages["entry"].ExecuteTemplate(w, "entry-detail", dd)
		return
	}
	s.render(w, "entry", struct {
		baseData
		entryDetailData
	}{s.base(r), dd})
}

func (s *Server) handleSetRead(w http.ResponseWriter, r *http.Request) {
	s.toggleFlag(w, r, true)
}
func (s *Server) handleSetStarred(w http.ResponseWriter, r *http.Request) {
	s.toggleFlag(w, r, false)
}

// countOOB is one count badge rendered standalone as an hx-swap-oob
// swap (template "countoob"). Class is "count" for the badge spots
// (feed row / group header / sidebar) and "count-inline" for the
// page total + scope pill, matching the first-render markup so the
// OOB swap targets and restyles identically.
type countOOB struct {
	ID    string
	N     int
	Class string
}

// writeEntryStateOOB emits the COMPLETE set of fragments affected by a
// single entry's read/star mutation, every one as an hx-swap-oob swap
// carrying freshly-recomputed ABSOLUTE values. The client does zero
// state math: it applies whatever fragments are present in its current
// layout and htmx silently drops the rest (absent target id → no-op).
// This is the single source of truth for the UI after a mutation.
//
// Emitted (all OOB):
//  1. the list row            (#entry-<hash>)
//  2. the detail article      (#entry-detail-<hash>) — always; dropped if absent
//  3. on READ changes only, every affected count badge with an absolute
//     value: the feed's count-feed + count-scope, each of the feed's
//     tags' count-grouphead + count-side (or the untagged pair when the
//     feed carries no tags), count-side-all and count-total.
//
// Star toggles leave unread counts unchanged, so the count badges are
// skipped for them to keep the payload tight.
func (s *Server) writeEntryStateOOB(w io.Writer, op *store.OPML, e store.Entry, f store.Feed, st store.EntryState, isRead bool) {
	// 1. list row.
	row := rowFor(e, st)
	row.OOB = true
	_ = s.pages["feed"].ExecuteTemplate(w, "entryrow", row)
	// 2. detail article (always emitted; htmx drops it when absent).
	dd := entryDetailData{
		Entry: e, Body: entryBody(e), Title: entryTitle(e),
		SourceLink: LinkURL(e.Link), State: st,
		FeedURL: f.XMLURL, FeedTitle: f.Title, OOB: true,
	}
	_ = s.pages["entry"].ExecuteTemplate(w, "entry-detail", dd)
	// 3. count badges — only read toggles move unread counts.
	if !isRead {
		return
	}
	s.writeCountsOOB(w, op, f)
}

// writeCountsOOB emits the OOB count-badge set affected by a change to
// feed f's unread total: the feed's own count-feed + count-scope, each
// of its tags' count-grouphead + count-side (or the untagged pair when
// it carries no tags), count-side-all and count-total. Values are
// absolute, recomputed from the store. Shared by the single-entry
// toggle (writeEntryStateOOB) and the whole-feed mark-all-read path.
func (s *Server) writeCountsOOB(w io.Writer, op *store.OPML, f store.Feed) {
	perFeed, buckets := s.unreadCounts(op)
	emit := func(id, class string, n int) {
		_ = s.pages["feed"].ExecuteTemplate(w, "countoob", countOOB{ID: id, N: n, Class: class})
	}
	emit("count-feed-"+store.FeedHash(f.XMLURL), "count", perFeed[f.XMLURL])
	emit("count-scope", "count-inline", perFeed[f.XMLURL])
	if len(f.Tags) == 0 {
		emit("count-grouphead-untagged", "count", buckets[store.ReservedTagUntagged])
		emit("count-side-untagged", "count", buckets[store.ReservedTagUntagged])
	} else {
		for _, t := range f.Tags {
			slug := tagSlug(t)
			emit("count-grouphead-"+slug, "count", buckets[t])
			emit("count-side-"+slug, "count", buckets[t])
		}
	}
	emit("count-side-all", "count", buckets[""])
	// count-total is always the GLOBAL unread total. On a tag-filtered
	// home (?tag=…) the first render shows the scoped total instead, so
	// an in-pane toggle there overwrites it with the global value — a
	// cosmetic edge that self-heals on the next navigation. The home
	// master-detail is normally unfiltered; we don't plumb filter scope
	// through the toggle endpoint for this.
	emit("count-total", "count-inline", buckets[""])
}

func (s *Server) toggleFlag(w http.ResponseWriter, r *http.Request, isRead bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	hash := r.URL.Query().Get("id")
	state := r.URL.Query().Get("state")
	if hash == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	on := state == "1"
	var err error
	if isRead {
		err = s.Store.SetRead(hash, on)
	} else {
		err = s.Store.SetStarred(hash, on)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	op, err := s.OPML.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	e, f, ok := s.findEntry(op, hash)
	if !ok {
		http.NotFound(w, r)
		return
	}
	st := s.Store.EntryState(hash)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Always emit the unified OOB set — row, detail, and (for read
	// changes) every affected count badge. The client applies whatever
	// is present in its layout; htmx drops the rest. No view= branching.
	s.writeEntryStateOOB(w, op, e, f, st, isRead)
}

// handleSettings renders the settings page (GET /ui/settings). Refuses
// with 404 when ConfigPath isn't wired, e.g. test setups without a
// disk-backed config.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.ConfigPath == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d := s.base(r)
	s.render(w, "settings", struct {
		baseData
		Ok       bool
		Passkeys []passkeyView
	}{d, r.URL.Query().Get("ok") == "1", s.passkeyViews()})
}

// passkeyView is the per-credential row rendered on the settings page.
type passkeyView struct {
	Label   string
	ID      string // standard-base64 credential ID, for the remove form
	AddedAt string
}

// passkeyViews returns the registered credentials in display form, or nil
// when passkeys are disabled.
func (s *Server) passkeyViews() []passkeyView {
	if !s.passkeyOn() {
		return nil
	}
	creds := s.Passkey.Store().List()
	out := make([]passkeyView, 0, len(creds))
	for _, c := range creds {
		out = append(out, passkeyView{
			Label:   c.Label,
			ID:      b64Std.EncodeToString(c.ID),
			AddedAt: c.AddedAt.Format("2006-01-02"),
		})
	}
	return out
}

// handlePasswd handles POST /ui/settings/passwd. Verifies the current
// password, hashes the new one, atomically updates config.json + the
// in-memory auth.Cfg, then revokes every session (including this one)
// and bounces the browser back to the login page so the user has to
// authenticate with the new password.
func (s *Server) handlePasswd(w http.ResponseWriter, r *http.Request) {
	if s.ConfigPath == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderPasswdErr(w, r, "bad form")
		return
	}
	old := r.FormValue("old")
	newp := r.FormValue("new")
	confirm := r.FormValue("confirm")
	if newp != confirm {
		s.renderPasswdErr(w, r, "new password and confirmation do not match")
		return
	}
	if len(newp) < 8 {
		s.renderPasswdErr(w, r, "new password must be at least 8 characters")
		return
	}
	if err := s.Auth.Verify(s.Auth.Cfg.Username, old); err != nil {
		s.renderPasswdErr(w, r, "current password is incorrect")
		return
	}
	h, err := authHashPasswordHook(newp)
	if err != nil {
		s.renderPasswdErr(w, r, "hash: "+err.Error())
		return
	}
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		s.renderPasswdErr(w, r, "load config: "+err.Error())
		return
	}
	cfg.Auth.PasswordHash = h
	if err := config.Save(s.ConfigPath, cfg); err != nil {
		s.renderPasswdErr(w, r, "save config: "+err.Error())
		return
	}
	s.Auth.SetPasswordHash(h)
	_ = s.Auth.RevokeAllSessions()
	auth.ClearSessionCookie(w)
	RelRedirect(w, r, "../login?passwd=1", http.StatusSeeOther)
}

func (s *Server) renderPasswdErr(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	d := s.base(r)
	d.Error = msg
	s.render(w, "settings", struct {
		baseData
		Ok       bool
		Passkeys []passkeyView
	}{d, false, s.passkeyViews()})
}

// handleStatic serves bundled CSS / overrides theme.css.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ui/static/")
	if name == "theme.css" && s.Overrides != "" {
		p := filepath.Join(s.Overrides, "overrides", "theme.css")
		http.ServeFile(w, r, p)
		return
	}
	data, err := embeddedStatic.ReadFile("static/" + name)
	if err != nil {
		// embed.FS.ReadFile only returns NotExist for missing files.
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript")
	case strings.HasSuffix(name, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	case strings.HasSuffix(name, ".png"):
		w.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(name, ".ico"):
		w.Header().Set("Content-Type", "image/x-icon")
	case strings.HasSuffix(name, ".webmanifest"):
		w.Header().Set("Content-Type", "application/manifest+json")
	}
	// Aggressive cache when the URL is fingerprinted (?v=...). Without
	// the fingerprint we still allow short caching but require revalidation.
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	w.Write(data)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Authenticated UI pages must never be cached. Without this, hitting
	// the browser's Back button (or pressing `u` in the entry view) often
	// shows a stale snapshot of the list page — the entry you just toggled
	// read/unread/starred still appears in its old state until you F5.
	// Per the HTTP cache spec, no-store also disqualifies the response
	// from bfcache, so all back-navigations re-fetch from the server.
	// keys.js still has a bfcache pageshow.reload listener as belt-and-
	// suspenders in case a downstream cache layer overrides this header.
	w.Header().Set("Cache-Control", "no-store")
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "unknown template: "+name, http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// authHashPasswordHook is the seam tests swap to exercise the hash-error
// branch of handlePasswd. Production callers go through auth.HashPassword.
var authHashPasswordHook = auth.HashPassword

// parseTagInput splits a comma-separated tag form value into a
// normalised tag slice (trimmed, deduped, sorted).
func parseTagInput(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	return store.NormalizeTags(parts)
}

// formatPublished returns a compact relative-or-absolute timestamp used
// on entry list rows. <1h → "Nm", <24h → "Nh", <7d → "Nd", same year →
// "Jan 02", older → "2006-01-02". Zero time renders as "" so old test
// fixtures without a Published field don't show a stray label.
func formatPublished(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case t.Year() == now.Year():
		return t.Format("Jan 02")
	default:
		return t.Format("2006-01-02")
	}
}

// FeedPreview is the lightweight description the add-feed page renders
// after a successful fetch. We deliberately don't reuse store.Entry —
// the preview is purely visual and the user has not yet subscribed.
type FeedPreview struct {
	Title       string
	Description string
	Link        string
	Items       []FeedPreviewItem
}

// FeedPreviewItem is one entry shown in the add-feed preview list.
type FeedPreviewItem struct {
	Title string
}

// FeedPreviewer fetches + parses a feed URL without persisting it.
// Implementations should bound time + size and refuse non-feed content.
type FeedPreviewer interface {
	Preview(url string) (*FeedPreview, error)
}

// handleFeedNew renders the dedicated "add feed" page. GET shows an
// empty form; POST fetches the URL via Previewer and shows a preview
// underneath the form. The user then clicks "subscribe" which POSTs
// to the existing /ui/feed/add handler.
func (s *Server) handleFeedNew(w http.ResponseWriter, r *http.Request) {
	type newFeedData struct {
		baseData
		URL     string
		Tags    string
		Preview *FeedPreview
		AllTags []string // every tag across all feeds, for the tags datalist
	}
	d := newFeedData{baseData: s.base(r)}
	// Populate the tags datalist from every tag the OPML knows about, so
	// the comma-separated tags input can autocomplete existing tags
	// (mirrors handleFeed). A load failure is non-fatal: the form still
	// works, just without suggestions.
	if op, err := s.OPML.Load(); err == nil {
		d.AllTags = op.AllTags()
	}
	switch r.Method {
	case http.MethodGet:
		s.render(w, "newfeed", d)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			d.Error = err.Error()
			w.WriteHeader(http.StatusBadRequest)
			s.render(w, "newfeed", d)
			return
		}
		d.URL = strings.TrimSpace(r.FormValue("url"))
		d.Tags = strings.TrimSpace(r.FormValue("tags"))
		if d.URL == "" {
			d.Error = "feed URL is required"
			w.WriteHeader(http.StatusBadRequest)
			s.render(w, "newfeed", d)
			return
		}
		if s.Previewer == nil {
			d.Error = "preview not configured on this server"
			w.WriteHeader(http.StatusServiceUnavailable)
			s.render(w, "newfeed", d)
			return
		}
		p, err := s.Previewer.Preview(d.URL)
		if err != nil {
			d.Error = "could not fetch feed: " + err.Error()
			w.WriteHeader(http.StatusBadRequest)
			s.render(w, "newfeed", d)
			return
		}
		d.Preview = p
		s.render(w, "newfeed", d)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// entryTitle resolves a display title for e. Most feeds set Item.Title,
// but some (notably Mastodon, whose RSS items have no <title>) leave it
// empty — rendering blank rows and a blank entry header. In that case
// synthesise a title from the entry body: strip HTML to plain text and
// take a leading snippet. Falls back to "(untitled)" if there is no
// usable text at all.
func entryTitle(e store.Entry) string {
	if t := strings.TrimSpace(e.Title); t != "" {
		return t
	}
	body := e.Content
	if isLinkOnly(body) {
		body = e.Summary
	}
	text := htmlToText(body)
	if text == "" {
		return "(untitled)"
	}
	return truncateTitle(text, 100)
}

// truncateTitle shortens s to at most maxRunes runes, breaking on the
// last word boundary within budget where possible and appending an
// ellipsis. Operates on runes so multibyte text is not split mid-glyph.
func truncateTitle(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	cut := string(r[:maxRunes])
	if i := strings.LastIndex(cut, " "); i > maxRunes/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ") + "\u2026"
}
