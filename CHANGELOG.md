# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

## [0.20.1] - 2026-07-29

### Fixed

- **GReader clients (Reeder iOS) showed the ingest date instead of the
  article date for feeds whose whole archive arrived in one poll.** The
  emitted `timestampUsec` was the internal sync time
  (`max(published, fetched)`); Reeder treats `timestampUsec` as the item's
  canonical date and renders it in preference to `published`, so a feed
  backfilled in a single poll (e.g. `ratfactor.com/atom.xml`: 132 entries,
  one fetch) had every article stamped with — and displayed at — the
  ingest time, and all 132 items collapsed onto one identical stream
  cursor. `timestampUsec` is now the item's publication time
  (`entryDisplayTime`, falling back to fetch time when the feed gives no
  date), matching miniflux (`timestampUsec = entry.Date`) and restoring the
  v0.4.8/0.4.9 behaviour that v0.13.0 regressed. Arrival time is
  still exposed as `crawlTimeMsec`. Stream ordering and the `ot=`/`nt=`
  windows keep using the internal sync time, so the v0.13.0 guarantee that
  backdated-but-newly-fetched items reach incremental clients is
  unaffected. Read-time fix only — stored `published` values were already
  correct, so no migration or data rewrite is needed.

## [0.20.0] - 2026-07-16

### Changed

- **GReader API tokens are now deterministic and non-expiring**
  (miniflux/FreshRSS model). The `Auth` token is derived via
  HMAC-SHA256 from the stored password hash and username
  (`username/<hmac-hex>`), so it is stable across restarts, secret,
  and rotates automatically when the password changes. Tokens are no
  longer minted randomly, persisted, or expired, which keeps clients
  (Reeder et al.) signed in indefinitely instead of prompting to
  re-authorize every 30 days. Legacy random tokens already on disk in
  `tokens.json` stay valid — without expiry — until the next password
  change, so upgrading causes zero re-auth prompts. A password change
  clears legacy API tokens and rotates the deterministic one. UI
  cookie sessions are unchanged (still random, persisted, 30-day
  expiry).

### Fixed

- **GReader API 401 responses now use the real-world FreshRSS header
  name `Google-Bad-Token: true`** (was the ineffective
  `X-Reader-Google-Bad-Token` shipped in v0.19.1, which no client
  recognises).

## [0.19.1] - 2026-07-16

### Fixed

- **GReader API 401 responses now carry `X-Reader-Google-Bad-Token:
  true`.** When an API token expires (30-day lifetime), the
  `requireAuth`-protected Reader endpoints return this de-facto GReader
  convention header alongside the 401. Clients (Reeder et al.) key off
  it to silently re-run `ClientLogin` with stored credentials rather
  than prompting the user to log in again, making token expiry
  invisible to the UX.

## [0.19.0] - 2026-07-09

### Fixed

- **Entry identity no longer flips basis between polls — the class of
  "old article resurfaces as new/unread" duplicates is closed.** The
  identity basis (guid-only `D1` vs guid+link `D4`) used to be chosen
  from **unstable** signals: the poll batch window (v0.16.0), a disk
  union (v0.18.0), and item **title**. Three proven failure modes
  resulted — a same-guid sibling scrolling out of the feed window
  flipping D4→D1 (NWR `news/75967` "Star Fox"); an on-disk title vs
  fresh-parsed title differing only by HTML-entity decoding
  (`&hellip;` vs `…`) faking reuse (v0.18.0 inverse dup); and legacy
  D4 copies mismatching a re-listed D1 (CBC, Sarah Andersen).

  Identity is now **`D1 = sha1(NormalizeGUID(guid))` always**, except
  guids in a **persistent, per-feed "sticky reuse set"**, which are
  **`D4 = sha1(NormalizeGUID(guid) \0 link)` forever**. A guid enters
  the sticky set only by **within-single-batch co-occurrence** (a guid
  on ≥2 items in one fetch — count-based, never title/link/disk/time).
  The assigned hash is a **pure function of `(entry fields, sticky
  set)`**: batch composition can only *grow* the set, never re-hash a
  stored item. WordPress slug-drift feeds (guid listed once per batch)
  collapse to guid-only D1; CBC-style distinct articles sharing a
  recycled guid co-occur in a batch, stay D4, stay distinct. The sticky
  set is stored per feed in `entries/<feed-hash>/reused-guids.json`.

### Added

- **`harb migrate --identity` — a one-time, version-gated, idempotent
  migration to the sticky-identity model, with a `--dry-run` preview.**
  The dry-run emits a full human-reviewable report — every collapse
  group (guid, link, title, published, old hashes, read/star state,
  chosen survivor), every sticky-set marking, and an old→new hash map
  artifact (`--map-out`) — and writes nothing, so an operator can
  eyeball it on a copy of live data before the real run. Migration
  seeds the sticky set conservatively by **guid form**: an opaque-token
  guid (e.g. CBC `9.7227935`) with ≥2 distinct on-disk links is genuine
  reuse → marked sticky, kept distinct; a URI-form guid (WordPress
  `?p=N`, Tumblr, `tag:`/`urn:`) is a stable identity → collapsed to one
  survivor. Read/starred state is remapped **in place** through the
  old→new map (survivor read/starred if any collapsed copy was),
  survivor `fetched_at` is the earliest copy's (so it does not resurface
  as new in Reeder), and accounting asserts (`survivor_count = old −
  collapses`, exact starred-count, read state preserved) guard the run.

### Changed

- `Store.AssignEntryHashesForFeed` now drives identity from the
  persistent sticky reuse set (removing the v0.18.0 on-disk union /
  title-comparison path entirely). `Open`'s startup migration is now
  **format-only** (legacy 20→16-char + high-bit-mask canonicalization);
  all identity-basis decisions live in the explicit `harb migrate
  --identity` step.
- `docs/feed-identity.md` rewritten to describe the sticky-set model,
  the pure-function invariant, and the migration.


## [0.18.0] - 2026-07-09

### Fixed

- **Guid-reuse feeds no longer re-duplicate an article when a same-guid
  title sibling scrolls out of the feed window.** The guid-reuse guard
  (which mixes `<link>` back into the identity for feeds that reuse one
  `<guid>` across distinct items) computed its "is this guid reused?"
  verdict over the **poll batch alone**. When two items sharing a guid
  but carrying different titles were both in-window, the guid read as
  reused; once one title aged out of the feed, the survivor's identity
  basis silently flipped, changing its hash and re-storing the same
  article as **new** (and fragmenting its read/starred state). The
  Nintendo World Report feed hit this: `news/75967` "Star Fox Gets Free
  Demo" re-duplicated a month after first ingest. Poll-time hashing now
  computes the reuse verdict over the **union of the feed's existing
  on-disk entries and the incoming batch**, making the verdict monotone
  and batch-independent. No on-disk hashes change and no migration is
  needed — see [docs/feed-identity.md](docs/feed-identity.md).


## [0.17.1] - 2026-06-22

### Fixed

- **Keyboard shortcuts no longer hijack browser/OS modifier chords.**
  The web UI key handlers fired on bare keys (`r`, `s`, `j`/`k`, etc.)
  without exempting Ctrl/Cmd/Alt combinations, so **Cmd-R / Ctrl-R was
  swallowed as "mark read" instead of reloading the page** (and likewise
  for other reserved chords such as Cmd-L). All `keydown` handlers now
  bail out when Ctrl, Cmd (Meta), or Alt is held, letting the browser
  handle the chord; Shift is still allowed, since `N` / `R` / `G` need
  it. Reload, address-bar focus, and friends work as expected again.
- **Timezone-dependent unit test.** `TestPollRetryAfterMissing` formatted
  a local time with `http.TimeFormat` (a literal `GMT` label) and parsed
  it back as UTC, so on any host east of UTC the "past date" case landed
  in the future and the test failed. It now formats in UTC. (CI runs in
  UTC and was unaffected; the failure only bit local dev machines.)

## [0.17.0] - 2026-06-19

### Added

- **App icon / favicon and iOS home-screen support.** The web UI now
  ships a proper icon set — an anchor mark on a harbour-blue gradient.
  Browsers get an SVG favicon (with `.ico` and 16/32px PNG fallbacks);
  iOS/iPadOS get an `apple-touch-icon` (full-bleed square, so Safari's
  own rounded-corner mask applies cleanly rather than double-rounding a
  pre-rounded image). A `manifest.webmanifest` (name "Harbour RSS",
  standalone display, theme/background `#0a2c4a`, 192/512px mascable
  icons) makes "Add to Home Screen" install as a standalone PWA with the
  correct name, icon, and status-bar styling. All assets are served from
  the existing fingerprinted `/static/` route with correct content types
  (`image/svg+xml`, `image/png`, `image/x-icon`,
  `application/manifest+json`); icon/manifest URLs are relative so they
  resolve correctly regardless of the UI mount path.

## [0.16.0] - 2026-06-19

### Fixed

- **The same article no longer appears twice when a feed edits an
  item's link after publishing.** Entry identity now follows the
  RSS/Atom spec: when an item carries a `<guid>` (RSS) or `<id>` (Atom),
  that identifier *alone* is the dedup key. The item's `<link>` is
  location/presentation — the spec explicitly allows it to differ from
  the identifier and to change over time — so it is no longer mixed into
  the identity. Previously a WordPress feed that moved a post's slug or
  category (e.g. `/istorii/<slug>/` → `/novini/<slug>/`) shortly after
  publishing produced a second stored entry with a new hash, surfacing
  the article twice in clients and the web UI. Identity precedence is
  now: guid/id → else link → else title+published (last resort, so
  genuinely distinct linkless/untitled items stay distinct). guid/link
  are whitespace-trimmed (but not lowercased or slash-normalised — IRI
  path/query are case-sensitive). A defensive guard keeps feeds that
  reuse one guid across genuinely-distinct articles (different titles)
  from collapsing. The volatile-pubDate-in-guid normalisation (v0.12.4)
  still applies under guid-only identity. See `docs/feed-identity.md`.

  On first start after upgrade, a one-time migration recomputes every
  stored entry's identity, collapses the duplicate pairs in place, and
  rewrites `read.log` / `starred.log` so read/starred state follows the
  surviving copy. Because affected entries get a new Reader item-id,
  incremental clients (Reeder) re-deliver the surviving copy once and the
  duplicate id disappears — no full resync is required.

## [0.15.0] - 2026-06-19

### Fixed

- **Web UI entry read/star state no longer desyncs across the page.**
  Read/starred state is shown in up to four places (list row, article
  detail pane, the home count badges, the scope pill) and previously
  each mutation hand-patched an ad-hoc subset, so the numbers and the
  two representations drifted until a full reload. Concretely: toggling
  a list row didn't update the open article; toggling the read circle
  never updated any of the home count badges; and detail→row sync was
  partial. The UI is now **server-authoritative**: every state-mutation
  endpoint returns the COMPLETE set of affected fragments as id-targeted
  `hx-swap-oob` swaps carrying absolute, recomputed values — the list
  row, the detail article, and (for read changes) every affected count
  badge (`count-feed`, `count-scope`, per-tag `count-grouphead` /
  `count-side`, `count-side-all`, `count-total`). The client does zero
  state math; htmx silently drops swaps whose target id isn't in the
  current layout. The dwell auto-mark-read and the home master-detail
  "mark feed read" (`r`) paths now apply the same server fragments
  instead of patching counts client-side, and the bespoke
  `decAncestorCounts` / `patchReadBtn` JS is gone. UI-only change;
  `internal/reader` (Google Reader API) is untouched.

  Known limitation: on a tag-filtered home page (`?tag=…`) the page
  total badge first renders the scoped total, while an in-pane toggle
  emits the global total — a cosmetic edge that self-heals on the next
  navigation. The home master-detail is normally unfiltered.

## [0.14.0] - 2026-06-19

### Fixed

- **Keyboard "back" (← / u) from a feed now restores that feed's
  selection on the home master-detail.** Backing out of a feed reloads
  the home page from scratch (bfcache force-reload), which previously
  auto-selected the *first* feed — losing your place. The feed you were
  reading is now re-selected and previewed in the right-hand pane. If
  that feed is now empty (0 unread under the unread-only filter) the
  server omits its row entirely; in that case a dimmed, synthetic
  "returning" row is shown selected with an empty (all-caught-up)
  preview, and Right/Enter re-enters the feed. Pure client-side change
  (`keys.js`), gated to wide screens; stale (>5 min) returns fall back to
  the old first-feed behaviour. (Issue: home back-nav loses feed.)

## [0.13.0] - 2026-06-15

### Fixed

- **Backdated feed items were invisible to incremental sync clients
  (Reeder).** The Google Reader `timestampUsec` — the stream cursor and
  sort key clients use to discover new items — was derived from each
  item's *publish* date. Feeds that backdate items to article time (CBC,
  Penny Arcade, …) therefore received a cursor value in the past, so once
  a client's incremental cursor had advanced beyond it the item was never
  delivered: it stayed unread in the web UI but never appeared in the
  reader. `timestampUsec`, the reading-list sort order, and the
  `mark-all-read` cutoff now use the item's *sync time* (the later of
  publish and fetch), so a late-fetched, old-dated entry arrives as new.
  The displayed article date (`published`/`updated`) and `crawlTimeMsec`
  are unchanged. Stream ordering also gained deterministic
  `published`-then-hash tiebreakers so pagination offsets stay stable when
  many items share a single poll-cycle fetch time.

- **Docs corrected to the `harb` name.** `README.md`, `AGENTS.md`, and the
  bundled `THIRD_PARTY.md` still referenced the pre-rename `harborrs`
  binary, the `kfet/tap/harborrs` Homebrew formula, the `cmd/harborrs`
  package, and the stale `harborrsVersion` Reader-API field — none of
  which exist. They now use `harb` / `kfet/tap/harb` / `cmd/harb` /
  `harbVersion`, matching the shipped binary, so the install and
  quick-start instructions actually work.

## [0.12.4] - 2026-06-09

### Fixed

- **Duplicate entries from feeds whose guid embeds a volatile pubDate.**
  Some feeds (e.g. Nintendo World Report) emit a non-permalink guid of the
  form `<stable-path> <pubDate>`; the pubDate's seconds drift between polls
  (`…:26` → `…:00`), changing the guid and thus the entry hash, so the same
  article was stored twice. The entry hash now strips a strict trailing
  RFC 1123 date from the guid before hashing (`NormalizeGUID`), and a
  one-time migration recomputes affected entries from the normalised guid,
  collapses the duplicates, and remaps read/starred state onto the
  survivor. Only an exact, fully-anchored date tail is stripped, so
  genuinely-distinct items that merely share a link are left untouched.

## [0.12.3] - 2026-06-09

### Fixed

- **Duplicate entries from the pre-mask high-bit hash migration.** Entry
  hashes gained an int64-safe high-bit mask (`sum[0] &= 0x7F`) after some
  entries had already been stored unmasked; on the next poll the masked
  hash no longer matched the stored unmasked one, so the same article was
  stored — and displayed — twice. A storage-level canonicalisation
  (`StoreEntryHash`) now re-masks legacy hashes on load, in the state
  logs, in dedup and lookups; the on-disk migration re-masks and prunes
  the duplicate lines. Reader API item-ids are unchanged.

## [0.12.2] - 2026-06-09

### Fixed

- **Tag group header (and sidebar) unread counts now update when a feed
  is marked read in place.** On the home page, marking a feed read with
  `r` zeroed only that feed row's badge; the enclosing tag group header,
  the matching sidebar tag entry, the sidebar "all" entry, and the page
  total stayed stale until a reload. They are now all drawn down by the
  feed's prior unread count.

## [0.12.1] - 2026-06-09

### Fixed

- **`o` / `→` now opens the source article in the wide-screen split
  view.** The "open source in new tab" shortcut only bound on the
  standalone entry page; in the master-detail entry list (wide screens)
  the article is loaded into the detail pane after page load, so the
  keypress did nothing. The list-view key handler now opens the detail
  pane article's source link directly.

## [0.12.0] - 2026-06-08

### Changed

- **Article reading pane now uses the full page height on wide screens.**
  On the entry-list views (per-feed, unread, starred), the page chrome
  (heading, the unread-only filter, and the "manage feed" disclosure)
  now lives in the left entries column instead of spanning full width
  above the split. The right-hand article pane consequently rises to the
  top of the page and gains the reclaimed vertical space, so long
  entries have room to read instead of being boxed into a short pane
  with empty space above it. Narrow screens are unchanged (the column
  still stacks heading → filter → manage → list).

## [0.11.0] - 2026-06-08

### Changed

- **Feed-management controls now hide behind an on-demand disclosure.**
  On the single-feed view, the RSS feed URL, rename form, tag editor and
  unsubscribe button are tucked behind one collapsed-by-default native
  `<details>` ("manage feed"), so the reading view stays uncluttered.
  Mark-all-read and the unread-only filter remain inline (reading
  actions, not management). Works with JS off.

## [0.10.0] - 2026-06-08

### Added

- **UI auto-refresh.** Authenticated pages now poll a lightweight
  `GET /ui/version` endpoint (~every 50s, paused while the tab is
  hidden) and surface an unobtrusive "new items — tap to refresh" pill
  when new entries or read/star changes have landed since the page was
  served. The pill is dismissable and reloads on tap; the list is never
  auto-swapped, so scroll position and keyboard selection are preserved
  mid-read. The endpoint reports `Store.StateVersion()` as both body
  and ETag and honours `If-None-Match` with a 304, so the poll does no
  OPML load, rendering or disk reads on the hot path.

## [0.9.1] - 2026-06-08

### Fixed

- **Synthesise a display title for feeds whose items have no title.**
  Mastodon RSS items (and some other sources) carry no <title> element,
  which left entry rows and the entry-view header blank. The UI now
  derives a title from a plain-text snippet of the entry body (Content,
  or Summary when Content is link-only) when the item title is empty,
  falling back to "(untitled)" only when there is no usable text.

## [0.9.0] - 2026-06-08

### Added

- **Preserve the forum/discussion link when enriching link-only feeds.**
  Aggregator feeds (Lobsters, Hacker News, Reddit) publish each item's
  body as only a "Comments"/"Source" discussion link, with the external
  article as the entry link. Link-only enrichment (v0.7.11) replaces that
  body with the fetched article, which previously dropped the discussion
  link from view. The original link is now appended to the enriched
  Content as a `<p class="enriched-source-link">…</p>` footer, preserving
  the feed's own anchor label, so the reader keeps a one-click path to the
  forum thread. Entries whose original body has no anchor get no footer.
  Note: enrichment is new-only — entries already enriched before this
  release won't retroactively gain the footer; a backfill may be wanted.

## [0.8.3] - 2026-06-08

### Added

- **Tag auto-complete on the add-feed page.** The "tags
  (comma-separated)" input on `/ui/feed/new` now offers a `<datalist>`
  of every known tag, consistent with the feed-edit view. A small
  progressive enhancement in `keys.js` makes it token-aware: as you
  type, the suggestions complete only the tag after the last comma
  (already-typed tags are preserved and filtered out). With JavaScript
  disabled the plain datalist still completes the first tag.

## [0.8.2] - 2026-06-08

### Added

- **Entry view: `→` (Right arrow) opens the source article** in a new
  window, as an alias for the existing `o` shortcut. Help overlay
  updated to show `o`/`→`.

## [0.8.1] - 2026-06-08

### Performance

- **Poll-hot-path dedup no longer disk-scans.** `AppendEntries` used to
  re-read every `entries/<feed>/*.ndjson` file (current + archives) on
  each poll to build its de-duplication set. With v0.8.0's concurrent
  fan-out this disk read ran on every feed in parallel on the hot path.
  Dedup now consults a snapshot of the in-memory per-feed index
  (`s.idx[feedHash]`) taken under the read lock, eliminating the
  per-poll scan. Per-feed isolation is preserved (the cross-feed
  `byHash` map is never consulted) and the index is a complete superset
  of the on-disk hash set (built from the same glob, append-only, and
  unaffected by archive rollover), so no duplicates can reappear. A
  mid-stream write failure now commits the already-written entries to the
  index (break-then-commit) so they still de-duplicate on the next poll
  without waiting for a restart.

## [0.8.0] - 2026-06-08

### Added

- **Bounded concurrent feed polling.** A refresh cycle previously polled
  feeds one-at-a-time, so a large OPML took the sum of every feed's
  latency to finish. Cycles now fan feeds out across a bounded worker
  pool (default 8 parallel polls), cutting wall-clock refresh time on
  multi-hundred-feed OPMLs roughly by the pool size. The bound is
  deliberate — it keeps a 1000-feed OPML from opening 1000 sockets/fds
  or saturating the uplink at once. Tune with `HARB_POLL_CONCURRENCY`
  (positive integer; invalid/unset falls back to 8) or the
  `Refresher.Concurrency` field. Each poll uses its own feed parser, so
  the fan-out is data-race-free; cancellation mid-cycle stops dispatch
  promptly and drains in-flight polls.

## [0.7.11] - 2026-06-08

### Added

- **Full-text previews for link-only aggregator feeds.** Feeds such as
  Lobsters, Hacker News and Reddit publish each item's body as nothing
  but a bare "Comments" link, so entries showed only a title. Polling now
  detects link-only entries and fetches the linked external article,
  extracting its main readable content (largest `<article>`/`<main>`, else
  the densest paragraph block) to use as the entry body. New-entries-only
  and best-effort: a fetch failure or a page with no extractable article
  simply leaves the entry as before. Applies to every feed, not just
  Webflow-synthesised ones.

## [0.7.10] - 2026-06-08

### Added

- **`o` keyboard shortcut on the entry view** opens the article's
  original source URL in a new browser window/tab (no-op when the entry
  has no source link). Documented in the keyboard-help overlay.

## [0.7.9] - 2026-06-07

### Added

- **Rename a subscribed feed.** The feed page now has an inline rename
  form (an editable title field + a save button) so the display title of
  an already-subscribed feed can be changed. The new name persists to the
  OPML and is reflected on the feed page heading and the home feed list.

### Fixed

- **Entries whose feed body is only a bare link now show a preview.**
  Some feeds (e.g. "News – CIRA") publish `content:encoded` as nothing
  but a lone "Source" link with no article text, which suppressed the
  entry preview. The entry view now detects content with no meaningful
  text or media outside of `<a>` labels and falls back to the feed's
  summary excerpt (the separate source link is unaffected).

## [0.7.8] - 2026-06-06

### Changed

- **Touch swipe gestures revised (revises the 0.7.7 mapping).** The two
  swipe directions are flipped and the action swipe is extended:
  - **Swipe right** (left→right) now goes **up the hierarchy** (was
    swipe-left in 0.7.7) — same code path as `u` / `←`.
  - **Swipe left** (right→left) is now the **context action**, scoped to
    the element the swipe starts on: on an **entry row** a short swipe
    toggles read and a long swipe stars (was swipe-right in 0.7.7); in
    the **article view** (standalone page or split-panel `#detail-pane`)
    a short swipe toggles read and a long swipe stars (new); on a **feed
    row** it enters / drills into the feed.
  - **Live action preview:** while swiping left the target element
    translates under the finger and reveals which action will fire on
    release — a read hint (●) past the short threshold switching to a
    star hint (★) past the long threshold for rows/articles, or an open
    hint (→) for feed rows. Releasing before the short threshold, or
    letting the gesture become a vertical scroll, fires nothing and
    snaps back with no tint.
  - A left swipe that begins inside a horizontally-scrollable element
    (wide code block / table in an article) scrolls that element instead
    of firing an action, and a swipe starting on a link/button in the
    article is left alone. The `?` help overlay documents the new
    mapping.

## [0.7.7] - 2026-06-06

### Added

- **Touch swipe gestures (mobile / touch web UI).** Two touch-only
  gestures mirror the keyboard nav. **Swipe left** (right→left) goes up
  the hierarchy — the same action as the `u` / `←` keys (standalone
  entry view → parent feed; any other non-home page → universal back-up)
  — and shares one code path with them. **Swipe right on an entry row**
  toggles state by distance: a short swipe (≥ 40px) toggles read, a long
  swipe (≥ 120px) stars it, driving the row's existing buttons so htmx
  and the OOB/focus patching keep working. The row translates under the
  finger and snaps back on release. Gestures lock to the horizontal axis
  only once horizontal movement clearly dominates (so vertical scrolling
  and taps are unaffected), never fire from form fields or the help
  overlay, and are documented in the `?` help overlay.

## [0.7.6] - 2026-06-05

### Added

- **Auto-select the first row in split views (desktop).** On wide
  screens (≥ 64em), when a split view loads with at least one visible
  row, the first visible row is now auto-selected and its preview shown
  in the right pane, so you never land on an empty detail pane. Applies
  to the home master-detail (`/ui/`, first feed → `#feed-pane`) and the
  entry-list split (`/ui/feed`, `/ui/all`, `/ui/starred`, first entry →
  `#detail-pane`), and re-applies when navigating between the views and
  on bfcache restores. It is a fallback only — an existing restored
  selection is preferred — respects the unread-only / tag filters, is
  inert on narrow screens, and leaves empty lists showing their
  placeholder. The previewed first entry follows the existing ~0.7 s
  dwell auto-mark-read rule; merely loading a list marks nothing else
  read.

## [0.7.5] - 2026-06-05

### Added

- **Desktop master-detail home view.** On wide screens (≥ 64em) the
  home page (`/ui/`) becomes a two-pane master-detail: feeds on the
  left, the keyboard-selected feed's entries previewed on the right
  (`#feed-pane`, fed by a new `?panel=1` fragment of the feed view).
  Keyboard nav: `j`/`k` move the feed selection and preview its
  entries, `r` marks the whole selected feed read in place, `→`/`Enter`
  drill in to the feed's full entries+article view, `←`/`u` return to
  the feeds list. Narrow/mobile is unchanged — home stays a plain feeds
  list and `Enter` follows the feed link. The `?` help overlay and
  `keys.js` header document the new keys.

## [0.7.4] - 2026-06-05

### Added

- **Copyable RSS URL on each feed page.** The per-feed view now shows the
  feed's source URL in a selectable, monospace field with a **copy**
  button (flashes "copied"). Makes it easy to grab a feed's RSS
  address to share or re-subscribe elsewhere.

## [0.7.3] - 2026-06-05

### Fixed

- **Webflow feeds now show real publish dates.** A Webflow CMS index
  renders the post date as plain text in a `<div>` (e.g. "May 19, 2026"),
  not a `<time datetime>`, so the previous `date_selector` missed it and
  every entry fell back to the fetch time. `webflow-to-feed` now, when the
  selector yields no parseable date, scans the item's full text for the
  first date-like substring ("Month D, YYYY", ISO `YYYY-MM-DD`, or
  RFC3339) and validates it before use, so dates like "May 19, 2026" come
  through correctly.

### Added

- **Full article text for Webflow feed entries.** The Webflow index has
  no article body — it lives in the `.w-richtext` block on each post's
  detail page — so harb now enriches entries by fetching that page and
  taking the largest `.w-richtext` block as the entry content. The
  resolver marks synthesised feeds with a
  `<generator>harb-webflow-to-feed</generator>`; the poller enriches only
  on that marker and only for **new** entries (later polls fetch nothing
  for already-stored posts). Fetches use the SSRF-safe client with a 15s
  per-page timeout, a 5 MiB read cap, and bounded concurrency (4 workers),
  and are strictly best-effort — a slow or broken detail page leaves the
  entry's content empty and never fails the poll.

## [0.7.2] - 2026-06-05

### Changed

- **`webflow-to-feed` now auto-applies as a builtin** — no per-feed
  sidecar required. A feedless Webflow CMS blog index (e.g.
  `https://claude.com/blog`) can be added purely through the web UI:
  paste the URL, the preview succeeds, add. The resolver is registered
  among the builtins applied to every feed (alongside
  `strip-control-chars`) in both the preview and poll paths, with
  sensible zero-config defaults (it excludes `/category/` and `/tag/`
  taxonomy lists so only real posts surface). It remains a strict no-op
  unless the response is HTML carrying Webflow markers that yield at
  least one item, so real XML/JSON feeds and plain non-Webflow HTML pass
  through byte-identical. An explicit sidecar `webflow-to-feed` Spec can
  still override every parameter; a `looksLikeXML` guard makes the
  builtin + sidecar combination idempotent (the second pass sees the
  already-synthesised RSS and does nothing).

## [0.7.1] - 2026-06-05

### Fixed

- **Add-feed preview now applies resolvers**, so feedless pages that only
  become a feed after a resolver Transform — a Webflow CMS blog index
  (e.g. `https://claude.com/blog`) fixed by a `webflow-to-feed` sidecar,
  say — can finally be added through the web UI. Previously the preview
  path parsed the raw HTTP response with gofeed *without* running the
  resolve chain the poller uses, so the preview failed with "Failed to
  detect feed type" and the UI offered no way to add the feed. The
  previewer now loads the same builtins + per-feed sidecar chain
  (`<data-dir>/resolvers/<feedHash>.json`), runs `ShapeRequest` on the
  outgoing request and `Transform` on the response body before parsing.
  No sidecar means behaviour is unchanged (builtins only); the SSRF-safe
  client and 5 MiB read cap are preserved.

## [0.7.0] - 2026-06-05

### Added

- **`webflow-to-feed` resolver primitive.** Subscribe to Webflow CMS
  collection pages (blog indexes that ship no RSS/Atom feed, e.g.
  `https://claude.com/blog`) by synthesising an RSS 2.0 document from the
  server-rendered `.w-dyn-list` / `.w-dyn-item` HTML before gofeed parses
  it. A transform-stage resolver gated on `text/html`, so it never
  touches a real feed; a non-Webflow page or zero scraped items returns
  the body unchanged so genuine breakage still surfaces. Tunable via
  params (`item_selector`, `link_selector`, `title_selector`,
  `date_selector`, `base_url`, `title`, `limit`,
  `exclude_link_contains`, `require_webflow`). Promotes
  `github.com/PuerkitoBio/goquery` from an indirect to a direct
  dependency.

## [0.6.1] - 2026-06-04

### Fixed

- **Unread-count ETag no longer regresses on restart**, which had let
  Reeder (and other GReader clients) miss freshly-polled items — the
  web UI showed N unread while the client saw none. The `unread-count`
  validator is the store's content-version; new entries bumped it in
  memory, but `Open` rebuilt it from the read/starred logs alone, so
  after a restart it fell back below entries still on disk. A client
  whose cached ETag predated those entries was then wrongly served a
  `304 Not Modified` and never re-fetched. `Open` now also folds the
  newest entry `FetchedAt` across all feeds into the content-version,
  so the post-restart validator stays at or above the last append and
  cached-ETag clients always get a `200` when there is genuinely new
  content. Added a store regression test pinning the no-regression
  invariant.

## [0.6.0] - 2026-06-03

### Added

- **Passkey (WebAuthn) login for the web UI.** You can now sign in with
  Touch ID, Windows Hello, a phone, or a security key instead of (or as
  well as) the password. Verification is done by the stdlib-only
  `github.com/kfet/pinopass` library — ES256/P-256, attestation `none`,
  no third-party transitive dependencies. Passkeys are an *alternative*
  to the password for web-UI login only; the password stays as the
  Reader-API path and the recovery path.
  - Enabled via a new `webauthn` config block (`rp_id`, `origin`,
    optional `rp_name`); passkeys are off unless both `rp_id` and
    `origin` are set. WebAuthn requires a secure context (https or
    localhost) in the browser.
  - Registered credentials live in `credentials.json` in the data dir
    (multiple credentials supported — e.g. laptop + phone). Manage them
    on the settings page; remove individually.
  - New routes under `/ui/webauthn/{register,login}/{begin,finish}` and
    `/ui/settings/passkey/remove`; a small `passkey.js` shim drives the
    browser ceremonies (path-prefix-aware, like `keys.js`).

## [0.5.4] - 2026-06-03

### Fixed

- **Tapping an article on mobile (narrow screens) now opens it.** Entry-row
  title links used an htmx media-query trigger
  (`hx-trigger="click[matchMedia('(min-width: 64em)')...]"`) intended to
  swap into the split-panel on wide screens and fall through to native
  navigation on narrow ones. But htmx calls `preventDefault()` on `<a href>`
  clicks *before* evaluating the trigger filter, so on mobile the native
  navigation was cancelled *and* no request fired — the tap was dead. The
  open decision (panel swap vs full-page nav) now lives in `keys.js`
  (`openEntry`); the row is a plain `<a class="entry-link" href>` that also
  works with JavaScript disabled. Modifier-clicks (cmd/ctrl/shift/middle)
  keep opening in a new tab on desktop.

## [0.5.3] - 2026-06-03

### Fixed

- **Magnet (and other navigable-scheme) links in feeds are no longer dropped.**
  Torrent feeds like showRSS use `magnet:` URIs for item links. The entry
  "source ↗" link rendered through `html/template`'s URL filter, which
  rewrote any non-`http(s)`/`mailto` scheme to `#ZgotmplZ`, and the body
  sanitizer's scheme allow-list stripped the same links from item content.
  The link sanitizer now uses a deny-list keyed on the active-content
  schemes (`javascript:`, `data:`, `vbscript:`, `blob:`) and permits all
  navigable schemes (`magnet:`, `tel:`, `ed2k:`, `feed:`, `xmpp:`, …).

## [0.5.2] - 2026-06-02

### Fixed

- Update `.github/workflows/release.yml` to use new `harb` Homebrew tap formula path.

## [0.5.1] - 2026-06-02

### Changed

- **Renamed binary and environments to `harb`**:
  - Binary name is now `harb` (renamed from `harborrs`).
  - Environment variables: `HARB_DATA`, `HARB_ACCESS_LOG`, `HARB_ALLOW_PRIVATE_FETCH`, `HARB_REFRESH_INTERVAL` (renamed from `HARBORRS_*`).
  - Session cookie is now `harb_session`.
  - Frontend LocalStorage/JS references now use `harb.` prefix.
  - Reader API `"product"` is now `"harb"`, and `"harborrsVersion"` is `"harbVersion"`.

## [0.5.0] - 2026-06-02

### Added

- **Per-feed resolver chain + poll observability** (`internal/poll/resolve`,
  `internal/poll/observe`). Feed-specific breakage workarounds are now data,
  not hardcoded branches in the poll hot path: an ordered chain of small,
  named, parameterised resolvers with two hooks — `ShapeRequest` (mutate the
  outgoing request, e.g. set a header a CDN demands) and `Transform` (repair
  the response body before gofeed parses it). Resolvers come from in-tree
  builtins applied to every feed (currently `strip-control-chars`, the former
  `poll.sanitizeXML`) plus an optional per-feed sidecar at
  `<data-dir>/resolvers/<feedHash>.json` (a `[]Spec`) written out-of-process
  by a fixer; harborrs only reads it. Available primitives: `strip-control-chars`,
  `set-header`, `recode-charset` (latin1/windows-1252→UTF-8), `regex-replace`.
  A bad or unknown sidecar spec is skipped, never aborting a poll. Separately,
  every poll outcome is recorded as NDJSON under `<data-dir>/observe/` (with the
  last failing body saved as a `.sample`) so an out-of-process fixer can diagnose
  breakage and emit resolver sidecars. This is pure observability — harborrs
  reacts to nothing it records.
- **Web UI surfaces feed sync failures.** The home page (`/ui/`) now
  shows when feeds are failing to poll. A failing feed (one whose most
  recent poll left a non-zero consecutive error count) gets a ⚠ badge on
  its row whose tooltip carries the consecutive-error count, the last
  error message, and when the feed last synced successfully. A summary
  banner at the top of the page counts the failing feeds and links to
  each — it appears even under the default "unread only" filter (a feed
  can be broken yet have zero unread) and respects the active tag filter.
  Styled with the existing theme CSS variables; overridable like every
  other template.

### Changed

- **Project renamed to "Harbour RSS"; repo and Go module renamed to
  `harb`.** The display name in the web UI (page titles, brand, footer)
  and README is now "Harbour RSS", and the Reader API `/status` document
  carries a new `name: "Harbour RSS"` field alongside the unchanged
  `product: "harborrs"` machine identifier. The GitHub repo moved to
  `github.com/kfet/harb` (old URL redirects) and the **Go module path is
  now `github.com/kfet/harb`** (root package renamed `harborrs` → `harb`).
  Install with `go install github.com/kfet/harb/cmd/harborrs@latest`; the
  old module path keeps resolving only for already-published tags.
  Deliberately **unchanged**: the `harborrs` binary/CLI (still built from
  `cmd/harborrs`), the `harborrs_session` cookie, `HARBORRS_*` env vars,
  the data dir, and launchd/systemd labels — renaming those would break
  live deployments. Slug-based references (release-asset URLs,
  `install.sh` raw URLs and default `REPO`, self-update `DefaultRepo`, CI
  badge) now point at `kfet/harb`.
- **Homebrew tap repo renamed** `kfet/homebrew-harborrs` →
  `kfet/homebrew-tap`. The install incantation changes accordingly:
  `brew install kfet/tap/harborrs` (was `kfet/harborrs/harborrs`).
  GitHub redirects the old repo URL, and the release workflow's
  fine-grained `HOMEBREW_TAP_TOKEN` is unaffected (it targets the repo
  by ID). The release workflow now pushes the rendered formula to the
  new repo.
- **`state/<feed-hash>.json` now records `last_success`** — the time of
  the most recent *successful* sync (a 2xx with entries applied, or a
  304 not-modified). This is distinct from `last_fetched`, which records
  the most recent poll *attempt* and keeps advancing even while a feed is
  failing. Legacy state files without the field read as "never synced"
  until their next good poll. No migration required.
### Fixed

- **The read/star toggle endpoints (`/ui/entry/read`, `/ui/entry/star`)
  now require POST**, matching every other UI mutator. They previously
  accepted any method, so a state change could be triggered by a GET.

- **The web UI now reads entries from the in-memory index instead of
  re-scanning disk on every request.** `unreadCounts`, the feed and
  cross-feed (all/starred) lists, mark-all-read, and single-entry lookup
  used `store.ListEntries` (a `ReadDir` + NDJSON re-parse per feed);
  `findEntry` additionally scanned every entry of every feed. These now
  use `store.IndexedEntries(feedHash)` and `store.EntryByHash(hash)`
  (resolving the owning feed via `FeedHash`), matching the Reader API's
  already-indexed read path. Behaviour is unchanged (same Published-
  descending ordering); the per-request disk I/O is eliminated.

- **Expired API tokens / sessions are now swept from the token store**,
  fixing unbounded growth. Every Reader-API `ClientLogin` persists a new
  opaque token to `tokens.json` and nothing ever evicted the old ones,
  so the file grew without limit. `OpenStore` now prunes entries past
  `TokenLifetime` (and rewrites the file if it dropped any), and each
  token/session issue opportunistically sweeps before persisting.

- **OPML mutations are now serialized through a single
  read-modify-write path**, fixing a lost-update race. The UI handlers
  (`feed/add`, `feed/remove`, `feed/tag`) did `Load → mutate → Save`
  with no lock while the Reader API used its own mutex over the *same*
  `config.FileOPML`, so concurrent edits from the two front doors (or
  two UI tabs) could clobber each other. `FileOPML` now exposes
  `Update(func(*store.OPML) error)` that holds its lock across the whole
  load→mutate→save cycle, and every OPML mutator in both the `ui` and
  `reader` packages routes through it. The reader's now-redundant
  per-server mutex was removed.

### Security

- **Dependency bump: `golang.org/x/net` v0.4.0 → v0.55.0** (and its
  required `golang.org/x/text` → v0.37.0) to clear a batch of
  `golang.org/x/net/html` CVEs that `govulncheck` flagged as reachable
  from both the feed parser (gofeed) and the new HTML sanitizer —
  notably GO-2026-5030 (duplicate-attribute XSS in `x/net/html`),
  GO-2025-3595, GO-2024-3333 and GO-2023-1988. Shipping an XSS
  sanitizer on a known-vulnerable html parser would have been
  self-defeating, so this is bundled with the sanitizer fix. **The
  minimum Go version is now 1.25** (x/net v0.55.0's go directive); the
  remaining `govulncheck` findings are Go-stdlib issues fixed in
  go1.26.3, i.e. a build-toolchain update, not a module change.

- **SSRF guard on all outbound feed fetches.** Polling
  (`internal/poll`) and the add-feed preview (`internal/feedpreview`)
  fetched arbitrary user/redirect-supplied URLs with no destination
  filtering, so a feed pointing at `http://169.254.169.254/` (cloud
  metadata) or an internal `http://10.x/` service could reach into the
  host's private network. Outbound connections now refuse loopback,
  private (RFC1918/ULA), carrier-grade-NAT, link-local and multicast
  addresses. The check runs in the dialer's `Control` hook on the
  *resolved* IP, so it also defeats DNS-rebinding and is re-applied on
  every redirect hop. Opt out for legitimate private/localhost feeds
  with `HARBORRS_ALLOW_PRIVATE_FETCH=1`. (New `internal/safedial`.)

- **Feed HTML is now sanitized before rendering in the web UI**
  (stored-XSS fix). Entry content/summary was previously injected into
  the entry view verbatim as trusted HTML, so a malicious feed could run
  JavaScript in the UI origin. Bodies now pass through an allow-list
  sanitizer (built on `golang.org/x/net/html`, already a transitive
  dependency — no new module): only safe elements/attributes survive,
  `script`/`style`/`iframe`/`object`/`embed`/`svg`/form controls are
  dropped with their contents, `on*` handlers and inline `style` are
  stripped, and `javascript:`/`data:`/`vbscript:` URLs (including
  whitespace/control-char-obfuscated variants) are neutralised. The
  open-links-in-a-new-tab behaviour (`target="_blank"
  rel="noopener noreferrer"`) is preserved.

## [0.4.22] - 2026-05-31

### Fixed

- **Feed fetches now send a `User-Agent` without a disclosure URL** —
  `harborrs/<version>` instead of
  `harborrs/0.1 (+https://github.com/kfet/harborrs)`. Some CDN bot rules
  (observed with Akamai-fronted feeds such as CBC) **tarpit** any
  User-Agent containing a `(+https://…github.com…)` string: the
  connection is accepted but the response is stalled until the client
  times out, even though the identical request with a bare product token
  returns instantly. This was the real cause of CBC feeds never updating
  from the server (≈100% poll failures). Verified from the affected host:
  the URL-bearing UA failed 0/4 while `harborrs/<version>` succeeded
  8/8 — over plain HTTP/2 with keep-alive.

### Changed

- Reverted the HTTP/1.1 + disabled-keep-alive feed transport introduced
  in 0.4.21. It was a workaround for what turned out to be the
  User-Agent issue above; with the UA fixed, the default transport
  (HTTP/2, connection reuse) polls those feeds reliably, so the extra
  configuration was unnecessary complexity. `HTTP_PROXY` / `HTTPS_PROXY`
  / `NO_PROXY` are still honoured — that comes from Go's default
  transport, so routing a feed through a residential-egress proxy needs
  no code change.

## [0.4.21] - 2026-05-31

### Fixed

- Feed fetches now use HTTP/1.1 with connection keep-alive disabled.
  Some CDNs (observed with Akamai-fronted feeds such as CBC) accept the
  TLS connection but then reset Go's reused **HTTP/2** streams with
  `INTERNAL_ERROR` for requests originating from datacenter IPs — so
  HTTP/2 polling failed ~100% from a VPS while a one-shot `curl`
  (HTTP/1.1, fresh connection) from the same host succeeded every time.
  Forcing HTTP/1.1 + fresh connections mirrors the per-invocation `curl`
  behaviour these CDNs tolerate. The poll transport also now honours
  `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`, so a feed only reachable
  from a residential egress can be routed through a proxy without code
  changes. Keep-alive gives no benefit here anyway: polling makes one
  request per host per cycle, a minute apart.

## [0.4.20] - 2026-05-31

### Fixed

- Poller now strips illegal XML 1.0 control characters (C0 controls
  other than tab/LF/CR) from feed bodies before parsing. Go's
  `encoding/xml` (under gofeed) aborts on the first such byte, so a
  single stray `U+0008` anywhere in a feed would silently drop **every**
  item and the feed would stop updating entirely (observed on
  `answer.ai`, which embeds `U+0008` bytes — 6000+ consecutive parse
  failures). Sanitisation is byte-level and encoding-safe for all
  ASCII-superset encodings (UTF-8, ISO-8859-x, Windows-125x); UTF-16
  bodies (detected by BOM) are left untouched. Clean feeds take a
  zero-allocation fast path.

## [0.4.19] - 2026-05-28

### Changed

- Web UI: links inside rendered article content now open in a new
  browser tab. Every `<a>` tag in the entry body that doesn't already
  carry a `target=` attribute is rewritten with
  `target="_blank" rel="noopener noreferrer"` before the body is
  handed to the template, so following a link no longer loses the
  reader's place in the feed list. Author-set `target=` values are
  preserved verbatim.
- Web UI home page: the sidebar no longer lists individual labels as
  bullets — labels already appear as group headers in the feed list,
  so showing them twice was redundant. The sidebar now holds only the
  pinned "All" and "Untagged" affordances.

### Fixed

- Decode HTML entities in feed-supplied **title** and **author name**
  text once at ingestion (and in the add-feed preview), so the web UI
  no longer renders literal entity strings like `&#8216;unintended&#8217;`
  to the user. The decode is applied only to text fields — `summary`
  and `content` (which are HTML) are left untouched, so this does not
  start trusting raw HTML in title fields and introduces no XSS
  surface. Templates continue to escape `&`, `<`, `>`, `'` etc. on
  output as normal.

## [0.4.18] - 2026-05-26

### Changed

- Replace per-feed adaptive poll scheduler with a pull-driven
  `Refresher`. The v0.4.17 scheduler bumped intervals up to 24h on a
  string of 304s and applied multiplicative backoff on errors; the
  result was that well-behaved feeds got pinned to once-per-24h and
  new entries took a day (or longer) to surface. The new model has no
  per-feed cadence at all:
  - A background ticker fires `Refresher.Trigger()` every minute
    (configurable via `HARBORRS_REFRESH_INTERVAL`, default `1m`).
  - A request middleware on `/reader/api/0/*` and `/ui/*` also fires
    `Trigger()` on every request. Fire-and-forget; the response itself
    serves whatever is in the store right now, and refreshed entries
    show up on the client's next sync. No added request latency.
  - `Trigger()` is single-flight: concurrent calls collapse to one
    in-flight cycle. One cycle iterates every feed sequentially and
    calls `Poll` for each.
  - The only per-feed throttle that survives is `RetryAfter`, set
    inside `Poll` when a feed responds 429 / 503. While the cooldown
    window is open, subsequent `Poll` calls return `ErrCooldown`
    immediately without a network round-trip. Default cooldown when
    the response omits `Retry-After` is 15 minutes.

- `store.FeedState` schema: dropped `NextFetch` and `Interval`, added
  `RetryAfter`. Legacy on-disk state files written by v0.4.17 and
  earlier still load — the dropped JSON tags are silently ignored by
  `encoding/json`, and the next `SaveFeedState` rewrites the file
  without them.

- `harborrs poll-once` now calls `Poller.ResetCooldown` per feed
  before polling, so the command still forces a fetch of every
  subscribed feed regardless of any stored 429/503 cooldown.

### Removed

- `Poller.Run`, `Poller.tickOnce`, `DefaultInterval`, `MinInterval`,
  `MaxInterval`, `backoffSeconds`, `clampSeconds*`. The 304
  multiplicative-bump branch and the error-backoff branch are gone.
  Anything that used to depend on adaptive scheduling now uses the
  Refresher or invokes `Poll` directly.

## [0.4.17] - 2026-05-26

### Changed

- Internal refactor (no behaviour change): extract `applyETag(w, r, etag) bool`
  helper to deduplicate the ETag/INM plumbing between
  `serveConditionalJSON` (used by `subscription/list` and `tag/list`)
  and the early-304 short-circuit in `handleUnreadCount`. Same headers,
  same 304 contract — just one source of truth. Doc-only fixes on
  `reedercompat.newRequest` (the doc claimed the Harness token was
  applied here; it's actually applied in `doRaw`) and on
  `store.StateVersion` (now documents the `AppendEntries` bump path
  and the new-entries-only-not-preserved-across-restart restart
  semantics introduced in v0.4.16).

## [0.4.16] - 2026-05-26

### Added

- ETag / If-None-Match on the three poll-hot GReader endpoints
  (`subscription/list`, `tag/list`, `unread-count`). Each response
  emits a strong, quoted ETag plus `Cache-Control: private, no-cache`
  and `Vary: Authorization`; a request that includes
  `If-None-Match: <etag>` matching the current validator returns 304
  Not Modified with no body. Reeder fires these three endpoints on
  every sync, so the 304 path collapses three full round trips per
  sync into ~zero-byte conditional GETs whenever subscription state
  and entry state are unchanged. The `unread-count` handler does the
  INM check before the per-feed unread scan, so a 304 also avoids
  the index walk entirely.

- Validator design:
  - `subscription/list` and `tag/list`: validator is
    `sha256[:8](OPML.Marshal())` — a deterministic short fingerprint
    over the same canonical-sorted XML the server writes to disk, so
    equal OPML → equal ETag across processes.
  - `unread-count`: validator combines the OPML fingerprint with the
    store's content version (`StateVersion()`), encoded as
    `"<opml_fp>.<unix_us>"`. The content version is bumped on every
    successful `SetRead` / `SetStarred` (state-flag flips) and on
    `AppendEntries` (new entries observable via unread counts). It
    is rebuilt from on-disk state-log UpdatedAt timestamps on
    `store.Open` — so state-flag changes survive restarts; new-entry-
    only bumps do not, which manifests as a single forced 200 right
    after restart (acceptable on a single-user, single-writer server).

- New `Store.StateVersion()` method backing the validator and a
  `contentVer time.Time` field on `Store`. `StateVersion` is
  forward-monotonic within a process lifetime.

- 6 new conformance contracts in `internal/reedercompat`:
  - `etag-conditional/subscription-list` — 304 on INM match;
    ETag changes across OPML mutation.
  - `etag-conditional/tag-list` — same shape, mutation via tag set
    change.
  - `etag-conditional/unread-count` — 304 on same state; ETag
    changes across both `SetRead` and `SetStarred`; post-mutation
    re-INM yields 304 with the new ETag.
  - `etag-conditional/unread-count-tracks-content-changes` — new
    entries arriving via `SeedFeed` (AppendEntries internally)
    invalidate the validator. Catches regressions where the
    `unread-count` validator only tracks state-flag mutations and
    misses the new-entry case.
  - `etag-conditional/headers` — `ETag`, `Cache-Control` with
    `no-cache`, and `Vary` including `Authorization` are all
    present on the 200 response across the three endpoints.
  - Reader-level unit tests (`internal/reader/reader_test.go`)
    cover the `matchesINM` edge cases not exposed via the
    conformance API: empty inputs, wildcard `*`, weak-tag `W/`
    prefix on INM, comma-separated multi-tag list, non-match.

- Mutation tests: stripping the state-version from `etagOPMLState`
  makes both the reader unit test and the compat suite fail loudly
  on the read-state and starred-state mutation cases ("ETag did not
  change across SetRead").

## [0.4.15] - 2026-05-26

### Changed

- Reeder/GReader conformance suite (`internal/reedercompat`) — make
  `timestamp-encoding/stream-contents` differentiate the *source* of
  each wire field, not just the unit. `Harness.SeedFeedAt(fetched)` is
  replaced by `Harness.SeedFeedTimes(published, fetched)` so each
  entry can be seeded with disjoint Published and FetchedAt times.
  The test now asserts `published`/`updated`/`timestampUsec` read from
  the entry's Published while `crawlTimeMsec` reads from FetchedAt;
  a regression that swaps the two sources fails loudly with
  "want X (Published, …)" / "want Y (FetchedAt, …)" diagnostics.
  Mutation-tested by swapping `displayTime`/`e.FetchedAt` in
  `toStreamItems` — both failing items report the wrong source.
  New `timestamp-encoding/zero-published-falls-back-to-fetched`
  sub-contract locks the `entryDisplayTime` zero-Published fallback
  path (Published-zero → display time = FetchedAt → all three
  display-time wire slots and crawlTimeMsec align on FetchedAt).
  Suite-public API is now: `SeedFeed(count)` (all-equal Published =
  FetchedAt = now) and `SeedFeedTimes(published, fetched)` (parallel
  per-entry control); the old `SeedFeedAt` is removed and existing
  callers in the suite pass the same slice twice when they don't need
  to differentiate.

## [0.4.14] - 2026-05-26

### Changed

- `FileOPML` is now in-memory authoritative: the parsed `*store.OPML`
  is the source of truth at runtime, with `subscriptions.opml` acting
  as the persistence layer. The file is read and parsed once on first
  `Load` (or `Save`); subsequent `Load`s return a defensive deep copy
  of the in-memory state with no disk I/O. `Save` serializes, atomic-
  writes the file, then on success replaces the in-memory state — a
  failed write leaves the in-memory state untouched (no partial
  state). Public `OPMLProvider` surface unchanged. Measured ~100×
  speedup on the per-`Load` hot path (208 µs → 2 µs for a 60-feed
  OPML on M1); removes a redundant disk read + XML parse on every
  `stream/contents` request (which previously called `OPML.Load`
  twice via `collectStream` + `writeStreamPage`). New tests cover
  read-once, Load isolation, and Save-failure-keeps-state. New
  `BenchmarkFileOPMLLoad` / `BenchmarkFileOPMLLoadColdDisk` lock the
  steady-state vs pre-rework numbers.

- Reeder/GReader conformance suite (`internal/reedercompat`) updated
  to reflect v0.4.8 / v0.4.9 / v0.4.13 contracts:
  - `longid-unsigned-int63/wire-format` (new) — seeds a feed and
    asserts every `id` / `longId` on `stream/items/ids` and every
    16-hex item id on `stream/contents` decodes to a non-negative
    int63. Replaces the now-stale `longid-roundtrip/highbit-negative`
    test (the v0.4.13 sha1-top-bit mask makes high-bit longIds
    impossible by construction). Mutation-tested by un-masking the
    hash: the suite fails loudly with "strict clients drop it".
  - `items-contents-empty/reading-list-stream-id` (new) — POST
    `/stream/items/contents` with no `i` params must respond with
    `id == reading-list` and a fresh `updated` timestamp, not the
    pre-v0.4.13 `{"id":"items","updated":0}` placeholder.
  - `timestamp-encoding/stream-contents` (new) — pins
    `published`/`updated` in seconds, `timestampUsec` in
    microseconds, and `crawlTimeMsec` in milliseconds against a
    fixed-epoch seed; locks v0.4.8 + v0.4.9 wire encoding against
    accidental unit regressions.
  - `ItemLongID` helper now uses unsigned decimal (`FormatUint`) to
    mirror the implementation.

## [0.4.13] - 2026-05-25

### Added

- Reader API perf-regression guard (`TestPerfBudget` in
  `internal/reader/perf_guard_test.go`): rebuilds the ~60-feed ×
  ~33-entry fixture from `perf_test.go` deterministically and asserts
  the measured ns/op for the Reeder-like sync shape
  (12× `stream/items/ids` + 6× `stream/items/contents` + 1×
  `unread-count`) and an all-feeds `IndexedEntries` sweep stay under
  budget (25 ms and 0.5 ms respectively — ~3× / ~7× the v0.4.11 M1
  baseline). Trips if anyone reintroduces a per-request disk scan or
  bypasses the in-memory entry index. Auto-skips under `-short`,
  `-race`, or on architectures other than amd64/arm64.
- Web UI now uses a two-panel layout on wide screens (≥ 64em): the
  entry-list pages (`/ui/feed`, `/ui/all`, `/ui/starred`) keep the list
  on the left and load the selected entry into a sticky detail pane on
  the right via htmx — no full page navigation. Narrow screens
  (phones, small tablets) keep the existing single-column behaviour:
  entry-row clicks follow the native `href` because the htmx swap is
  gated by a `matchMedia('(min-width: 64em)')` trigger filter. The new
  fragment endpoint is `GET /ui/entry?id=<hash>&panel=1`, which
  returns the bare `entry-detail` template (no page chrome) and is
  themable through the existing template-overrides mechanism (the
  `entry-detail` block in `entry.html` is the override point, and the
  split scaffolding lives in `feed.html`). Read/star toggles fired
  from inside the detail pane patch the matching list row in the same
  response via an out-of-band htmx swap, so the two panes stay in
  sync. Keyboard navigation (j/k) on wide screens auto-previews the
  focused row into the detail pane (140 ms debounced).
- Read / star toggles are now icon-only buttons (● / ○ and ☆ / ★) with
  the human action carried in `title=` and `aria-label=` so hover
  tooltips and screen readers still work. Saves horizontal space on
  the entry rows and reads more naturally in the split-panel detail
  view.

### Fixed

- Reader API: ~50% of entries were silently dropped by strict Greader
  clients (Reeder confirmed) because their decimal `longId` exceeded
  signed-int64 max. Root cause: `EntryHash` produced the full 16-hex
  sha1, half of which have the top bit set, decoding to values above
  `2^63-1`. Fix masks the top bit of the first sha1 byte so every
  hash decodes to a positive int63 on the wire. Costs 1 bit of hash
  entropy (still ~63 bits, no collision risk at any realistic scale).
  No migration: entries created before this release retain their
  original hashes and continue to behave as before; new entries
  ingested after upgrade become fully visible to strict clients.
- Reader API: `stream/items/contents` response now sets `id` to the
  reading-list stream and `updated` to the current server timestamp
  (previously `"items"` / `0`). Spec-compliance fix noticed while
  diagnosing the above.
- Reader API: `longId` is now formatted as unsigned decimal
  (`FormatUint`) rather than signed (`FormatInt`). With the
  top-bit-masked hash this is observationally identical for new
  entries, but `itemIDToHash` now accepts both unsigned and signed
  decimal POST bodies so clients caching IDs emitted by harborrs
  ≤ v0.4.12 still resolve correctly.

## [0.4.12] - 2026-05-24

### Fixed

- Reader API: `stream/items/ids?s=user/-/state/com.google/{read,starred}`
  now filters `ot=` / `nt=` against `EntryState.UpdatedAt` (when the
  read or starred flag changed) instead of the entry's fetch/publish
  time. Reeder Classic uses this endpoint as its incremental read-
  state sync; the previous behaviour re-streamed every recently-polled
  item on each sync and clobbered the client's own unread display
  (observed as 18 unread instead of 795 against a 2001-entry library).
  Content streams (`reading-list`, `feed/`, `label/`) keep their
  publish/fetch-time semantics — that contract is unchanged.

### Added

- New `internal/reedercompat` package: a behaviour-only Reeder /
  Google Reader API conformance suite, driven through the public HTTP
  surface plus a small `Harness` the embedder supplies. Hosts the
  previous `TestGReaderCompat` contracts plus dedicated state-stream
  `ot`/`nt` delta-sync tests, and is designed to be lifted out into a
  standalone OSS conformance kit.
- E2E: explicit assertions for the state-stream `ot=` contract, so the
  read-state delta regression class cannot slip past unit-only
  refactors.

## [0.4.11] - 2026-05-24

### Changed

- Reader API (Google Reader / FreshRSS protocol surface): per-call
  latency drops from ~200ms to <1ms by serving stream/contents,
  stream/items/ids, stream/items/contents, mark-all-as-read, and
  unread-count from a new in-memory entry index built at startup and
  maintained on `AppendEntries`. The on-disk NDJSON layout is
  unchanged. Reeder Classic syncs against a 60-feed / ~2000-entry
  library that previously took several seconds now complete in tens
  of milliseconds.
- Reader API: responses are gzip-compressed when the client sends
  `Accept-Encoding: gzip` (every modern Reader client does). Pays
  off most on stream/contents and items/contents, where article HTML
  compresses 5-10×.

## [0.4.10] - 2026-05-22

### Added

- Web UI: icon-style read/star buttons on entry rows and entry detail
  (●/○, ★/☆) with `aria-label`s for screen readers and CSS tints
  (accent for unread, gold for starred).
- Web UI: home feed list is grouped by tag, with a collapsible
  section per tag (state persisted per-tag in `localStorage`). Feeds
  with multiple tags appear under each of them; an "Untagged"
  section trails. Keyboard list-nav walks every visible group and
  skips collapsed ones.
- Web UI: the add-tag input on the per-feed page is backed by a
  `<datalist>` populated with every tag in the OPML, giving native
  autocomplete for existing tags while still allowing free-form new
  ones.

### Changed

- Web UI: home and per-feed views default to "show unread only".
  The user's last choice is persisted in an `h_unread` cookie so it
  carries across the feed list, entering a feed, and back-navigation.
  Toggle pills emit explicit `?unread=0`/`?unread=1` links so the off
  state is also recorded.
- Web UI: the `×` on a tag chip is hidden until the chip is hovered
  or focused, so a feed's tags read as a row of tags rather than a
  row of delete buttons.
- Web UI: `mark-all-read` on a feed now redirects back to the home
  list without re-pinning `?unread=1`, deferring to the user's
  persisted choice.

## [0.4.9] - 2026-05-23

### Fixed

- Restored `crawlTimeMsec` to mean actual fetch/crawl time while keeping
  `timestampUsec` aligned with article published time for Reeder display
  ordering.

## [0.4.8] - 2026-05-22

### Fixed

- Reader API item timestamp fields now use the entry's published time
  for `published`, `updated`, `crawlTimeMsec`, and `timestampUsec`
  (falling back to fetch time only when published time is absent).
  Reeder appears to display/group articles from `timestampUsec`; using
  fetch time there made entries fetched in the same refresh appear with
  identical or batch-grouped dates.

## [0.4.7] - 2026-05-22

### Fixed

- `stream/items/ids` now encodes empty result sets as `"itemRefs":[]`
  instead of `null`, preserving the array shape for Reeder and other
  strict Google Reader clients when `ot` / `nt` filters match no items.

## [0.4.6] - 2026-05-22

### Added

- Lightweight Google Reader / Reeder compatibility contract tests in
  `internal/reader/compat_test.go`, consolidated under one
  `TestGReaderCompat` umbrella with named sub-tests
  (`item-id-16-hex/contents`,
  `item-ref-decimal+longid-int64/items-ids`,
  `longid-roundtrip/highbit-negative`,
  `accept-legacy-20hex+decimal-longid/edit-tag`,
  `item-categories/reading-list+label`, `direct-stream-ids/items-ids`,
  `preserve-i-order/items-contents`,
  `reading-list-all-vs-xt-unread/items-ids`,
  `ot-nt-filters/items-ids`, `it-filter/selects-starred`,
  `continuation-paging/items-ids`,
  `unread-count-newest/per-row+global+output-json`). Failures read as
  contract violations, not generic test diffs. Pure Go + `httptest`
  through the real `Routes` handler; no containers, no FreshRSS
  runtime, no external network. Runs under the normal `make all`.

### Fixed

- `stream/items/ids` now emits decimal signed-int64 `itemRefs[].id`
  values (with matching `longId`) instead of tag-form hex ids, matching
  the Google Reader shape that Reeder uses to join unread item refs to
  later `stream/items/contents` payloads. This prevents freshly synced
  entries from being imported but shown as already read.
- The `reading-list` stream now represents all items; unread-only views
  are produced by adding `xt=user/-/state/com.google/read`, as in
  FreshRSS/Miniflux. The Reader API also honours `ot` / `nt` timestamp
  filters on item-id and stream-content queries so Reeder's read-state
  delta sync does not receive over-broad pages.

## [0.4.5] - 2026-05-21

### Fixed

- Entry hashes are now stored on disk as 16-hex-character Google
  Reader/FreshRSS item ids instead of the old 20-hex SHA-1 prefixes.
  `Store.Open` migrates existing `entries/**/*.ndjson`, `read.log`,
  and `starred.log` in place before folding state, preserving read /
  starred flags and rejecting the astronomically unlikely case where
  two legacy 20-hex hashes collide after truncation. The Reader API now
  emits 16-hex `tag:google.com,2005:reader/item/...` ids plus `longId`,
  and still accepts legacy 20-hex ids and signed decimal long ids on
  write endpoints. This fixes Reeder collapsing every synced item onto
  a single local primary key, which appeared in the app as only one
  visible item in All/Unread despite successful sync traffic.

## [0.4.4] - 2026-05-21

### Added

- Optional access log for the HTTP server, off by default. Opt in
  with `HARBORRS_ACCESS_LOG=1` in the unit-file environment; output
  goes to stderr (so systemd journal picks it up). One key=value
  line per request: `method`, `path`, `status`, `bytes`, `dur_ms`,
  `ua`, `remote`, plus a sanitised `query` summary. Redaction is by
  construction: the middleware never reads Authorization or Cookie
  headers, never reads request or response bodies, and summarises
  the URL query through an allow-list. Safe Reader-API parameters
  (`s`, `n`, `r`, `xt`, `it`, `output`, `ac`, `ts`) are logged after
  stripping embedded feed-URL query strings (private feeds often carry
  `?token=...`); multi-value keys (`i`, `a`, `t`) emit just a count; the
  continuation token (`c`) emits only `<present>`; every other key
  — including `T`, `Auth`, `SID`, `Email`, `Passwd`, `password`,
  `token`, `lsid` — emits `<redacted>` so a value can't leak even
  if a future endpoint adds a new query parameter. The `path`,
  `ua`, and `remote` fields are emitted through `strconv.Quote` so
  a client-controlled byte (e.g. a percent-encoded newline in the
  request path like `GET /foo%0AFAKE`) cannot forge a second
  "access" log line. Unit tests pin every redaction case plus the
  log-injection defence; the e2e smoke runs the binary with the
  flag on and asserts no plaintext credential, password, or
  session cookie reaches the stderr log.

### Fixed

- `stream/contents` now treats a continuation offset beyond the end of
  the stream as an empty page instead of panicking.
- Reader/FreshRSS item sync responses are more Reeder-compatible: item
  contents now carry the reading-list and label categories,
  `stream/items/ids` includes `directStreamIds` and honours `xt` / `it`
  state filters and `r=o` ordering, and `stream/items/contents`
  preserves the requested item order. After adding the harborrs
  instance as a Reader/FreshRSS account in Reeder, items now appear in
  the unread view; previously the empty `categories[]` on stream items
  and missing `directStreamIds` on item refs caused Reeder to silently
  drop them from the feed/unread view. E2E asserts the new contract on
  the typical Reeder unread-sync flow (`stream/contents` →
  `stream/items/ids?s=reading-list&xt=read` → `stream/items/contents`)
  so a future regression fails CI instead of the user.

## [0.4.3] - 2026-05-21

### Added

- The harborrs build version is now exposed unobtrusively in the web UI
  footer (e.g. "harborrs 0.4.2") and on the API side via two surfaces:
  - `GET /status` — small **unauthenticated** JSON document
    `{"product":"harborrs","version":"...","commit":"...","buildDate":"..."}`
    suitable for monitoring, version-pinning, and identifying the
    server before going through the `ClientLogin` dance.
  - `/reader/api/0/user-info` now carries an extra
    `"harborrsVersion": "x.y.z"` field. FreshRSS-compatible clients
    (Reeder, NetNewsWire, etc.) ignore unknown fields, so this is
    backwards-compatible.

### Fixed

- Web UI back-navigation no longer shows stale state. Pressing `u`
  (or the browser Back button) from an entry view after toggling
  read/unread/starred used to show the list with the entry in its
  pre-toggle state until the user hit F5 — both bfcache restores
  and HTTP-cached back-navigations could serve a stale snapshot.
  Authenticated UI pages now ship `Cache-Control: no-store`, which
  forces re-fetch on Back and (per spec) disqualifies them from
  bfcache. E2E asserts the header is set on `/ui/`, `/ui/feed`,
  and `/ui/entry`.
- Web UI keyboard shortcuts are prefix-aware again. After the
  relative-URL refactor in 0.3.1 the bundled `keys.js` still
  hard-coded absolute `/ui/...` paths in two places: the entry-view
  auto-mark-read `fetch()` and the `u` (back-to-feed) selector.
  Under a path-prefix mount (Tailscale Funnel `--set-path=/rss`)
  both silently 404 / mismatch, so entries never auto-marked as
  read and pressing `u` from an entry view did nothing. The base
  template now emits `<html data-ui-base="…">`, `keys.js` reads
  it, and the back-link selector is the relative-friendly
  `[href*='feed?id=']`. The e2e smoke now asserts these contracts
  (no absolute `/ui/...` literals in `keys.js`; every UI page emits
  relative hrefs only; the auto-mark-read endpoint flips `read`)
  so a future regression fails CI instead of the user.

## [0.4.2] - 2026-05-19

### Fixed

- `/reader/api/0/unread-count` now emits `newestItemTimestampUsec` on
  every row — per-feed and the `user/-/state/com.google/reading-list`
  aggregate. The field was missing entirely from the JSON; Reeder iOS
  (and other FreshRSS-compatible clients) use it to decide whether the
  local cache is stale and silently showed zero unread without ever
  calling `stream/items/ids` when it was absent. Empty feeds emit
  `"0"`, matching FreshRSS.
- `/reader/api/0/stream/items/ids` now supports the `c=` continuation
  token (base64 RawURL of `{"o":offset}`), matching `stream/contents`.
  Previously the handler silently capped responses at `MaxPage` (100)
  with no way to walk further, so clients with >100 unread items could
  only ever see the first 100. The `n=` parameter is also now clamped
  to `MaxPage` rather than falling through to the default when larger.
- Absolute-path `Location` headers in two root-level redirects defeated
  the prefix-agnostic UI claim from 0.3.1 when served under a path
  prefix (e.g. Tailscale Funnel `--set-path=/rss`, which strips the
  prefix before forwarding). `GET /` now emits a relative `ui/`
  Location (was `http.Redirect`, which Go resolves to absolute `/ui/`),
  and an explicit `/ui` handler pre-empts `http.ServeMux`'s automatic
  301 canonicalisation to `/ui/` (which also emitted an absolute
  Location) with a relative `ui/` Location of its own. The
  previously-private `relRedirect` helper in `internal/ui` is now
  exported as `RelRedirect`.

## [0.4.0] - 2026-05-16

### Changed

- **Tags replace folders as the feed-organisation primitive.**
  `Feed` now carries a `Tags []string` instead of `Folder string`;
  feeds can be in many tags at once. OPML is now written flat with a
  comma-separated `category` attribute per OPML 2.0; reads merge the
  union of nested-outline parent names and the `category` attribute.
  Pre-existing OPML files load transparently: a single folder becomes
  a single tag, nested `a/b` folders become a single `a/b` tag (no
  splitting). First write rewrites the file in the new layout.
- `subscription/edit` now interprets multiple `a=` / `r=` params as
  add/remove tag operations (previously: overwrite a single folder).
- The "+ add feed" form takes a comma-separated `tags` input instead
  of a single `folder` field.

### Added

- GReader API: `rename-tag` and `disable-tag` endpoints, used by
  Reeder Classic + FreshRSS clients to manage labels.
- Web UI home page now renders a sidebar with pinned `All` /
  `Untagged` rows plus every tag, each with its unread count. The
  feed list filters via `?tag=<name>` (or `?tag=__untagged__`).
- Per-feed view shows clickable tag chips with hx-post add/remove.
- The pseudo-tag name `__untagged__` is reserved (sentinel for the
  no-tags sidebar bucket). User/client attempts to create or rename
  to it are silently dropped at every entry point (UI form, reader
  `subscription/edit`), except `rename-tag dest=` which 400s — that
  endpoint is an explicit user-visible op and deserves feedback.

## [0.3.1] - 2026-05-16

### Changed

- UI now emits only relative URLs in Location headers and templates,
  so harborrs can be served under any URL prefix (e.g.
  `tailscale funnel --set-path=/rss`) without breakage. Every
  rendered `href`/`action`/`hx-*` attribute and every 3xx
  `Location` header is now a relative reference resolved by the
  browser against the effective request URI (RFC 7231 §7.1.2). No
  config knob is needed — the prefix-agnosticism comes from the
  URLs themselves.

### Added

- The "source" link on the entry view opens the article in a new tab
  (`target="_blank" rel="noopener noreferrer"`) so the harborrs UI
  doesn't get clobbered by the article. Marked with a `↗` glyph.
- Pressing <kbd>u</kbd> now prefers `history.back()` when you arrived
  from a same-origin list page — so the "show unread only" pill state
  and scroll position are preserved on the way up. Falls back to the
  canonical parent URL when there's no list-page referrer.
- Keyboard shortcut: <kbd>N</kbd> toggles the "show unread only"
  filter on the home feed list and per-feed entry list (anywhere the
  pill is rendered). No-op on pages without the pill.
- Theme toggle button in the header — small circular button (◐/●/○)
  that cycles **auto → dark → light → auto** on click. Choice is
  persisted in the browser's `localStorage` (key `harborrs.theme`)
  and applied before paint via an inline bootstrap, so there's no
  flash between reloads. The server-side `[ui] theme = …` config
  still acts as the default for users who never touch the toggle.
- Per-feed entry list (`/ui/feed?id=…`) gained the same "show unread
  only" toggle pill as the home feed list. `?unread=1` hides read
  entries; counter shows how many are unread; empty state when nothing
  is unread says "all caught up".
- Keyboard nav: <kbd>u</kbd> now goes "up the hierarchy" universally —
  on the entry view it goes to the parent feed (unchanged), on
  `/ui/feed`, `/ui/all`, `/ui/starred`, `/ui/feed/new`, and `/ui/settings`
  it goes to the home feeds list. Help overlay text updated.
- Entry view auto-marks the entry as read after ~2.5 s of dwell. If
  you navigate away or hit <kbd>m</kbd> first, nothing happens.

### Changed

- Pages restored from the browser's back/forward cache now
  force-reload, so read / star toggles made on an entry are reflected
  on the list when you hit Back — no more F5.

### Fixed

- Auto-mark-read no longer races with a manual "mark unread" click —
  if you click the read/star buttons (or hit <kbd>m</kbd>/<kbd>s</kbd>)
  before the 2.5 s dwell elapses, the timer is cancelled. Previously
  it would silently flip the entry back to read 2.5 s later.
- Auto-mark-read now checks the response status before mutating local
  DOM, so a 4xx/5xx from the server doesn't leave the UI thinking the
  entry is read.
- Theme toggle now sits on the right edge of the header on logged-out
  pages too (was bunched next to the brand).
- Dark mode no longer flashes a white background on overscroll
  (rubber-band scroll). All themes now declare the matching
  `color-scheme` so the browser uses a dark canvas + scrollbars.
- Keyboard-focused row on the home feeds list (`j`/`k`) is now
  visible — previously the `.kb-focus` highlight only had styling for
  entry rows, not feed rows.

## [0.3.0] - 2026-05-14

### Added

- Home page (`/ui/`) gained a "show unread only" filter toggle. With
  `?unread=1` the feed list hides feeds whose unread count is 0, so
  triage-by-feed only walks the feeds that actually have something
  new. Counter on the toggle shows how many feeds qualify. Empty
  state when nothing's unread says "all caught up".
- Each entry row now shows the published timestamp (compact relative:
  `now` / `Nm` / `Nh` / `Nd`, then `Jan 02` for same-year, ISO date for
  older). `title` attribute carries the full local time on hover.
- Web UI: dedicated **add-feed page** at `/ui/feed/new`. GET shows an
  empty form; POST fetches the URL with a bounded HTTP client (15 s
  timeout, 5 MiB read cap) and renders a preview (feed title +
  description + up to 10 recent item titles). User then clicks
  *subscribe* to actually persist the subscription. Home page now
  links to it instead of carrying an inline form.
- New `internal/feedpreview` package wraps gofeed for the preview path
  so the live polling path stays untouched.
- Keyboard help: press <kbd>?</kbd> anywhere to toggle a shortcuts
  cheatsheet. <kbd>j</kbd>/<kbd>k</kbd>/<kbd>gg</kbd>/<kbd>G</kbd>/
  <kbd>Enter</kbd> now work on the **home feeds list** too (previously
  only on entry-list views) — walk down feeds, hit Enter to open. The
  `?` handler uses a capture-phase listener so it isn't shadowed by
  page hotkeys, and the helper ignores keys typed inside any
  `contenteditable` element on top of inputs/textareas/selects.
- New `auto` theme that follows `prefers-color-scheme`. It is now the
  default; existing configs with explicit `light` / `dark` / `sepia`
  continue to work unchanged. Polished default light + Dracula-ish
  dark palette.

### Changed

- `harborrs serve` now prints a clickable `http://host:port/` URL
  instead of the raw `host:port` listen address — terminals (iTerm2,
  Terminal.app, gnome-terminal, VS Code) auto-link it, so Cmd-click
  opens the UI in the browser. Wildcard listens (`:8088`, `0.0.0.0`,
  `[::]`) display as `localhost`.
- "remove feed" moved out of the home feed list (where a stray click
  could unsubscribe by mistake) onto the per-feed page header, next
  to "mark all read", as an explicit `unsubscribe` button with a
  confirm prompt. Cross-feed views (`/ui/all`, `/ui/starred`) don't
  show it — only the single-feed view (`/ui/feed?id=...`).
- "mark all read" on a single-feed view now redirects to the home
  feeds list filtered to unread-only (`/ui/?unread=1`) so you keep
  walking the queue of feeds-with-unread instead of either staring
  at the now-empty feed or context-switching into the flat unread
  list.
- Default theme in `harborrs init`, `config.Default()`, and the UI
  fallback is now `auto`.
- Light stylesheet refresh: sticky header, accent-coloured focus
  ring, pill-shaped unread count, hover-highlighted entry rows, a
  proper card for login + add-feed + settings, blockquote +
  underline styling tweaks for article body.

### Added

- `harborrs passwd [-data DIR] [-password NEW]` — CLI command to change
  the configured password. Reads from `-password`, otherwise prompts on
  stdin. Rewrites only `auth.password_hash` in `config.json`; a running
  `serve` won't pick up the change until restart.
- Web UI: `/ui/settings` with a change-password form (current + new +
  confirm). On success: persists the new hash, revokes every existing
  browser session, clears the current cookie, and bounces to
  `/ui/login?passwd=1` with a friendly notice. Reader API tokens stay
  valid until clients re-authenticate with the new password.
- e2e regression check that `/ui/static/style.css` is non-trivial
  (>4 KB and contains rules for key selectors), so the previous
  stub-CSS regression cannot silently come back.

### Fixed

- Static asset URLs (`style.css`, `htmx.min.js`, `keys.js`,
  `theme.css`) now carry a `?v=<commit>` cache-buster derived from the
  binary's build commit. Combined with a stricter `Cache-Control`
  (`immutable` when the version is pinned, `max-age=60` otherwise),
  a binary upgrade now invalidates the browser cache automatically —
  no more "I upgraded harborrs and the UI still looks unstyled".

### Fixed

- Web UI was rendering with browser-default styling on every page
  except `/ui/login` because the bundled `style.css` only had rules
  for a handful of selectors. Fleshed out to a real (still
  framework-less, ~7.5 KB) stylesheet covering header/nav, forms,
  buttons, feed list, entry list/detail, the three themes
  (light/dark/sepia), and a small-screen breakpoint. Overrides
  (`<config_dir>/overrides/theme.css`) still load after this file
  and continue to win.

### Changed

- README: `brew tap kfet/harborrs` no longer needs the explicit URL
  (proper `homebrew-harborrs` tap repo now exists); install.sh
  arch list updated to include `linux/armv6`.

## [0.2.1] - 2026-05-14

### Added

- Build matrix now also cross-compiles `linux/armv6` (Raspberry Pi 1 / Zero).
- Release pipeline auto-publishes the Homebrew formula to the
  `kfet/homebrew-harborrs` tap on every tagged release (rendered from
  `packaging/homebrew/harborrs.rb.tmpl` using the just-built tarball
  checksums). Requires a `HOMEBREW_TAP_TOKEN` repo secret.

### Changed

- `install.sh` and `harborrs update` now resolve `armv6l`/`armv7l` and
  `GOARCH=arm` to the new `linux-armv6` release asset (previously
  rejected as unsupported).

## [0.2.0] - 2026-05-13

### Added

- **Distribution & self-update**:
  - Homebrew formula (`Formula/harborrs.rb`) — `brew tap kfet/harborrs
    https://github.com/kfet/harborrs && brew install kfet/harborrs/harborrs`.
  - `install.sh` curl-pipe installer for Linux/macOS (linux/amd64,
    linux/arm64, darwin/amd64, darwin/arm64); verifies sha256 against
    `checksums.txt` from the release.
  - `harborrs update [-check] [-version vX.Y.Z] [-repo owner/name]`
    subcommand: downloads the matching release tarball, verifies
    checksum, and atomically replaces the running binary. Refuses to
    run when the binary lives under a package-manager-owned path
    (Homebrew, linuxbrew, /usr/bin).
  - GitHub Actions release workflow on tag push: builds the four-target
    matrix, ships tarballs + `checksums.txt` as release assets.
  - `make release-local`: build the same artefacts locally under
    `dist/` for testing the installer / updater without cutting a tag.
  - `harborrs version` now reports commit SHA + build date alongside
    the semver, populated via `-ldflags -X` at build time.
- `harborrs init` subcommand: one-shot bootstrap that creates the data
  dir, writes `config.json`, and generates (or accepts) a password.
  Flags: `-data`, `-username`, `-password`, `-listen`, `-theme`,
  `-force`. The top-level usage text and `serve`'s missing-config error
  now point at it.
- `make build` target produces `./harborrs`; wired into `make all` so a
  plain `go build` failure is caught without relying on the e2e harness.
- `make build-matrix` cross-compile check for linux/amd64, linux/arm64,
  darwin/amd64, darwin/arm64 (CGO disabled, compile-only). Wired into
  `make all`.
- Web UI: cross-feed views `/ui/all` (unread) and `/ui/starred`.
- Web UI: mark-all-read button on per-feed and `/ui/all` pages.
- Web UI: top nav linking feeds / unread / starred.
- Web UI: read/star toggle buttons on the single-entry view, with htmx
  fragment swap to update the panel in place.
- Web UI: add-feed form and per-feed remove button on the home page.

### Fixed

- Web UI now serves htmx from the embedded asset (`/ui/static/htmx.min.js`)
  instead of an unpkg.com CDN reference. The previous CDN reference
  violated the AGENTS.md requirement that templates and static assets
  ship via `go:embed`, and broke the UI on offline / air-gapped hosts.
- `store.setFlag` no longer mutates in-memory read/starred state when
  the state-log persist fails; the in-memory and on-disk views now
  stay in sync across crashes.

## [0.1.0] - 2026-05-12

### Added

- Initial scaffolding (Makefile, CI, LICENSE, AGENTS.md).
- `internal/atomic`: crash-safe tmp-file-and-rename file writer.
- `internal/store`: on-disk storage layer — OPML parse/write, feed/entry
  hashing, NDJSON entry append with archive rollover, append-only state
  logs (read/starred) with periodic compaction, and per-feed conditional-
  GET state files.
- `internal/poll`: feed poller — gofeed-based parsing, conditional GETs
  (ETag / Last-Modified), 304 handling, exponential backoff on errors,
  `Retry-After` honouring on 429/503, and a simple `Run` loop ticking
  feeds whose `NextFetch` has passed.
- `internal/auth`: single-user credential (stdlib SHA-256 salt-and-stretch),
  opaque API + cookie-session token store persisted to `tokens.json`,
  ClientLogin-style token extraction helpers.
- `internal/reader`: Google Reader API subset — ClientLogin, token,
  user-info, subscription list/edit/quickadd, tag list, stream contents
  (per-feed/label/starred/read/reading-list), item id queries, item
  contents lookup, edit-tag (read/starred), mark-all-as-read with `ts`
  cutoff, unread-count.
- `internal/ui`: htmx + html/template web UI — cookie-session login,
  home (feed list with unread counts), per-feed entry list, single-entry
  view, read/star toggles via hx-post returning fragment rows. Theme via
  `[ui] theme` config (light/dark/sepia) and optional overrides at
  `<config>/overrides/templates/*.html` and `<config>/overrides/theme.css`.
- `internal/config`: JSON config + FileOPML provider.
- `cmd/harborrs`: subcommands `serve`, `import`, `poll-once`, `hashpass`,
  `version`.
- `make e2e`: end-to-end smoke that builds the binary, polls a canned
  RSS feed, exercises ClientLogin → subscription/list →
  stream/contents → edit-tag → unread-count plus a UI login + home.
