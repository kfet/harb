package reedercompat

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Reeder/GReader response shapes the suite asserts against. These are
// JSON-shape mirrors of the implementation's internal types — keep
// the field tags exactly equal to what real clients parse.

type streamLinkJSON struct {
	HREF string `json:"href"`
	Type string `json:"type,omitempty"`
}

type streamContentJSON struct {
	Direction string `json:"direction,omitempty"`
	Content   string `json:"content"`
}

type streamOriginJSON struct {
	StreamID string `json:"streamId"`
	Title    string `json:"title,omitempty"`
	HTMLURL  string `json:"htmlUrl,omitempty"`
}

type streamItemJSON struct {
	ID            string            `json:"id"`
	LongID        string            `json:"longId"`
	Categories    []string          `json:"categories"`
	Title         string            `json:"title"`
	Published     int64             `json:"published"`
	Updated       int64             `json:"updated"`
	CrawlTimeMsec string            `json:"crawlTimeMsec"`
	TimestampUsec string            `json:"timestampUsec"`
	Author        string            `json:"author,omitempty"`
	Alternate     []streamLinkJSON  `json:"alternate"`
	Summary       streamContentJSON `json:"summary"`
	Origin        streamOriginJSON  `json:"origin"`
}

type streamResponseJSON struct {
	ID           string           `json:"id"`
	Updated      int64            `json:"updated"`
	Title        string           `json:"title,omitempty"`
	Items        []streamItemJSON `json:"items"`
	Continuation string           `json:"continuation,omitempty"`
}

type itemRefJSON struct {
	ID              string   `json:"id"`
	LongID          string   `json:"longId"`
	TimestampUsec   string   `json:"timestampUsec"`
	DirectStreamIDs []string `json:"directStreamIds"`
}

type itemsIDsResponseJSON struct {
	ItemRefs     []itemRefJSON `json:"itemRefs"`
	Continuation string        `json:"continuation"`
}

// ---- assertion helpers ----

func assertItemHex16(t *testing.T, contract, id string) {
	t.Helper()
	if !ItemIDHex16Pattern.MatchString(id) {
		t.Errorf("compat %s: item id %q is not tag:google.com,2005:reader/item/<16-hex>", contract, id)
	}
}

func assertSignedInt64(t *testing.T, contract, s string) {
	t.Helper()
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		t.Errorf("compat %s: %q does not parse as signed int64: %v", contract, s, err)
	}
}

// assertUnsignedInt63 asserts s is a decimal that fits in [0, 2^63-1].
// This is the strict-Reeder wire-format requirement for item longIds:
// the value must parse as a non-negative signed int64.
func assertUnsignedInt63(t *testing.T, contract, s string) {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Errorf("compat %s: %q does not parse as signed int64: %v", contract, s, err)
		return
	}
	if n < 0 {
		t.Errorf("compat %s: %q is negative (%d); strict clients (Reeder) drop items whose longId exceeds int63", contract, s, n)
	}
}

// Run executes the full Reeder / GReader conformance suite against the
// embedder-supplied Harness factory. Each sub-test gets a fresh
// Harness via newH(t).
func Run(t *testing.T, newH NewHarness) {
	t.Run("item-id-16-hex+published-timestamps/contents", func(t *testing.T) {
		h := newH(t)
		u, hashes := h.SeedFeed(t, "F", "F", 2)
		w := Do(t, h, "GET", "/reader/api/0/stream/contents/feed/"+u, nil)
		if w.Code != 200 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var resp streamResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.Items) != 2 {
			t.Fatalf("items=%d, want 2", len(resp.Items))
		}
		want := map[string]bool{}
		for _, hh := range hashes {
			want[ItemID(hh)] = true
		}
		for _, it := range resp.Items {
			assertItemHex16(t, "item-id-16-hex/contents", it.ID)
			if !want[it.ID] {
				t.Errorf("compat item-id-16-hex/contents: unexpected id %q", it.ID)
			}
			if it.TimestampUsec == "" {
				t.Errorf("compat published-timestamps/contents: missing timestampUsec for %q", it.ID)
			}
			if it.CrawlTimeMsec == "" {
				t.Errorf("compat published-timestamps/contents: missing crawlTimeMsec for %q", it.ID)
			}
		}
	})

	t.Run("item-ref-decimal+longid-int64/items-ids", func(t *testing.T) {
		h := newH(t)
		_, hashes := h.SeedFeed(t, "F", "F", 3)
		w := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+StreamReadingList, nil)
		if w.Code != 200 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.ItemRefs) != 3 {
			t.Fatalf("itemRefs=%d, want 3", len(resp.ItemRefs))
		}
		want := map[string]bool{}
		for _, hh := range hashes {
			want[ItemLongID(hh)] = true
		}
		for _, r := range resp.ItemRefs {
			assertUnsignedInt63(t, "item-ref-decimal/items-ids", r.ID)
			if r.ID != r.LongID {
				t.Errorf("compat item-ref-decimal/items-ids: id=%q longId=%q must be equal", r.ID, r.LongID)
			}
			assertUnsignedInt63(t, "longid-int63/items-ids", r.LongID)
			assertSignedInt64(t, "timestampUsec-int64", r.TimestampUsec)
			if !want[r.ID] {
				t.Errorf("compat item-ref-decimal/items-ids: unexpected longId %q", r.ID)
			}
		}
	})

	t.Run("longid-unsigned-int63/wire-format", func(t *testing.T) {
		// Strict-Reeder contract (v0.4.13+): every server-emitted
		// item longId must parse as a non-negative signed int64
		// (i.e. fit in int63). The implementation guarantees this
		// by masking the high bit of the underlying sha1 hash; the
		// suite verifies it black-box by seeding a feed and
		// scanning every emitted ref.
		//
		// Pre-v0.4.13, ~half of items emitted longIds with the high
		// bit set, which Reeder silently dropped — a major item
		// visibility bug.
		h := newH(t)
		const n = 8
		h.SeedFeed(t, "Wide", "Wide", n)
		w := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+StreamReadingList+"&n=100", nil)
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if len(resp.ItemRefs) != n {
			t.Fatalf("itemRefs=%d, want %d", len(resp.ItemRefs), n)
		}
		for _, r := range resp.ItemRefs {
			assertUnsignedInt63(t, "longid-unsigned-int63/items-ids.id", r.ID)
			assertUnsignedInt63(t, "longid-unsigned-int63/items-ids.longId", r.LongID)
		}
		// stream/contents emits 16-hex item ids; cross-check that
		// the same int63 guarantee holds on that path.
		c := Do(t, h, "GET", "/reader/api/0/stream/contents/feed/https://feed.example/Wide", nil)
		var cresp streamResponseJSON
		if err := json.Unmarshal(c.Body.Bytes(), &cresp); err != nil {
			t.Fatalf("unmarshal contents: %v body=%s", err, c.Body.String())
		}
		for _, it := range cresp.Items {
			hex := strings.TrimPrefix(it.ID, "tag:google.com,2005:reader/item/")
			n, err := strconv.ParseUint(hex, 16, 64)
			if err != nil {
				t.Errorf("compat longid-unsigned-int63/contents: id %q not 16-hex: %v", it.ID, err)
				continue
			}
			if n>>63 != 0 {
				t.Errorf("compat longid-unsigned-int63/contents: id %q has top bit set (decoded=%d); strict clients drop it", it.ID, n)
			}
		}
	})

	t.Run("accept-decimal-longid/edit-tag", func(t *testing.T) {
		h := newH(t)
		_, hashes := h.SeedFeed(t, "F", "F", 2)
		// Decimal long id (Reeder uses this shape on writes).
		long := ItemLongID(hashes[1])
		assertUnsignedInt63(t, "accept-decimal-longid", long)
		body := url.Values{"i": {long}, "a": {StateStarredID}}
		if w := Do(t, h, "POST", "/reader/api/0/edit-tag", body); w.Code != 200 {
			t.Fatalf("compat accept-decimal-longid: code=%d body=%s", w.Code, w.Body.String())
		}
		// Verify via stream/items/ids?s=starred
		w := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(StateStarredID), nil)
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.ItemRefs) != 1 || resp.ItemRefs[0].ID != long {
			t.Errorf("compat accept-decimal-longid: starred set=%+v, want only %q", resp.ItemRefs, long)
		}
	})

	t.Run("item-categories/reading-list+label", func(t *testing.T) {
		h := newH(t)
		u, _ := h.SeedFeed(t, "Tech", "Tech", 1)
		w := Do(t, h, "GET", "/reader/api/0/stream/contents/feed/"+u, nil)
		var resp streamResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if len(resp.Items) != 1 {
			t.Fatalf("items=%d, want 1", len(resp.Items))
		}
		cats := resp.Items[0].Categories
		has := func(want string) bool {
			for _, c := range cats {
				if c == want {
					return true
				}
			}
			return false
		}
		for _, want := range []string{StreamReadingList, LabelStreamID("Tech")} {
			if !has(want) {
				t.Errorf("compat item-categories: %v missing %q", cats, want)
			}
		}
		if has(StateReadID) {
			t.Errorf("compat item-categories: unread item carries read state: %v", cats)
		}
	})

	t.Run("direct-stream-ids/items-ids", func(t *testing.T) {
		h := newH(t)
		u, _ := h.SeedFeed(t, "News", "News", 1)
		w := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+StreamReadingList, nil)
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.ItemRefs) != 1 {
			t.Fatalf("itemRefs=%d, want 1", len(resp.ItemRefs))
		}
		ds := resp.ItemRefs[0].DirectStreamIDs
		seen := map[string]bool{}
		for _, s := range ds {
			seen[s] = true
		}
		if !seen[FeedStreamID(u)] || !seen[LabelStreamID("News")] {
			t.Errorf("compat direct-stream-ids: %v missing %q and/or %q", ds, FeedStreamID(u), LabelStreamID("News"))
		}
	})

	t.Run("items-contents-empty/reading-list-stream-id", func(t *testing.T) {
		// v0.4.13: POST /stream/items/contents with no `i` params
		// must respond with a well-formed stream envelope keyed on
		// the reading-list stream id and a fresh `updated`
		// timestamp — not the pre-fix placeholder `{"id":"items",
		// "updated":0,...}`. Strict clients reject the placeholder
		// shape as malformed.
		h := newH(t)
		h.SeedFeed(t, "F", "F", 1)
		w := Do(t, h, "POST", "/reader/api/0/stream/items/contents", url.Values{})
		if w.Code != 200 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var resp streamResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if resp.ID != StreamReadingList {
			t.Errorf("compat items-contents-empty: id=%q, want %q", resp.ID, StreamReadingList)
		}
		if resp.Updated <= 0 {
			t.Errorf("compat items-contents-empty: updated=%d, want >0 (current server time)", resp.Updated)
		}
		if len(resp.Items) != 0 {
			t.Errorf("compat items-contents-empty: items=%d, want 0", len(resp.Items))
		}
	})

	t.Run("timestamp-encoding/stream-contents", func(t *testing.T) {
		// Wire-format lock — both units AND source:
		//   - published/updated = entry Published, in seconds (display date)
		//   - timestampUsec     = entry SYNC time = max(Published, FetchedAt), µs
		//   - crawlTimeMsec     = entry FetchedAt, in milliseconds
		// timestampUsec is the GReader stream cursor/sort key and MUST track
		// arrival (fetch) time, not publish date, or incremental clients
		// (Reeder) never see late-fetched, old-dated items (CBC/Penny Arcade
		// backdate their items). published/updated stay the publish date so
		// the displayed article date is unchanged. Published and FetchedAt are
		// deliberately disjoint so a regression swapping the slots fails here.
		h := newH(t)
		// Two fixed, well-defined timestamps with no overlap. Use
		// values that round-trip cleanly through seconds/ms/µs.
		pubBase := time.Unix(1700000000, 0).UTC()   // 2023-11-14 22:13:20 UTC
		fetchBase := time.Unix(1800000000, 0).UTC() // 2027-01-15 08:00:00 UTC
		published := []time.Time{pubBase, pubBase.Add(time.Minute)}
		fetched := []time.Time{fetchBase, fetchBase.Add(time.Minute)}
		u, hashes := h.SeedFeedTimes(t, "TS", "TS", published, fetched)
		w := Do(t, h, "GET", "/reader/api/0/stream/contents/feed/"+u, nil)
		if w.Code != 200 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var resp streamResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		byID := map[string]streamItemJSON{}
		for _, it := range resp.Items {
			byID[it.ID] = it
		}
		for i, hh := range hashes {
			it, ok := byID[ItemID(hh)]
			if !ok {
				t.Fatalf("compat timestamp-encoding: missing item for hash %q", hh)
			}
			wantPubSec := published[i].Unix()
			// sync time = the later of publish/fetch; fetched is after
			// published in this fixture, so timestampUsec must read fetch.
			syncT := published[i]
			if fetched[i].After(syncT) {
				syncT = fetched[i]
			}
			wantSyncUsec := strconv.FormatInt(syncT.UnixMicro(), 10)
			wantFetchMsec := strconv.FormatInt(fetched[i].UnixMilli(), 10)
			if it.Published != wantPubSec {
				t.Errorf("compat timestamp-encoding: item[%d].published=%d, want %d (Published, seconds)", i, it.Published, wantPubSec)
			}
			if it.Updated != wantPubSec {
				t.Errorf("compat timestamp-encoding: item[%d].updated=%d, want %d (Published, seconds)", i, it.Updated, wantPubSec)
			}
			if it.TimestampUsec != wantSyncUsec {
				t.Errorf("compat timestamp-encoding: item[%d].timestampUsec=%q, want %q (sync time = max(Published,FetchedAt), microseconds)", i, it.TimestampUsec, wantSyncUsec)
			}
			if it.CrawlTimeMsec != wantFetchMsec {
				t.Errorf("compat timestamp-encoding: item[%d].crawlTimeMsec=%q, want %q (FetchedAt, milliseconds)", i, it.CrawlTimeMsec, wantFetchMsec)
			}
		}
	})

	t.Run("timestamp-encoding/zero-published-falls-back-to-fetched", func(t *testing.T) {
		// Edge: when Published is zero (e.g. feed item with no
		// pubDate), the display time falls back to FetchedAt for
		// timestampUsec / published / updated. crawlTimeMsec stays
		// pinned to FetchedAt regardless. This is the documented
		// `entryDisplayTime` contract in reader.go.
		h := newH(t)
		fetch := time.Unix(1800000000, 0).UTC()
		u, hashes := h.SeedFeedTimes(t, "TZ", "TZ", []time.Time{{}}, []time.Time{fetch})
		w := Do(t, h, "GET", "/reader/api/0/stream/contents/feed/"+u, nil)
		var resp streamResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if len(resp.Items) != 1 {
			t.Fatalf("items=%d, want 1", len(resp.Items))
		}
		it := resp.Items[0]
		if it.ID != ItemID(hashes[0]) {
			t.Fatalf("item id mismatch: got %q want %q", it.ID, ItemID(hashes[0]))
		}
		wantSec := fetch.Unix()
		wantUsec := strconv.FormatInt(fetch.UnixMicro(), 10)
		wantMsec := strconv.FormatInt(fetch.UnixMilli(), 10)
		if it.Published != wantSec || it.Updated != wantSec {
			t.Errorf("compat zero-published-fallback: published=%d updated=%d, want both %d (FetchedAt seconds)", it.Published, it.Updated, wantSec)
		}
		if it.TimestampUsec != wantUsec {
			t.Errorf("compat zero-published-fallback: timestampUsec=%q, want %q (FetchedAt µs)", it.TimestampUsec, wantUsec)
		}
		if it.CrawlTimeMsec != wantMsec {
			t.Errorf("compat zero-published-fallback: crawlTimeMsec=%q, want %q (FetchedAt ms)", it.CrawlTimeMsec, wantMsec)
		}
	})

	t.Run("preserve-i-order/items-contents", func(t *testing.T) {
		h := newH(t)
		_, hashes := h.SeedFeed(t, "F", "F", 3)
		want := []string{ItemID(hashes[2]), ItemID(hashes[0]), ItemID(hashes[1])}
		body := url.Values{}
		for _, id := range want {
			body.Add("i", id)
		}
		w := Do(t, h, "POST", "/reader/api/0/stream/items/contents", body)
		if w.Code != 200 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var resp streamResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.Items) != len(want) {
			t.Fatalf("items=%d, want %d", len(resp.Items), len(want))
		}
		for i, it := range resp.Items {
			if it.ID != want[i] {
				t.Errorf("compat preserve-i-order: item[%d]=%q, want %q", i, it.ID, want[i])
			}
		}
	})

	t.Run("reading-list-all-vs-xt-unread/items-ids", func(t *testing.T) {
		h := newH(t)
		_, hashes := h.SeedFeed(t, "F", "F", 3)
		h.SetRead(t, hashes[0], true)

		all := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+StreamReadingList+"&n=10", nil)
		unread := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+StreamReadingList+"&xt="+url.QueryEscape(StateReadID)+"&n=10", nil)
		var allResp, unreadResp itemsIDsResponseJSON
		if err := json.Unmarshal(all.Body.Bytes(), &allResp); err != nil {
			t.Fatalf("unmarshal all: %v", err)
		}
		if err := json.Unmarshal(unread.Body.Bytes(), &unreadResp); err != nil {
			t.Fatalf("unmarshal unread: %v", err)
		}
		if len(allResp.ItemRefs) != 3 {
			t.Errorf("compat reading-list-all: bare reading-list returned %d refs, want all 3", len(allResp.ItemRefs))
		}
		if len(unreadResp.ItemRefs) != 2 {
			t.Errorf("compat reading-list-xt-unread: itemRefs=%d, want 2 (read entry must be excluded only by xt=read)", len(unreadResp.ItemRefs))
		}
		readID := ItemLongID(hashes[0])
		for _, r := range unreadResp.ItemRefs {
			if r.ID == readID {
				t.Errorf("compat reading-list-xt-unread: read id %q leaked through xt=read", r.ID)
			}
		}
	})

	t.Run("ot-nt-filters-content-stream/items-ids", func(t *testing.T) {
		// On content streams (feed/, label/, reading-list) ot/nt
		// compare against entry fetch/publish time. This is the
		// pre-existing contract; assert it still holds.
		h := newH(t)
		base := time.Now().UTC().Add(-3 * time.Hour)
		stamps := []time.Time{
			base,
			base.Add(time.Hour),
			base.Add(2 * time.Hour),
		}
		u, _ := h.SeedFeedTimes(t, "Filters", "F", stamps, stamps)
		path := "/reader/api/0/stream/items/ids?s=" + url.QueryEscape("feed/"+u) +
			"&ot=" + strconv.FormatInt(base.Add(30*time.Minute).Unix(), 10) +
			"&nt=" + strconv.FormatInt(base.Add(90*time.Minute).Unix(), 10) + "&n=10"
		w := Do(t, h, "GET", path, nil)
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if len(resp.ItemRefs) != 1 {
			t.Errorf("compat ot-nt-filters/content: got %d refs, want exactly the entry between ot and nt; body=%s", len(resp.ItemRefs), w.Body.String())
		}
		future := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape("feed/"+u)+
			"&ot="+strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10), nil)
		if !strings.Contains(future.Body.String(), `"itemRefs":[]`) {
			t.Errorf("compat ot-nt-filters: empty result must encode itemRefs as [], body=%s", future.Body.String())
		}
	})

	t.Run("backdated-item/visible-to-incremental", func(t *testing.T) {
		// Regression (the CBC / Penny Arcade bug): an item published in the
		// past but FETCHED now must sort and stamp by SYNC time =
		// max(Published, FetchedAt). Many feeds backdate items to article
		// time; if timestampUsec uses publish time it sits below an
		// incremental client's cursor and the item is never delivered, even
		// though it is genuinely new and unread.
		h := newH(t)
		now := time.Now().UTC().Truncate(time.Second)
		// 0 = fresh (pub & fetched ~now-2min); 1 = backdated (pub 48h ago,
		// fetched NOW — it arrived later than the fresh item).
		published := []time.Time{now.Add(-2 * time.Minute), now.Add(-48 * time.Hour)}
		fetched := []time.Time{now.Add(-2 * time.Minute), now}
		_, hashes := h.SeedFeedTimes(t, "BD", "BD", published, fetched)
		backdated := hashes[1]

		// 1. Default reading-list order is newest-ARRIVAL first: backdated
		//    (fetched now) sorts before fresh (fetched -2min).
		w := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+StreamReadingList+"&n=100", nil)
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.ItemRefs) != 2 {
			t.Fatalf("reading-list returned %d refs, want 2", len(resp.ItemRefs))
		}
		if resp.ItemRefs[0].ID != ItemLongID(backdated) {
			t.Errorf("order: ref[0]=%q want backdated %q (latest-fetched must sort first)", resp.ItemRefs[0].ID, ItemLongID(backdated))
		}
		// 2. Backdated item's timestampUsec is its fetch time (~now), not its
		//    publish time (48h ago).
		var backTS string
		for _, r := range resp.ItemRefs {
			if r.ID == ItemLongID(backdated) {
				backTS = r.TimestampUsec
			}
		}
		wantTS := strconv.FormatInt(fetched[1].UnixMicro(), 10)
		if backTS != wantTS {
			t.Errorf("backdated timestampUsec=%q, want fetch-time %q (not publish time)", backTS, wantTS)
		}
		// 3. An incremental client whose cursor already advanced past the
		//    fresh item (ot between fresh and backdated sync times) MUST still
		//    receive the backdated item. Under the old publish-time contract
		//    it was lost forever.
		ot := now.Add(-time.Minute).Unix()
		inc := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+StreamReadingList+
			"&ot="+strconv.FormatInt(ot, 10)+"&n=100", nil)
		var incResp itemsIDsResponseJSON
		if err := json.Unmarshal(inc.Body.Bytes(), &incResp); err != nil {
			t.Fatalf("unmarshal inc: %v", err)
		}
		if len(incResp.ItemRefs) != 1 || incResp.ItemRefs[0].ID != ItemLongID(backdated) {
			t.Errorf("incremental ot=%d returned %+v, want exactly the backdated item %q", ot, incResp.ItemRefs, ItemLongID(backdated))
		}
	})

	t.Run("ot-state-stream-uses-state-updatedat/read", func(t *testing.T) {
		// THE BUG: stream/items/ids?s=read&ot=X must return items
		// whose READ STATE was updated after X — not items whose
		// fetch/publish time is after X. Reeder uses this as its
		// incremental read-state sync; if it returns "everything
		// recently fetched", Reeder re-marks every just-polled item
		// as read on every sync and clobbers its own unread display.
		//
		// We construct a scenario that is observable through the
		// public HTTP surface and that DIFFERENTIATES the buggy
		// "FetchedAt-based" filter from the correct "UpdatedAt-based"
		// filter: entries with FetchedAt in the FAR FUTURE, then
		// marked read (UpdatedAt ≈ now). A query with
		// ot = (mark + 30min) MUST exclude them under the correct
		// semantics; under the buggy semantics they leak through
		// because FetchedAt is hours after ot.
		h := newH(t)
		const total, marked = 5, 3
		futureFetched := time.Now().UTC().Add(time.Hour)
		fetched := make([]time.Time, total)
		for i := range fetched {
			fetched[i] = futureFetched
		}
		_, hashes := h.SeedFeedTimes(t, "RS", "RS", fetched, fetched)
		var tMark time.Time
		for i := 0; i < marked; i++ {
			tMark = h.SetRead(t, hashes[i], true)
		}
		// All-time s=read returns the marked set.
		full := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(StateReadID)+"&n=100", nil)
		var fullResp itemsIDsResponseJSON
		if err := json.Unmarshal(full.Body.Bytes(), &fullResp); err != nil {
			t.Fatalf("unmarshal full: %v", err)
		}
		if len(fullResp.ItemRefs) != marked {
			t.Fatalf("compat ot-state-stream/baseline: s=read returned %d refs, want %d", len(fullResp.ItemRefs), marked)
		}
		// ot well before the mark → include all marked.
		before := tMark.Add(-30 * time.Minute).Unix()
		w := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(StateReadID)+
			"&ot="+strconv.FormatInt(before, 10)+"&n=100", nil)
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if len(resp.ItemRefs) != marked {
			t.Errorf("compat ot-state-stream-uses-state-updatedat/read: ot=%d (before mark) returned %d refs, want %d", before, len(resp.ItemRefs), marked)
		}
		// ot AFTER the mark but well before the (future) FetchedAt:
		// the differentiating case. The buggy filter would compare
		// FetchedAt (= now+1h) against ot (= now+30min), keep the
		// items, and return `marked`. The correct filter compares
		// UpdatedAt (≈ now) against ot and returns 0.
		after := tMark.Add(30 * time.Minute).Unix()
		w2 := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(StateReadID)+
			"&ot="+strconv.FormatInt(after, 10)+"&n=100", nil)
		var resp2 itemsIDsResponseJSON
		if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w2.Body.String())
		}
		if len(resp2.ItemRefs) != 0 {
			t.Errorf("compat ot-state-stream-uses-state-updatedat/read: ot=%d (after mark, before FetchedAt) returned %d refs, want 0 — filter is comparing entry fetch time instead of state UpdatedAt", after, len(resp2.ItemRefs))
		}
		// Cross-check: s=reading-list&xt=read returns the unread N-M.
		unread := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+StreamReadingList+
			"&xt="+url.QueryEscape(StateReadID)+"&n=100", nil)
		var unreadResp itemsIDsResponseJSON
		if err := json.Unmarshal(unread.Body.Bytes(), &unreadResp); err != nil {
			t.Fatalf("unmarshal unread: %v", err)
		}
		if len(unreadResp.ItemRefs) != total-marked {
			t.Errorf("compat ot-state-stream cross-check: reading-list&xt=read returned %d, want %d", len(unreadResp.ItemRefs), total-marked)
		}
	})

	t.Run("ot-state-stream-uses-state-updatedat/starred", func(t *testing.T) {
		// Same differentiating construction for the starred stream.
		h := newH(t)
		const total, marked = 4, 2
		futureFetched := time.Now().UTC().Add(time.Hour)
		fetched := make([]time.Time, total)
		for i := range fetched {
			fetched[i] = futureFetched
		}
		_, hashes := h.SeedFeedTimes(t, "ST", "ST", fetched, fetched)
		var tMark time.Time
		for i := 0; i < marked; i++ {
			tMark = h.SetStarred(t, hashes[i], true)
		}
		before := tMark.Add(-30 * time.Minute).Unix()
		w := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(StateStarredID)+
			"&ot="+strconv.FormatInt(before, 10)+"&n=100", nil)
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if len(resp.ItemRefs) != marked {
			t.Errorf("compat ot-state-stream/starred: ot before mark returned %d, want %d", len(resp.ItemRefs), marked)
		}
		after := tMark.Add(30 * time.Minute).Unix()
		w2 := Do(t, h, "GET", "/reader/api/0/stream/items/ids?s="+url.QueryEscape(StateStarredID)+
			"&ot="+strconv.FormatInt(after, 10)+"&n=100", nil)
		var resp2 itemsIDsResponseJSON
		if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w2.Body.String())
		}
		if len(resp2.ItemRefs) != 0 {
			t.Errorf("compat ot-state-stream/starred: ot after mark (before FetchedAt) returned %d, want 0 — filter is comparing fetch time, not state UpdatedAt", len(resp2.ItemRefs))
		}
	})

	t.Run("it-filter/selects-starred", func(t *testing.T) {
		h := newH(t)
		_, hashes := h.SeedFeed(t, "F", "F", 3)
		h.SetStarred(t, hashes[2], true)
		u := "https://feed.example/F"
		path := "/reader/api/0/stream/items/ids?s=" + url.QueryEscape("feed/"+u) +
			"&it=" + url.QueryEscape(StateStarredID)
		w := Do(t, h, "GET", path, nil)
		var resp itemsIDsResponseJSON
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		wantID := ItemLongID(hashes[2])
		if len(resp.ItemRefs) != 1 || resp.ItemRefs[0].ID != wantID {
			t.Errorf("compat it-filter: refs=%+v, want only %q", resp.ItemRefs, wantID)
		}
	})

	t.Run("continuation-paging/items-ids", func(t *testing.T) {
		h := newH(t)
		const total = 25
		h.SeedFeed(t, "F", "F", total)
		seen := map[string]bool{}
		cont := ""
		pages := 0
		for {
			path := "/reader/api/0/stream/items/ids?s=" + StreamReadingList + "&n=10"
			if cont != "" {
				path += "&c=" + cont
			}
			w := Do(t, h, "GET", path, nil)
			if w.Code != 200 {
				t.Fatalf("page %d code=%d body=%s", pages, w.Code, w.Body.String())
			}
			var p itemsIDsResponseJSON
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, r := range p.ItemRefs {
				if seen[r.ID] {
					t.Errorf("compat continuation-paging: duplicate id %q on page %d", r.ID, pages)
				}
				seen[r.ID] = true
			}
			pages++
			if p.Continuation == "" {
				break
			}
			// Token shape sanity-check.
			dec, err := base64.RawURLEncoding.DecodeString(p.Continuation)
			if err != nil {
				t.Fatalf("compat continuation-paging: token not base64-raw: %v", err)
			}
			var off struct {
				Offset int `json:"o"`
			}
			if err := json.Unmarshal(dec, &off); err != nil {
				t.Fatalf("compat continuation-paging: token not {\"o\":int}: %v", err)
			}
			cont = p.Continuation
			if pages > 10 {
				t.Fatal("compat continuation-paging: walked too many pages (infinite loop)")
			}
		}
		if len(seen) != total {
			t.Errorf("compat continuation-paging: saw %d distinct ids, want %d", len(seen), total)
		}
	})

	t.Run("unread-count-newest/per-row+global+output-json", func(t *testing.T) {
		h := newH(t)
		base := time.Now().UTC().Add(-24 * time.Hour)
		urlA, _ := h.SeedFeedTimes(t, "A", "A", []time.Time{base, base.Add(1 * time.Hour)}, []time.Time{base, base.Add(1 * time.Hour)})
		urlB, _ := h.SeedFeedTimes(t, "B", "B", []time.Time{base.Add(10 * time.Hour)}, []time.Time{base.Add(10 * time.Hour)})
		w := Do(t, h, "GET", "/reader/api/0/unread-count?output=json", nil)
		if w.Code != 200 {
			t.Fatalf("compat output-json: code=%d body=%s", w.Code, w.Body.String())
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
			if r.NewestItemTimestampUsec == "" {
				t.Errorf("compat unread-count-newest: row %q has empty newestItemTimestampUsec", r.ID)
			}
			assertSignedInt64(t, "unread-count-newest", r.NewestItemTimestampUsec)
			got[r.ID] = r.NewestItemTimestampUsec
			gotCount[r.ID] = r.Count
		}
		if got["feed/"+urlA] == "" {
			t.Errorf("compat unread-count-newest: feed A missing")
		}
		if got["feed/"+urlB] == "" {
			t.Errorf("compat unread-count-newest: feed B missing")
		}
		if got[StreamReadingList] == "" {
			t.Errorf("compat unread-count-newest: reading-list missing")
		}
		if gotCount[StreamReadingList] != 3 {
			t.Errorf("compat unread-count-newest: reading-list count=%d, want 3", gotCount[StreamReadingList])
		}
	})

	t.Run("etag-conditional/subscription-list", func(t *testing.T) {
		// First GET emits an ETag; a follow-up with If-None-Match
		// returns 304 + the same ETag. Mutating the OPML (seed a
		// new feed) MUST change the ETag and the conditional GET
		// returns 200 with the fresh body.
		h := newH(t)
		h.SeedFeed(t, "S1", "S1", 1)
		w := Do(t, h, "GET", "/reader/api/0/subscription/list", nil)
		if w.Code != 200 {
			t.Fatalf("first GET: code=%d body=%s", w.Code, w.Body.String())
		}
		etag := w.Result().Header.Get("ETag")
		if etag == "" {
			t.Fatal("compat etag-conditional/subscription-list: first response has no ETag header")
		}
		// Conditional GET → 304, same ETag, empty body.
		r2 := newRequest(t, "GET", "/reader/api/0/subscription/list", nil)
		r2.Header.Set("If-None-Match", etag)
		w2 := doRaw(t, h, r2)
		if w2.Code != http.StatusNotModified {
			t.Errorf("compat etag-conditional/subscription-list: INM match code=%d, want 304; body=%s", w2.Code, w2.Body.String())
		}
		if got := w2.Result().Header.Get("ETag"); got != etag {
			t.Errorf("compat etag-conditional/subscription-list: 304 ETag=%q, want %q", got, etag)
		}
		if w2.Body.Len() != 0 {
			t.Errorf("compat etag-conditional/subscription-list: 304 body must be empty, got %d bytes", w2.Body.Len())
		}
		// State change: add a feed → ETag must change.
		h.SeedFeed(t, "S2", "S2", 1)
		r3 := newRequest(t, "GET", "/reader/api/0/subscription/list", nil)
		r3.Header.Set("If-None-Match", etag)
		w3 := doRaw(t, h, r3)
		if w3.Code != 200 {
			t.Errorf("compat etag-conditional/subscription-list: after OPML change INM should miss, got code=%d", w3.Code)
		}
		if got := w3.Result().Header.Get("ETag"); got == "" || got == etag {
			t.Errorf("compat etag-conditional/subscription-list: ETag did not change across OPML mutation: was=%q now=%q", etag, got)
		}
	})

	t.Run("etag-conditional/tag-list", func(t *testing.T) {
		// Same shape as subscription-list. Mutate OPML by adding a
		// feed with a new tag; the tag set changes → ETag changes.
		h := newH(t)
		h.SeedFeed(t, "T1", "alpha", 1)
		w := Do(t, h, "GET", "/reader/api/0/tag/list", nil)
		if w.Code != 200 {
			t.Fatalf("first GET: code=%d body=%s", w.Code, w.Body.String())
		}
		etag := w.Result().Header.Get("ETag")
		if etag == "" {
			t.Fatal("compat etag-conditional/tag-list: first response has no ETag header")
		}
		r2 := newRequest(t, "GET", "/reader/api/0/tag/list", nil)
		r2.Header.Set("If-None-Match", etag)
		w2 := doRaw(t, h, r2)
		if w2.Code != http.StatusNotModified {
			t.Errorf("compat etag-conditional/tag-list: INM match code=%d, want 304", w2.Code)
		}
		h.SeedFeed(t, "T2", "beta", 1)
		r3 := newRequest(t, "GET", "/reader/api/0/tag/list", nil)
		r3.Header.Set("If-None-Match", etag)
		w3 := doRaw(t, h, r3)
		if w3.Code != 200 {
			t.Errorf("compat etag-conditional/tag-list: after tag mutation INM should miss, got code=%d", w3.Code)
		}
		if got := w3.Result().Header.Get("ETag"); got == etag {
			t.Errorf("compat etag-conditional/tag-list: ETag did not change across tag mutation: %q", got)
		}
	})

	t.Run("etag-conditional/unread-count", func(t *testing.T) {
		// unread-count depends on BOTH OPML state and entry-state
		// version. A read-state mutation MUST change the ETag even
		// when OPML is unchanged.
		h := newH(t)
		_, hashes := h.SeedFeed(t, "U", "U", 3)
		w := Do(t, h, "GET", "/reader/api/0/unread-count?output=json", nil)
		if w.Code != 200 {
			t.Fatalf("first GET: code=%d body=%s", w.Code, w.Body.String())
		}
		etag := w.Result().Header.Get("ETag")
		if etag == "" {
			t.Fatal("compat etag-conditional/unread-count: first response has no ETag header")
		}
		// Same state → 304.
		r2 := newRequest(t, "GET", "/reader/api/0/unread-count?output=json", nil)
		r2.Header.Set("If-None-Match", etag)
		w2 := doRaw(t, h, r2)
		if w2.Code != http.StatusNotModified {
			t.Errorf("compat etag-conditional/unread-count: INM match code=%d, want 304", w2.Code)
		}
		// Read-state mutation → ETag changes.
		h.SetRead(t, hashes[0], true)
		r3 := newRequest(t, "GET", "/reader/api/0/unread-count?output=json", nil)
		r3.Header.Set("If-None-Match", etag)
		w3 := doRaw(t, h, r3)
		if w3.Code != 200 {
			t.Errorf("compat etag-conditional/unread-count: after read-state mutation INM should miss, got code=%d", w3.Code)
		}
		etag2 := w3.Result().Header.Get("ETag")
		if etag2 == "" || etag2 == etag {
			t.Errorf("compat etag-conditional/unread-count: ETag did not change across read-state mutation: was=%q now=%q", etag, etag2)
		}
		// Same state again → 304 with the new ETag.
		r4 := newRequest(t, "GET", "/reader/api/0/unread-count?output=json", nil)
		r4.Header.Set("If-None-Match", etag2)
		w4 := doRaw(t, h, r4)
		if w4.Code != http.StatusNotModified {
			t.Errorf("compat etag-conditional/unread-count: post-mutation re-INM code=%d, want 304", w4.Code)
		}
		// Starred-state mutation also bumps the validator.
		h.SetStarred(t, hashes[1], true)
		r5 := newRequest(t, "GET", "/reader/api/0/unread-count?output=json", nil)
		r5.Header.Set("If-None-Match", etag2)
		w5 := doRaw(t, h, r5)
		if w5.Code != 200 {
			t.Errorf("compat etag-conditional/unread-count: after star mutation INM should miss, got code=%d", w5.Code)
		}
	})

	t.Run("etag-conditional/unread-count-tracks-content-changes", func(t *testing.T) {
		// Anything that changes the unread-count payload must
		// invalidate the cached ETag — including new entries
		// arriving from a poll cycle (the bug a state-only
		// validator would have: clients get stale 304s after
		// fresh items are ingested).
		//
		// We exercise this by seeding a second feed via the
		// harness, which both adds entries and adds a feed to the
		// OPML — either change is sufficient to invalidate the
		// validator, and the contract is "content changed → ETag
		// changed", not "exactly this kind of change". A regression
		// that tracks only state-flag mutations (missing the new-
		// entries bump on AppendEntries) would still fail under
		// this scenario in implementations where OPML is also
		// fingerprint-validated, because such implementations
		// must include the AppendEntries bump for cases where the
		// client polls into an unchanged OPML.
		h := newH(t)
		h.SeedFeed(t, "P1", "P1", 2)
		w := Do(t, h, "GET", "/reader/api/0/unread-count?output=json", nil)
		etag := w.Result().Header.Get("ETag")
		if etag == "" {
			t.Fatal("compat unread-count-tracks-content-changes: first ETag missing")
		}
		r2 := newRequest(t, "GET", "/reader/api/0/unread-count?output=json", nil)
		r2.Header.Set("If-None-Match", etag)
		if w2 := doRaw(t, h, r2); w2.Code != http.StatusNotModified {
			t.Errorf("compat unread-count-tracks-content-changes: re-GET code=%d, want 304", w2.Code)
		}
		h.SeedFeed(t, "P2", "P2", 3)
		r3 := newRequest(t, "GET", "/reader/api/0/unread-count?output=json", nil)
		r3.Header.Set("If-None-Match", etag)
		w3 := doRaw(t, h, r3)
		if w3.Code != 200 {
			t.Errorf("compat unread-count-tracks-content-changes: after seed INM should miss, got code=%d", w3.Code)
		}
		if got := w3.Result().Header.Get("ETag"); got == "" || got == etag {
			t.Errorf("compat unread-count-tracks-content-changes: ETag did not change: was=%q now=%q", etag, got)
		}
	})

	t.Run("etag-conditional/headers", func(t *testing.T) {
		// Vary: Authorization and Cache-Control: private,no-cache
		// are part of the contract — they prevent shared caches
		// from returning the response for a different user and
		// require clients to revalidate every request.
		h := newH(t)
		h.SeedFeed(t, "H", "H", 1)
		for _, path := range []string{
			"/reader/api/0/subscription/list",
			"/reader/api/0/tag/list",
			"/reader/api/0/unread-count?output=json",
		} {
			w := Do(t, h, "GET", path, nil)
			if w.Code != 200 {
				t.Errorf("compat etag-conditional/headers %s: code=%d", path, w.Code)
				continue
			}
			hdrs := w.Result().Header
			if hdrs.Get("ETag") == "" {
				t.Errorf("compat etag-conditional/headers %s: missing ETag", path)
			}
			if cc := hdrs.Get("Cache-Control"); cc == "" || !strings.Contains(cc, "no-cache") {
				t.Errorf("compat etag-conditional/headers %s: Cache-Control=%q must contain no-cache", path, cc)
			}
			if vary := hdrs.Get("Vary"); !strings.Contains(vary, "Authorization") {
				t.Errorf("compat etag-conditional/headers %s: Vary=%q must include Authorization", path, vary)
			}
		}
	})
}
