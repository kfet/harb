// Package store is the on-disk storage layer for harb.
//
// Layout under the data dir:
//
//	subscriptions.opml          # feeds + folders (source of truth)
//	state/<feed-hash>.json      # per-feed poll state
//	entries/<feed-hash>/
//	    current.ndjson          # hot file
//	    YYYY-Qn.ndjson          # quarterly archives
//	read.log                    # append-only state log
//	starred.log                 # append-only state log
//
// Hashing: feed hashes are 20-hex-char (10-byte) sha1 prefixes used only
// for local filenames. Entry hashes are 16-hex-char (8-byte) sha1 prefixes:
// that size is the Google Reader / FreshRSS item-id convention and fits in
// the signed-int64 ids used by Reeder and other clients.
package store

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

const (
	FeedHashLen  = 20
	EntryHashLen = 16
)

// FeedHash returns the short hex hash for a feed URL.
func FeedHash(url string) string {
	sum := sha1.Sum([]byte(url))
	return hex.EncodeToString(sum[:])[:FeedHashLen]
}

// EntryHash returns the Reader-compatible short hex hash that is an entry's
// stable identity across polls. Identity precedence follows the RSS 2.0
// <guid> and Atom (RFC 4287 §4.2.6) <id> specs:
//
//  1. guid / atom:id when present & non-empty  → sole key (D1).
//  2. else <link>.
//  3. else hash(title + published RFC3339)      → last resort (D3).
//
// The <guid>/<id> is the identifier; <link> is location/presentation and is
// explicitly allowed to differ from the identifier and to change over time
// (WordPress slug/category edits move the link while the guid is fixed), so
// link MUST NOT be mixed into the key when a guid is present. Doing so was the
// pre-0.15 bug that double-stored slug-edited WordPress items.
//
// Normalisation is deliberately minimal and spec-safe (D2): surrounding ASCII
// whitespace is trimmed from guid and link, and NormalizeGUID strips a single
// trailing volatile RFC 1123 date. The identity string is NOT lowercased and
// trailing slashes are NOT touched — IRI path/query are case-sensitive and
// slash/percent-encoding equivalence cannot be assumed.
//
// isPermaLink (RSS) is not consulted (D5): it governs whether a guid is a URL,
// not whether it is the identity, and the universal gofeed parser does not
// surface it. A guid is the identity whether or not it is also a permalink.
//
// The high bit of the first byte is masked off so the 16-hex hash always
// fits in a positive int64 when decoded. Google Reader's monotonic
// uint64 item ids never used the top bit, and at least one mature client
// (Reeder) silently drops items whose `longId` exceeds int64 max — manifesting
// as roughly half of items missing from the feed display. Masking the
// high bit costs us 1 bit of hash space (still ~63 bits, no collision
// risk at this scale) and keeps the wire format compatible.
//
// This single-entry form never applies the D4 guid-reuse guard; callers that
// process a poll/file batch should use AssignEntryHashes so feeds that misuse
// one guid across distinct items do not collapse.
func EntryHash(guid, link, title string, published time.Time) string {
	return entryHashKey(guid, link, title, published, false)
}

// entryHashKey computes the masked 16-hex identity. When guidReused is true
// (D4 guard), the link is mixed back into a present guid's key — this is
// exactly the pre-0.15 (guid,link) scheme, so guid-reuse feeds keep their
// existing hashes and need no migration remap.
func entryHashKey(guid, link, title string, published time.Time, guidReused bool) string {
	g := NormalizeGUID(strings.TrimSpace(guid))
	l := strings.TrimSpace(link)
	h := sha1.New()
	switch {
	case g != "" && !guidReused:
		// guid/atom:id is the sole identity (D1).
		h.Write([]byte(g))
	case g != "":
		// D4 guard: feed reuses one guid across distinct items. Mix the
		// link back in (the pre-0.15 scheme: NG(guid) \0 link).
		h.Write([]byte(g))
		h.Write([]byte{0})
		h.Write([]byte(l))
	case l != "":
		// No guid: link is the identity (pre-0.15 empty-guid scheme:
		// "" \0 link), so link-only feeds keep their existing hashes.
		h.Write([]byte{0})
		h.Write([]byte(l))
	default:
		// D3 last resort: no guid and no link. Use title + published so
		// distinct linkless/untitled items stay distinct instead of all
		// collapsing to one "no identity" hash.
		h.Write([]byte{0})
		h.Write([]byte(title))
		h.Write([]byte{0})
		h.Write([]byte(published.UTC().Format(time.RFC3339)))
	}
	sum := h.Sum(nil)
	sum[0] &= 0x7F
	return hex.EncodeToString(sum)[:EntryHashLen]
}

// reusedGUIDs returns the set of normalised guids that appear on two or more
// entries with a differing Title within a single batch. Such feeds misuse
// <guid> as a non-unique value across genuinely-distinct articles; for those
// guids identity falls back to including the link (D4) so they are not
// collapsed.
//
// Distinctness keys on Title ONLY, deliberately NOT on <link>. Per the RSS/Atom
// identity spec, <link> is location/presentation and is explicitly allowed to
// drift for the SAME article (the WordPress slug/category edit that motivates
// this whole change), so a differing link is NOT evidence of a distinct item —
// using it here would wrongly treat slug-edited twins as reuse and refuse to
// collapse them (the exact bug, especially during migration where both
// historical link variants of one article coexist on disk). Genuinely-distinct
// articles differ in title/content. A guid repeated on items with the same
// title is the same article re-served (link drift or a re-poll) and is NOT
// treated as reuse.
func reusedGUIDs(entries []Entry) map[string]bool {
	first := map[string]string{} // guid -> first-seen title
	reused := map[string]bool{}
	for _, e := range entries {
		g := NormalizeGUID(strings.TrimSpace(e.GUID))
		if g == "" {
			continue
		}
		if prev, ok := first[g]; ok {
			if prev != e.Title {
				reused[g] = true
			}
		} else {
			first[g] = e.Title
		}
	}
	return reused
}

// AssignEntryHashes sets .Hash on every entry in the batch, applying the D4
// guid-reuse guard across the whole batch. Use this for poll and migration
// batches; EntryHash is the per-entry form without the guard.
func AssignEntryHashes(entries []Entry) {
	reused := reusedGUIDs(entries)
	for i := range entries {
		g := NormalizeGUID(strings.TrimSpace(entries[i].GUID))
		entries[i].Hash = entryHashKey(
			entries[i].GUID, entries[i].Link, entries[i].Title,
			entries[i].Published, g != "" && reused[g])
	}
}

// trailingRFC1123 matches a single RFC 1123 date-time anchored at the end
// of a string, e.g. " Mon, 18 May 2026 21:12:26 EDT" or
// " Tue, 9 Jun 2026 13:05:18 +0000". The day-of-month may be 1 or 2 digits
// and the zone is a short alpha abbreviation or a numeric offset.
var trailingRFC1123 = regexp.MustCompile(
	` [A-Z][a-z]{2}, \d{1,2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} (?:[A-Za-z]{2,5}|[+-]\d{4})$`)

// NormalizeGUID strips a single trailing RFC 1123 date-time from a feed's
// item guid. Some feeds (e.g. Nintendo World Report) emit a non-permalink
// guid of the form "<stable-path> <pubDate>"; the pubDate's seconds drift
// between polls (…:26 → …:00), changing the guid and therefore the entry
// hash, so the same article is stored twice. Removing the volatile date
// tail yields a stable identity. Only an exact, fully-anchored RFC 1123
// tail is stripped — a guid that merely ends in digits is left untouched,
// so genuinely-distinct items are not collapsed.
func NormalizeGUID(guid string) string {
	return trailingRFC1123.ReplaceAllString(guid, "")
}

// CanonicalEntryHash normalises legacy on-disk entry hashes to the current
// 16-hex-char format. v0.4.4 and earlier stored 20-hex-char sha1 prefixes;
// Google Reader item ids are 16 hex chars, so migration truncates old hashes.
// It is length/case-only and does NOT touch the bits — the Reader API item-id
// round-trip relies on that identity.
func CanonicalEntryHash(hash string) string {
	if len(hash) >= EntryHashLen && isHex(hash) {
		return strings.ToLower(hash[:EntryHashLen])
	}
	return hash
}

// StoreEntryHash is the canonical *storage identity* of an entry hash:
// CanonicalEntryHash plus the high-bit mask that EntryHash applies
// (sum[0] &= 0x7F). The mask was added after some entries had already
// been persisted with the top bit set; on the next poll EntryHash
// produced the masked form, which no longer matched the stored unmasked
// hash, so the same article was stored — and displayed — twice. Masking
// here collapses a legacy unmasked hash and its masked re-poll to one
// id. The high bit lives in the first hex nibble, so clearing 0x8 off
// the leading hex digit is equivalent to sum[0] &= 0x7F.
//
// This is used only for on-disk/in-memory dedup, state-log keys and
// lookups — NOT for Reader item-id encoding, which keeps using
// CanonicalEntryHash so already-issued ids stay stable.
func StoreEntryHash(hash string) string {
	h := CanonicalEntryHash(hash)
	if len(h) != EntryHashLen || !isHex(h) {
		return h
	}
	b := []byte(h)
	switch c := b[0]; {
	case '8' <= c && c <= '9':
		b[0] = c - 8 // '8'..'9' -> '0'..'1'
	case 'a' <= c && c <= 'f':
		b[0] = c - 47 // 'a'..'f' (10..15) masked to 2..7 -> '2'..'7'
	}
	return string(b)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case '0' <= r && r <= '9':
		case 'a' <= r && r <= 'f':
		case 'A' <= r && r <= 'F':
		default:
			return false
		}
	}
	return true
}
