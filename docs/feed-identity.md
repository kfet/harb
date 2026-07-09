# Feed entry identity & de-duplication

How harb decides whether two items seen across polls are the **same entry**.
This is identity, not display: getting it wrong shows the same article twice
(or merges two distinct ones). Grounded in the RSS 2.0 and Atom specs below.

## First principles (the specs)

### RSS 2.0 — `<guid>`
- The guid is "a string that uniquely identifies the item. When present, an
  aggregator **may choose to use this string to determine if an item is new**."
  → The guid is the identity signal. The spec never directs aggregators to use
  `<link>` for identity.
- `isPermaLink` attribute, **default `true`**. If `false`, "the guid may not be
  assumed to be a url." When `true` it is a permalink URL, but it is **not
  guaranteed equal to `<link>`** — a guid permalink may legitimately differ
  (on-site permalink vs external article link, comment anchors, …).
- `<guid>` is optional. If absent, fall back to `<link>`, then to a
  title+date hash.

### Atom (RFC 4287) — `<id>`
- `atom:id` is **required**, **MUST be an IRI**, and is **permanent**:
  "When an Atom Document is relocated, migrated, syndicated, republished,
  exported, or imported, the content of its atom:id element **MUST NOT
  change**." (§4.2.6)
- Two entries are the same logical entry **iff** they carry the same
  `atom:id`. `atom:link rel="alternate"` is the *location*, separate from id.
- gofeed maps `atom:id` → the universal `Item.GUID`, so RSS guid and Atom id
  share one code path in harb.

### Why mixing `<link>` into identity is WRONG
`<link>` is **location / presentation**. The specs explicitly allow it to
differ from the identifier and to change over time. Publishers edit slugs and
move articles between sections; CMSes (WordPress especially) rewrite the
permalink after publish while keeping a stable guid. If link is part of the
identity key, every such edit mints a phantom "new" entry — the exact
duplicate-article bug.

## Identity precedence (the rule harb implements)

```
identity = NormalizeGUID(trim(guid))         if guid non-empty
         else trim(link)                      if link non-empty
         else hash(title + published RFC3339) last resort
```

- **guid / atom:id wins** whenever present. Link is never mixed in when a guid
  exists (see the one exception below).
- **link** is the fallback for feeds with no guid.
- **title+published** is the last-resort so distinct untitled & linkless items
  do not all collapse to one id.

### Exception: broken feeds that reuse a guid
Some feeds wrongly emit the **same guid on multiple distinct items**. If a
batch (poll or migration) contains one guid on ≥2 items whose **title** differs,
that feed misuses guid as a non-unique value; for that guid harb falls back to
including link in the key, so genuinely-distinct items are not collapsed.

Distinctness keys on **title only**, deliberately **not** on `<link>`: link is
location/presentation and is explicitly allowed to drift for the *same* article
(the whole point of this change), so a differing link is not evidence of a
distinct item — keying on it would refuse to collapse slug-edited twins,
especially during migration where both historical link variants of one article
coexist on disk. Live-data validation confirms the necessity: e.g. CBC reuses a
stale story id (`9.7050838`) across two genuinely-distinct articles whose links
carry different story numbers — title-distinctness keeps those apart, while
same-title link-only drift (the WordPress bug) still collapses. The residual
cost is that an article whose publisher edits **both** its title and its slug is
kept as two entries (can't be distinguished from genuine guid-reuse) — erring
toward keeping distinct never loses data.

#### Batch-independence guarantee
The reuse verdict must be **stable across polls**, not a function of which items
happen to share the current feed window. A verdict computed over the poll batch
alone flips an item's basis the moment a same-guid title sibling scrolls out of
the feed: a guid that read as *reused* (link mixed in, D4) while both titles were
in-window silently reverts to *not-reused* (guid-only, D1) once the sibling ages
out, changing the hash and re-storing the same article as new. This bit the
Nintendo World Report feed, which re-duplicated `news/75967` ("Star Fox Gets
Free Demo") a month after first ingest when its typo-titled sibling left the
window.

So at **poll time** harb computes the verdict over the **union of the feed's
existing stored entries and the incoming batch** (`Store.AssignEntryHashesForFeed`),
seeding the first-seen title map from `s.idx[feedHash]` before folding in the
batch. The verdict is then **monotone**: once a guid carries two distinct-title
entries on disk it stays reused permanently, so an item's identity basis never
flips with window composition. This changes **no existing on-disk hash** and
needs no migration — it only pins which basis a *new* entry is stored under. The
pure batch-only `AssignEntryHashes` is retained for migration and callers without
a feed context.

The one residual case: if a same-guid title variant appears *after* the original
was already stored solo, the original's first poll (which had no sibling on disk
or in batch) may still produce a single duplicate at the instant the second title
first arrives. That is pre-existing and accepted — solving it via a link/title
basis would regress slug-drift collapse (above).


## Safe vs unsafe normalization

| Operation | Verdict | Why |
|---|---|---|
| Trim surrounding ASCII whitespace | **safe** | stray whitespace is never significant |
| Strip a trailing volatile RFC 1123 date (`NormalizeGUID`) | **safe, kept** | some feeds (NWR-class) append a drifting pubDate to the guid; only an exact anchored date tail is stripped |
| Lowercasing | **unsafe** | IRI path/query are case-sensitive (only scheme + host are case-insensitive) |
| Add/strip trailing slash, percent-decode | **unsafe** | not guaranteed equivalent; would merge distinct URLs |

## Hash format (wire constraints — do not change lightly)

`EntryHash` returns a **16-hex-char (8-byte) sha1 prefix** — the Google Reader /
FreshRSS item-id convention, and the size that fits the signed-int64 ids Reeder
and other clients use. The **high bit of the first byte is masked off**
(`sum[0] &= 0x7F`): Reeder silently drops items whose `longId` exceeds int64
max (≈ half the feed goes missing). Both constraints are load-bearing — keep
them. `CanonicalEntryHash` / `StoreEntryHash` normalise legacy on-disk hashes;
`CanonicalEntryHash` is what the Reader API item-id round-trip uses, so already
-issued ids stay stable.

## Changing identity inputs requires a migration (hard-won lesson)

Any change to what `EntryHash` consumes **must** ship a migration that:
1. recomputes every stored entry's hash from `(guid, link, title)` with the new
   rule, and
2. **remaps `read.log` and `starred.log` through the same old→new map**, so the
   surviving entry of a collapsed duplicate inherits its read/starred state.

Skipping (2) causes mass re-duplication and lost triage state. The machinery
lives in `internal/store/migrate.go` (`migrateEntryFile` collapses intra-file
dups keeping the first occurrence; `migrateStateLog` rewrites the logs via
`remap`). Validate every such change against a **copy of live data** before
release — assert the target dups collapse, starred count is unchanged, read
state is preserved, and that feeds which legitimately share a link across
distinct guids do **not** collapse.

## History

- **v0.12.3** — high-bit mask drift (unmasked stored hash vs masked re-poll).
- **v0.12.4** — volatile **guid** (drifting pubDate-in-guid); `NormalizeGUID`
  strips the date tail. *(guid drifted, link stable.)*
- **v0.15.0** — volatile **link** (WordPress slug/category edited post-publish
  while guid stays stable). Identity moved to **guid-only when present**; this
  document. *(link drifted, guid stable — the mirror image of v0.12.4.)*

## Source

- RSS 2.0 spec — `<guid>` element (RSS Advisory Board / Harvard cyber.law).
- RFC 4287 (The Atom Syndication Format) §4.2.6 `atom:id`.
- Code: `internal/store/hash.go` (`EntryHash`, `NormalizeGUID`,
  `CanonicalEntryHash`, `StoreEntryHash`), `internal/poll/poll.go` (ingestion),
  `internal/store/migrate.go` (migration).
