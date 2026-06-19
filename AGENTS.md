# AGENTS.md

Project brief for AI agents working on **Harbour RSS** (binary `harb`).

## What this is

**Harbour RSS** (binary `harb`) is a self-hosted RSS server. One binary,
one user, plain-text
storage. It speaks the **Google Reader API** dialect (the FreshRSS
flavour) so existing RSS clients — Reeder Classic, NetNewsWire, Fiery
Feeds, ReadKit, Unread, lire, Newsify — can sync against it with no
extra adapters. It also serves an embedded htmx web UI on the same
port, themeable via overrides.

Inspirations: NewsBlur, FreshRSS, Miniflux. Differences: simpler, no
DB, single-user-first, stdlib-leaning Go.

## Scope (what's in v0.1)

- **Storage on disk**, no SQL. See "Storage model" below.
- **Polling loop** with ETag / Last-Modified conditional GETs and
  adaptive per-feed intervals + error backoff. Each refresh cycle fans
  feeds out across a **bounded worker pool** (default 8; override via
  `HARB_POLL_CONCURRENCY` or `Refresher.Concurrency`). The bound exists
  so a 1000-feed OPML can't exhaust fds or saturate the uplink. Poll is
  goroutine-safe end-to-end: a fresh `gofeed.Parser` per poll (gofeed is
  not goroutine-safe), the `store.Store` guards its in-memory indexes
  and writes per-feed files, and cycles stay single-flight.
- **OPML import / export**.
- **Google Reader API subset** sufficient for Reeder Classic to sync,
  triage (mark read / starred), and refresh:
  - `/accounts/ClientLogin`
  - `/reader/api/0/token`
  - `/reader/api/0/user-info`
  - `/reader/api/0/subscription/list`, `/edit`, `/quickadd`
  - `/reader/api/0/tag/list`, `/rename-tag`, `/disable-tag`
  - `/reader/api/0/stream/contents/...`
  - `/reader/api/0/stream/items/ids`, `/contents`
  - `/reader/api/0/edit-tag`
  - `/reader/api/0/mark-all-as-read`
  - `/reader/api/0/unread-count`
- **Web UI**: htmx + server-rendered templates from `embed.FS`,
  themeable via a config-dir overrides directory. Cookie-session auth,
  separate from the Reader API token path.
- **Single binary** with subcommands: `serve`, `import`, `poll-once`.

## Out of scope (v0.1)

- Multi-user. Code paths should make it possible later (every state
  fold is keyed by user implicitly), but the binary serves one user.
- Full-text search (defer; SQLite FTS5 or bleve is a v0.2 conversation).
- NewsBlur-specific API. Reader API is the only protocol surface.
- Social features (sharing, blurblogs, follow-users). Never.
- Mastodon / Reddit / YouTube synthetic feeds. Maybe later.

## Storage model

Filesystem-only, single-user. Layout under the config / state dir
(`$XDG_DATA_HOME/harb` or similar):

```
subscriptions.opml          # source of truth for feeds + tags
state/<feed-hash>.json      # etag, last-modified, last-fetched, last-success, error count
entries/<feed-hash>/
  current.ndjson            # hot file: last ~30 days
  2024-Q3.ndjson            # immutable archives, rolled over on poll
  2024-Q4.ndjson
read.log                    # append-only state log: "<ts> r <entry-hash>"
starred.log                 # append-only
```

- Feeds carry zero or more **tags** (many-to-many, flat — no nesting).
  OPML writes the tag list as a comma-separated `category` attribute
  per OPML 2.0; reads also accept nested folder outlines and merge
  their parent name into the tag list.

- `<feed-hash>` = sha1(feed URL) prefix. `<entry-hash>` = the item’s
  **identity** hash: `guid`/`atom:id` when present, else `link`, else
  title+published — the `<link>` URL is NOT mixed in when a guid exists.
  See **[docs/feed-identity.md](docs/feed-identity.md)** for the full
  RSS/Atom first-principles rule, normalization rules, and the migration
  requirement when identity inputs change.
- **State log fold**: on startup, read `read.log` / `starred.log` into
  `map[entryHash]EntryState`. Append on mutation; compact periodically
  when log size > 10× live set.
- **OPML write**: tmp-file-in-same-dir + `fsync` + `os.Rename`. There is
  no `WriteFileAtomic` in stdlib; this is the primitive. Use the
  helper, not `os.WriteFile`, for OPML.
- **NDJSON append**: `O_APPEND` with one JSON object per line; rely on
  POSIX `O_APPEND` atomicity for sub-PIPE_BUF (4KB) writes. Larger
  entries (rare) go through the atomic helper.
- **Archive rollover**: on poll, if `current.ndjson` has entries older
  than the cutoff, split → append-to-quarter-archive + rewrite current.
  This is the only NDJSON rewrite path.

## API auth

Two front doors, one credential:

- **Reader API** (`/reader/*`, `/accounts/ClientLogin`): the legacy
  Google `ClientLogin` dance. Login returns an `Auth=...` token;
  clients send `Authorization: GoogleLogin auth=<token>` (and Reeder
  uses `T=<token>` as a write-token via `/reader/api/0/token`).
- **Web UI** (`/ui/*`): standard cookie session, plain HTML login form.

Both verify against the same single-user password stored in
`config.toml` (hashed). Tokens are random opaque strings, persisted to
a `tokens.json` file (small, easy to inspect).

## Web UI

- **htmx + Go `html/template`**. No build step. Templates embedded via
  `//go:embed`.
- **Overrides**: on startup, after parsing embedded templates, parse
  any matching files in `$config_dir/overrides/templates/`. Last parse
  wins → user templates shadow embedded ones by name.
- **Theming**: ship 3 built-in themes as CSS-variable presets (light,
  dark, sepia). Selected via `[ui] theme = "..."` in config. User can
  drop a `$config_dir/overrides/theme.css` that loads after the bundled
  theme. Optional `[ui.custom_vars]` table injected as a `:root { ... }`
  block.
- **Auth**: cookie session. htmx requests inherit cookies automatically.

## Constraints

- **Stdlib-mostly.** The acceptable third-party dependencies right now
  are `github.com/mmcdole/gofeed` for feed parsing and
  `github.com/kfet/pinopass` for WebAuthn/passkey verification (itself
  stdlib-only, zero transitive deps). **All other dependencies require
  an aside-advisor escalation first.**
- **Go 1.25+.** (Raised from 1.22 when `golang.org/x/net` was bumped to
  v0.55.0 to clear html-parser CVEs under the feed HTML sanitizer; that
  release's go directive is 1.25.)
- **No global state.** No `init()` registries. Constructor returns a
  `*Server` value; HTTP handlers are methods on it.
- **Tests run real polling against real local HTTP servers** spun up
  with `httptest.NewServer`. No mocking the HTTP client.

## Workflow

- `make all` runs gofmt + go vet + staticcheck (if installed) + race
  tests with a **100% coverage gate** (excluding paths in `.covignore`).
  Must pass before any commit. **Do not weaken this gate** to make
  tests pass — instead, add a `.covignore` entry with a justifying
  comment, or write the test.
- Add a `## [Unreleased]` entry in `CHANGELOG.md` for every
  user-visible change.
- Update `README.md` and this file when scope or storage layout
  changes.

## Advisor cadence (mandatory)

Use `aside` with `escalate=true` (the aside-advisor skill) at minimum
at these points. Skipping them is a process error:

1. **Before writing the first line of storage code** — confirm the
   storage layout, atomic-write strategy, and hashing scheme.
2. **Before designing the Reader API handlers** — confirm endpoint
   shapes, especially `stream/contents` pagination and `edit-tag`
   semantics, which are under-documented.
3. **Before the polling scheduler** — confirm adaptive interval and
   backoff policy.
4. **Before the web UI / template layout** — confirm the overrides
   resolution rules.
5. **Before declaring v0.1 done** — sanity-check the whole shape.

Plus the standard triggers from the aside-advisor skill: stuck,
considering an approach change, about to declare a task done.

## Test harness (continuous verification)

There must be an end-to-end smoke target that:

1. Spins up `harb serve` in a temp dir.
2. Imports a small OPML pointing at an `httptest.NewServer` that serves
   a canned RSS feed.
3. Polls once (via subcommand or by sleeping past the next-fetch).
4. Hits the Reader API endpoints in order:
   `ClientLogin` → `subscription/list` → `stream/contents` →
   `edit-tag` (mark read) → `unread-count` (expect 0).
5. Hits the web UI: GET `/ui/`, assert the entry appears; POST
   `hx-post` to mark unread; assert.

Wire it as `make e2e` and have CI run it. Fast (sub-30s). Re-run after
every meaningful change.

## Reference repos

When in doubt about repo conventions (Makefile, CI, release flow,
CHANGELOG format, doc.go style), look at:

- `github.com/kfet/pinexec` — most similar build/test scaffolding
- `github.com/kfet/pinoauth` — auth-token shape ideas
- `github.com/kfet/covgate` — used by the coverage gate in `make all`

Clone them into `/tmp` for reference; do not vendor them.
