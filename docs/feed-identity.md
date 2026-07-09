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

### Exception: broken feeds that reuse a guid — the sticky reuse set
Some feeds wrongly emit the **same guid on multiple distinct items** (CBC
recycles a stale numeric story id across two unrelated articles). For such a
guid harb mixes `<link>` back into the key (basis **D4** =
`sha1(NormalizeGUID(guid) \0 link)`) so genuinely-distinct items are not
collapsed. Every other guid uses **D1** = `sha1(NormalizeGUID(guid))`.

The set of D4 guids is a **persistent, per-feed "sticky reuse set"**, and it is
the **only** source of D4. It is maintained by one rule:

> **Detection = within-single-batch co-occurrence.** A normalized guid that
> appears on ≥2 items *within one fetch/parse pass* is publisher-proof of reuse.
> Detection is **count-based only** — it never compares titles, never compares
> against on-disk entries, and never looks across polls.

Once a guid is detected reused it is added to the feed's sticky set
(`entries/<feed-hash>/reused-guids.json`) and **stays marked forever**
(monotone). Routing is then trivial: a marked guid → D4 permanently; every other
guid → D1.

Why this is correct on real feeds:
- **WordPress slug-drift** feeds list a given guid **once per batch** (only the
  current slug is live), so the guid is never marked, link never enters its
  identity, and the drifted-link copies collapse to the guid-only D1 identity.
- **CBC-style** distinct articles sharing a recycled guid **do co-occur** in a
  single fetch → marked → kept distinct via D4.

Title drift (HTML-entity decoding `&hellip;` vs `…`, editorial re-heading) is
**never** consulted, closing the v0.18.0 title-drift inverse-dup. Published date
is never consulted either.

#### The core invariant
> The assigned hash is a **pure function of `(entry fields, sticky set)`**. No
> batch composition, no on-disk state, no title, and no published date enters
> the hash.

Batch composition and ordering affect only *which* guids get **added** to the
sticky set — never the hash computed from a given `(fields, set)` pair. Because
the set only ever grows, an already-stored item can never re-hash under a
different basis, which is what ends the re-duplication bug class (proven by a
property test that polls permuted batch compositions and asserts no item is ever
stored under two hashes).

**Known residual (accepted):** the *first* time a guid becomes reused, an
already-stored solo-D1 entry yields one transition duplicate. Rare, and erring
toward keeping an extra copy never loses data. A second residual: an opaque-guid
feed whose *same* article merely drifts its link is marked sticky by the
migration seeding (below) and keeps one historical duplicate — again the
data-preserving direction, surfaced in the migration dry-run for review.

The basis is therefore **monotone and never title- or date-based**: a guid moves
D1 → D4 at most once (when first seen reused) and never moves back.


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
1. recomputes every stored entry's hash under the new rule, collapsing the
   resulting duplicates to one survivor per identity, and
2. **remaps `read.log` and `starred.log` through the same old→new map** in
   place, so the surviving entry of a collapsed duplicate inherits its
   read/starred state.

Skipping (2) causes mass re-duplication and lost triage state.

### The one-time sticky-identity migration (`harb migrate --identity`)
The v0.19.0 move to the sticky reuse set ships a dedicated, **version-gated,
idempotent** migration in `internal/store/idmigrate.go` (`MigrateIdentity`),
exposed as a subcommand:

```
harb migrate --identity --dry-run   # preview: full report, writes nothing
harb migrate --identity             # apply (no-op if already applied)
harb migrate --identity --dry-run --map-out old-new.json   # also dump the hash map
```

It is a **separate explicit step** (never run implicitly at `serve`/`Open`) so
the operator can eyeball the dry-run report on a copy of live data first. What it
does:

1. **Seed the sticky set conservatively from disk.** The disk contains dups
   *produced by the old bugs*, so a same-guid group with ≥2 on-disk entries is
   **not** blindly marked reused. The discriminator is the **guid form**:
   - a same-guid group with ≥2 distinct links whose guid is an **opaque token**
     (not an absolute URI — CBC `9.7227935`, `editorial/75136`) is genuine
     publisher reuse → **mark the guid sticky**, keep members distinct (D4);
   - a **URI-form** guid (WordPress `?p=N`, Tumblr post URL, `tag:`/`urn:uuid:`)
     is a stable publisher identity contract → **do not mark**; link/title drift
     is the same item edited → collapse to one survivor (D1).
2. **Recompute + collapse.** Every entry hash is recomputed under the new scheme
   and duplicates collapse to one survivor. Survivor `fetched_at` = the
   **earliest** copy's, so `timestampUsec = max(published, fetched)` does not
   resurface a survivor as new in Reeder.
3. **State remap in place.** `read.log` / `starred.log` hash columns are
   rewritten (copy + atomic swap), never appended — appended lines would carry
   new timestamps and shift GReader ordering/cutoff. A survivor is read if any
   collapsed copy was read, starred if any was starred.
4. **Accounting asserts** guard the run and abort before writing on any
   violation: every old hash maps, `survivor_count = old_count − intended
   collapses`, exact starred-count preservation, read state preserved.

Every collapse group and every sticky marking appears in the dry-run report
(guid, link, title, published, old hashes, read/star state, chosen survivor)
plus the full old→new hash map artifact. The migration is a **fixed point** — a
second run finds single-member groups and unchanged hashes — and version-gated
by an `identity-migration.json` marker so it runs once.

For entries whose hash is **unchanged**, Reeder item-ids stay byte-stable
(item-ids derive from the stored hash; verified by `reedercompat`).

`Open` still performs a small **format-only** canonicalization
(`migrateEntryHashes`: legacy 20→16-char truncation + high-bit mask, collapsing
exact-format duplicates). That path never changes an identity basis — all
data-affecting identity decisions live in `MigrateIdentity`.

## History

- **v0.12.3** — high-bit mask drift (unmasked stored hash vs masked re-poll).
- **v0.12.4** — volatile **guid** (drifting pubDate-in-guid); `NormalizeGUID`
  strips the date tail. *(guid drifted, link stable.)*
- **v0.15.0** — volatile **link** (WordPress slug/category edited post-publish
  while guid stays stable). Identity moved to **guid-only when present**; this
  document. *(link drifted, guid stable — the mirror image of v0.12.4.)*
- **v0.16.0 – v0.18.0** — guid-reuse guard iterations that keyed the D1/D4
  decision on **title** and/or an **on-disk/batch-window union**. Both leaked an
  unstable signal into identity: a per-batch verdict flipped when a same-guid
  sibling scrolled out of the window (NWR `news/75967`), and the disk-union +
  title-drift variant produced an inverse dup when an on-disk title differed from
  a fresh-parsed one only by HTML-entity decoding. **Superseded.**
- **v0.19.0** — the **sticky reuse set**. D1 always, except guids in a
  persistent per-feed set (detected purely by within-batch co-occurrence) which
  are D4 forever. The assigned hash is a pure function of `(entry fields, sticky
  set)` — never title, batch, disk, or date. Ships the one-time
  `harb migrate --identity` migration (opaque-token vs URI-form seeding,
  collapse, in-place state remap). *(ends the D1↔D4 flip bug class.)*

## Source

- RSS 2.0 spec — `<guid>` element (RSS Advisory Board / Harvard cyber.law).
- RFC 4287 (The Atom Syndication Format) §4.2.6 `atom:id`.
- Code: `internal/store/hash.go` (`EntryHash`, `NormalizeGUID`,
  `IsAbsoluteURIGUID`, `reusedInBatch`, `CanonicalEntryHash`, `StoreEntryHash`),
  `internal/store/store.go` (`AssignEntryHashesForFeed`, sticky sidecar),
  `internal/poll/poll.go` (ingestion), `internal/store/idmigrate.go`
  (`MigrateIdentity`), `internal/store/migrate.go` (format-only Open migration).
