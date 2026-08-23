# Harbour RSS

[![CI](https://github.com/kfet/harb/actions/workflows/test.yml/badge.svg)](https://github.com/kfet/harb/actions/workflows/test.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kfet/harb.svg)](https://pkg.go.dev/github.com/kfet/harb)
[![Go Report Card](https://goreportcard.com/badge/github.com/kfet/harb)](https://goreportcard.com/report/github.com/kfet/harb)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A small, single-binary self-hosted RSS server with a Google-Reader-
compatible API. Plain-text storage on disk, no SQL, stdlib-mostly Go.

> The project is **Harbour RSS** and lives at `github.com/kfet/harb`
> (Go module `github.com/kfet/harb`). The binary, CLI, and config keys
> all use the short name `harb`.

## What it does

- Polls RSS / Atom / JSON feeds (via `gofeed`), conditional GETs with
  `ETag` / `Last-Modified`, exponential backoff on errors, `Retry-After`
  honoured.
- Stores subscriptions in OPML, entries as NDJSON in per-feed
  directories with quarterly archives, read / starred state as append-
  only logs that compact themselves.
- Speaks the **Google Reader API** subset that
  FreshRSS-compatible clients (Reeder Classic, NetNewsWire, Fiery Feeds,
  ReadKit, Unread, lire, Newsify) talk: `ClientLogin`, token, user-info,
  subscription list / edit / quickadd, tag list, rename-tag, disable-tag,
  stream contents, item id queries, edit-tag, mark-all-as-read,
  unread-count.
- Organises feeds with **tags** (many-to-many, flat). OPML 2.0
  `category` attributes round-trip; pre-existing folder-style OPML
  imports as one tag per folder name.
- Serves an embedded htmx web UI on the same port — login, home, per-
  feed list, single-entry view, read / star toggles via hx-post. The
  home page flags feeds that are failing to sync, with the last error
  and last-successful-sync time.
- Themeable via CSS-variable presets (`light`, `dark`, `sepia`) and
  user overrides at `<data-dir>/overrides/templates/*.html` and
  `<data-dir>/overrides/theme.css`.
- Single static binary; subcommands `serve`, `import`, `poll-once`,
  `migrate`, `hashpass`, `version`.

## Install

**macOS (and Linux with Homebrew):**

```sh
brew tap kfet/tap
brew install kfet/tap/harb
```

Updates come via `brew upgrade`.

**Raspberry Pi & other Linux:**

```sh
curl -fsSL https://raw.githubusercontent.com/kfet/harb/main/install.sh | sh
```

Drops the binary in `/usr/local/bin` (or `~/.local/bin` if that isn't
writable). Supports `linux/amd64`, `linux/arm64`, `linux/armv6`
(Raspberry Pi 1 / Zero), `darwin/amd64`, `darwin/arm64`. Override the
target version with `VERSION=v0.1.0` or the install prefix with
`PREFIX=$HOME/.local`.

Once installed, `harb update` will pull the latest release in
place — except when the binary is owned by a package manager (Homebrew,
apt), in which case it'll tell you to use that instead. `harb
update -check` reports without installing.

**From source:**

```sh
go install github.com/kfet/harb/cmd/harb@latest
```

## Quick start

```sh
# build
go build -o harb ./cmd/harb

# one-shot bootstrap: creates data dir, writes config.json, prints a
# generated password. Pass -password to set your own, -username to
# change the login name (default "admin").
./harb init

# import your existing subscriptions (optional)
./harb import subscriptions.opml

# one-shot poll (handy for cron)
./harb poll-once

# serve (HTTP API + UI on :8088)
./harb serve
```

### Upgrading entry identity (v0.19.0)

v0.19.0 changes how entries are de-duplicated (a persistent per-feed
"sticky reuse set"; see `docs/feed-identity.md`). A one-time migration
recomputes stored hashes and collapses old duplicates. **Preview it on a
copy of your data dir first:**

```bash
./harb migrate --identity --dry-run           # report only, writes nothing
./harb migrate --identity --dry-run --map-out /tmp/idmap.json
./harb migrate --identity                      # apply (idempotent, runs once)
```

The dry-run prints every collapse group and sticky-set marking plus an
old→new hash map. The real run is version-gated (a second run is a
no-op) and remaps read/starred state in place.

Then point a FreshRSS-compatible client at `http://your-host:8088/` —
log in with the username (default `admin`) and the password printed by
`init`.

The web UI lives at `/ui/`; visiting `/` redirects there. The build
version is shown in the UI footer and exposed on the API at
`GET /status` (unauthenticated JSON: `{"product","version","commit","buildDate"}`)
and as `harbVersion` on `/reader/api/0/user-info`.

If you'd rather hand-roll the config, `harb hashpass <password>`
prints a hash you can drop into `<data-dir>/config.json` by hand.

### Passkeys (WebAuthn)

Optionally sign in to the web UI with Touch ID, Windows Hello, a phone,
or a security key instead of the password. The password still works (and
remains the RSS-client / Reader-API path). Enable it by adding a
`webauthn` block to `config.json`:

```json
"webauthn": {
  "rp_id": "rss.example.com",
  "origin": "https://rss.example.com",
  "rp_name": "Harbour RSS"
}
```

`rp_id` is the site's hostname; `origin` is its exact https origin.
Both are required — passkeys stay off until they're set. WebAuthn only
works in a **secure context** (https or `localhost`); a plain-http LAN
deployment can't use passkeys. Once serving, open **settings → passkeys
→ add a passkey** to register, then use **sign in with a passkey** on
the login page. Credentials are stored in `credentials.json` and you
can register several (e.g. laptop + phone).

### Link host rewriting

harb can send outbound links through a front-end mirror by remapping
their **host**. Off by default; opt in with a top-level `link_rewrite`
map in `config.json`:

```json
"link_rewrite": {"x.com": "xcancel.com", "twitter.com": "xcancel.com"}
```

Keys and values are bare hosts (no scheme, path or port); entries that
aren't are ignored rather than failing the config load. Matching is
case-insensitive and covers `www.` and subdomains (so
`mobile.twitter.com` follows `twitter.com`). Only the host is replaced —
scheme, port, path, query and fragment survive.

It applies to the links you follow while reading, on **both** front
doors:

- the **web UI** — entry-body anchors and the entry's own source link;
- the **Google Reader API** — anchors inside the article body served by
  `stream/contents` and `stream/items/contents`, so native clients
  (Reeder, NetNewsWire, …) get the mirrored links too.

It never applies to `img` sources or other media, and never to a Reader
item's `id` or `alternate[].href` — clients treat those as identity for
dedupe and read state, so rewriting them would resurface your whole
backlog as unread. The map is applied exactly once per URL, so a
mutually recursive map can't loop. Unsafe links dropped by the web UI
sanitizer stay dropped.

Rewriting happens when content is **served**, never on disk: removing
the map restores the publisher's original links immediately. Items your
client has already cached keep their old links until it refetches them.

> The v0.20.4 spelling `ui.link_rewrite` is **deprecated** but still
> honoured: it is used when the top-level `link_rewrite` is absent.
> Note that the UI-scoped map now also drives the Reader API — move it
> to the top level when convenient.

## Storage layout

```
<data-dir>/
  config.json
  subscriptions.opml
  tokens.json
  credentials.json       # registered passkeys (when webauthn is enabled)
  read.log
  starred.log
  state/<feed-hash>.json
  entries/<feed-hash>/
    current.ndjson
    2024-Q3.ndjson
    2024-Q4.ndjson
  overrides/
    templates/*.html     # user template overrides
    theme.css            # user theme overrides
```

## Design

See [`AGENTS.md`](./AGENTS.md) for the full design brief and
constraints. Highlights:

- Stdlib-mostly. The only third-party dependency is
  `github.com/mmcdole/gofeed` for feed parsing.
- `make all` runs gofmt + vet + staticcheck + race tests with a 100%
  coverage gate (excluding entry-point and e2e via `.covignore`).
- `make e2e` builds the binary and exercises the full surface end-to-
  end against a canned RSS feed.

## Status

v0.1 — minimum viable single-user server. Roadmap: full-text search
(SQLite FTS5 or bleve), more aggressive feed-shape coverage in the
poller, optional multi-user.

## License

MIT — see [LICENSE](./LICENSE).
