package reedercompat

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Harness is the per-test wiring an embedder supplies. Run calls
// NewHarness(t) once per sub-test so each contract gets a clean slate.
//
// The Harness deliberately uses concrete types over interfaces to keep
// the public surface tiny. Seed methods return per-entry 16-hex item
// hashes (the canonical id form, which the suite then folds into
// GReader-format ids and longIds itself). SetRead / SetStarred return
// the UpdatedAt time the implementation recorded; the suite uses this
// to assert ot/nt windows on state streams.
type Harness struct {
	// Handler serves the GReader API surface (and ClientLogin).
	Handler http.Handler
	// Token is a valid API token, sent as `Authorization: GoogleLogin auth=<Token>`.
	Token string

	// SeedFeed registers a feed with title=name (tags = [tag]) and
	// appends `count` entries with Published == FetchedAt == now()
	// (descending by minute is the embedder's choice; the suite only
	// requires the set of entries to exist and the hashes to be
	// returned in seed order).
	SeedFeed func(t *testing.T, name, tag string, count int) (feedURL string, hashes []string)

	// SeedFeedTimes is the differentiating variant: each entry takes
	// its own Published and FetchedAt time. The two slices must have
	// the same length and define the entries in seed order. Pass the
	// same slice twice when the test does not need to differentiate
	// the two sources.
	SeedFeedTimes func(t *testing.T, name, tag string, published, fetched []time.Time) (feedURL string, hashes []string)

	// SetRead flips the read flag and returns the UpdatedAt the
	// implementation recorded for that entry's state. The suite uses
	// the return value as the reference time for ot/nt assertions.
	SetRead func(t *testing.T, hash string, v bool) time.Time
	// SetStarred is the starred counterpart of SetRead.
	SetStarred func(t *testing.T, hash string, v bool) time.Time

	// SeedDriftTwin seeds a feed with a single article, then simulates a
	// later poll that re-serves the SAME <guid> with a drifted <link>
	// (the WordPress slug/category edit). It returns the feed URL and the
	// single canonical item hash the client must see. Optional: when nil,
	// the guid-stable link-drift dedup contract is skipped.
	SeedDriftTwin func(t *testing.T, name, tag string) (feedURL string, hash string)
}

// NewHarness is the factory the suite calls per sub-test.
type NewHarness func(t *testing.T) Harness

// ----- GReader spec constants (re-declared here so the suite has no
// import dependency on the harb implementation under test) -----

const (
	StreamReadingList = "user/-/state/com.google/reading-list"
	StateReadID       = "user/-/state/com.google/read"
	StateStarredID    = "user/-/state/com.google/starred"
)

// FeedStreamID and LabelStreamID build canonical GReader stream ids.
func FeedStreamID(feedURL string) string { return "feed/" + feedURL }
func LabelStreamID(name string) string   { return "user/-/label/" + name }

// ItemID returns the 16-hex tag-form item id GReader/Reeder expect on
// stream/contents responses.
func ItemID(hash string) string {
	return "tag:google.com,2005:reader/item/" + canonicalHex(hash)
}

// ItemLongID returns the decimal unsigned-int64 form GReader/Reeder
// expect on stream/items/ids responses (and accept on edit-tag).
//
// The wire is unsigned decimal. The implementation masks the high bit
// of the underlying 16-hex hash so every longId fits in a positive
// int63 — Reeder (and likely other strict clients) silently drop items
// whose longId parses as a negative signed-int64. Legacy clients that
// emitted signed-decimal ids on writes are still accepted on the server
// side; the suite asserts the read path emits unsigned only.
func ItemLongID(hash string) string {
	h := canonicalHex(hash)
	n, err := strconv.ParseUint(h, 16, 64)
	if err != nil {
		return "0"
	}
	return strconv.FormatUint(n, 10)
}

// canonicalHex returns the canonical 16-hex form. The hash may already
// be canonical (the common case) or may be a legacy 20-hex sha1 prefix
// — we truncate to 16 to match the implementation's hashing scheme.
func canonicalHex(hash string) string {
	if len(hash) >= 16 {
		return strings.ToLower(hash[:16])
	}
	return strings.ToLower(hash)
}

// ItemIDHex16Pattern matches a well-formed GReader stream/content item id.
var ItemIDHex16Pattern = regexp.MustCompile(`^tag:google\.com,2005:reader/item/[0-9a-f]{16}$`)

// ----- request helpers -----

// Do performs an authenticated request against the Harness handler and
// returns the recorder. Form-encoded body if vals is non-nil.
func Do(t *testing.T, h Harness, method, path string, vals url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return doRaw(t, h, newRequest(t, method, path, vals))
}

// newRequest builds an HTTP request with form-encoded body if vals is
// non-nil. The Harness token is NOT applied here — pass the request
// through doRaw (or Do), which will add the auth header unless one is
// already set. This split lets callers add extra headers between
// construction and execution (e.g. If-None-Match for ETag tests).
func newRequest(t *testing.T, method, path string, vals url.Values) *http.Request {
	t.Helper()
	var r *http.Request
	if vals != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(vals.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	return r
}

// doRaw runs a prebuilt request through the Harness handler, applying
// the Harness token if no Authorization header is already present.
func doRaw(t *testing.T, h Harness, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if h.Token != "" && r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "GoogleLogin auth="+h.Token)
	}
	w := httptest.NewRecorder()
	h.Handler.ServeHTTP(w, r)
	return w
}
