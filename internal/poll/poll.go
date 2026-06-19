// Package poll fetches RSS/Atom feeds with conditional GETs.
//
// Cadence is driven by the pull-side Refresher (see refresher.go), not by
// per-feed timers — v0.4.18 removed the adaptive scheduler that pinned
// well-behaved feeds to once-per-24h after a string of 304s. Poll itself
// is a single-shot operation: one HTTP request, one persist of the
// updated FeedState. The only per-feed throttle that survives in Poll
// is RetryAfter, set when a feed returned 429 / 503 with a Retry-After
// header; subsequent Poll calls within the window return early without
// touching the network.
package poll

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kfet/harb/internal/poll/observe"
	"github.com/kfet/harb/internal/poll/resolve"
	"github.com/kfet/harb/internal/safedial"
	"github.com/kfet/harb/internal/store"
	"github.com/mmcdole/gofeed"
)

// DefaultUserAgent is the fallback HTTP User-Agent for feed fetches when
// a Poller's UserAgent field is unset. It deliberately carries NO
// disclosure URL: some CDNs' bot rules (observed with Akamai-fronted
// feeds such as CBC) tarpit any User-Agent containing a
// "(+https://…github.com…)" string, stalling the response until the
// client times out — even though the same request with a bare product
// token succeeds. main wires the running build's version in via
// Poller.UserAgent ("harb/<version>").
const DefaultUserAgent = "harb"

// Poller fetches feeds and writes results to a Store.
//
// Concurrency: a single Poller is safe to use from many goroutines at
// once (the Refresher fans cycle() out across a bounded worker pool).
// Everything Poll touches is either goroutine-safe (the *store.Store
// guards its in-memory indexes with a RWMutex and writes per-feed
// files; the resolve registry is read-only after init; DiskObserver
// locks; http.Client is safe) or constructed fresh per call. In
// particular Poll allocates a fresh gofeed.Parser per invocation
// because gofeed.Parser is NOT goroutine-safe and carries no state
// worth sharing.
type Poller struct {
	Store  *store.Store
	Client *http.Client
	Now    func() time.Time
	// MaxBodyBytes caps how much body we read per feed (default 10MiB).
	MaxBodyBytes int64
	// UserAgent is sent on every feed fetch. Defaults to
	// DefaultUserAgent; main overrides it with "harb/<version>".
	UserAgent string
	// Observer records the outcome of every poll for an out-of-process
	// fixer to consume. Defaults to observe.Nop{} (no-op) — set to an
	// observe.DiskObserver to persist outcomes under <data-dir>/observe.
	Observer observe.Observer
	// Resolve loads the per-feed resolver chain (builtins + sidecar).
	// Defaults to resolve.Load; overridable in tests to inject a chain.
	Resolve func(dir, feedHash string) (resolve.Chain, error)
}

// New builds a Poller with sensible defaults. Store is required.
func New(s *store.Store) *Poller {
	return &Poller{
		Store:        s,
		Client:       safedial.NewClient(30 * time.Second),
		Now:          time.Now,
		MaxBodyBytes: 10 * 1024 * 1024,
		UserAgent:    DefaultUserAgent,
		Observer:     observe.Nop{},
		Resolve:      resolve.Load,
	}
}

// ErrCooldown is returned when Poll is called for a feed whose
// RetryAfter window has not yet elapsed. No HTTP request is made and
// no state is mutated.
var ErrCooldown = errors.New("poll: feed in 429/503 cooldown window")

// Poll fetches one feed and returns the number of new entries appended.
// State (ETag, Last-Modified, LastFetched, ErrorCount, RetryAfter) is
// persisted regardless of outcome — unless the feed is in a 429/503
// cooldown, in which case Poll returns (0, ErrCooldown) immediately.
func (p *Poller) Poll(ctx context.Context, feedURL string) (int, error) {
	fh := store.FeedHash(feedURL)
	st, err := p.Store.LoadFeedState(fh)
	if err != nil {
		return 0, fmt.Errorf("load state: %w", err)
	}
	if st.URL == "" {
		st.URL = feedURL
	}
	now := p.Now().UTC()
	if !st.RetryAfter.IsZero() && now.Before(st.RetryAfter) {
		p.obs().Observe(fh, observe.Observation{Outcome: observe.Cooldown})
		return 0, ErrCooldown
	}
	st.LastFetched = now

	// Build the per-feed resolver chain (builtins + sidecar). A sidecar
	// error is non-fatal: the returned chain is always usable, so we
	// proceed with whatever built and let the breakage surface in
	// observations rather than failing the poll.
	loadChain := p.Resolve
	if loadChain == nil {
		loadChain = resolve.Load
	}
	chain, _ := loadChain(p.Store.Dir, fh)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return 0, p.recordErr(fh, &st, err)
	}
	ua := p.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.8")
	if st.ETag != "" {
		req.Header.Set("If-None-Match", st.ETag)
	}
	if st.LastModified != "" {
		req.Header.Set("If-Modified-Since", st.LastModified)
	}
	// Request-shaping resolvers run after defaults so a sidecar can
	// override the User-Agent or add headers a feed's CDN demands.
	if err := chain.ShapeRequest(req, resolve.FeedMeta{URL: feedURL}); err != nil {
		p.obs().Observe(fh, observe.Observation{Outcome: observe.FetchError, Err: err.Error(), Resolvers: chain.Names()})
		return 0, p.recordErr(fh, &st, err)
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		p.obs().Observe(fh, observe.Observation{Outcome: observe.FetchError, Err: err.Error(), Resolvers: chain.Names()})
		return 0, p.recordErr(fh, &st, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// 304 — reachable + unchanged is a healthy sync; record it as a
		// success and clear error state. No scheduling side-effect.
		st.ErrorCount = 0
		st.LastError = ""
		st.LastSuccess = now
		st.RetryAfter = time.Time{}
		p.obs().Observe(fh, observe.Observation{Outcome: observe.NotModified, Status: resp.StatusCode})
		return 0, p.Store.SaveFeedState(fh, st)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// fall through
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		// Honour Retry-After when present; otherwise apply a short
		// default cooldown so a misbehaving server isn't hammered
		// every Refresher cycle.
		d := parseRetryAfter(resp.Header.Get("Retry-After"), now)
		if d <= 0 {
			d = defaultCooldown
		}
		st.ErrorCount++
		err := fmt.Errorf("http %d", resp.StatusCode)
		st.LastError = err.Error()
		st.RetryAfter = now.Add(d)
		p.obs().Observe(fh, observe.Observation{Outcome: observe.HTTPError, Status: resp.StatusCode, Err: err.Error()})
		if saveErr := p.Store.SaveFeedState(fh, st); saveErr != nil {
			return 0, saveErr
		}
		return 0, err
	default:
		err := fmt.Errorf("http %d", resp.StatusCode)
		p.obs().Observe(fh, observe.Observation{Outcome: observe.HTTPError, Status: resp.StatusCode, Err: err.Error()})
		return 0, p.recordErr(fh, &st, err)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, p.MaxBodyBytes+1))
	if err != nil {
		p.obs().Observe(fh, observe.Observation{Outcome: observe.FetchError, Status: resp.StatusCode, Err: err.Error()})
		return 0, p.recordErr(fh, &st, err)
	}
	if int64(len(body)) > p.MaxBodyBytes {
		err := errors.New("body too large")
		p.obs().Observe(fh, observe.Observation{Outcome: observe.TooLarge, Status: resp.StatusCode, Bytes: len(body)})
		return 0, p.recordErr(fh, &st, err)
	}

	ct := resp.Header.Get("Content-Type")
	meta := resolve.FeedMeta{URL: feedURL, ContentType: ct, Status: resp.StatusCode}
	transformed, terr := chain.Transform(body, meta)
	if terr != nil {
		p.obs().Sample(fh, body)
		p.obs().Observe(fh, observe.Observation{Outcome: observe.ParseError, Status: resp.StatusCode, ContentType: ct, Bytes: len(body), Err: terr.Error(), Resolvers: chain.Names(), Sample: true})
		return 0, p.recordErr(fh, &st, terr)
	}

	// gofeed.Parser is not goroutine-safe, so allocate a fresh one
	// per poll: cheap, and lets N workers parse concurrently without
	// a shared-parser data race.
	parsed, err := gofeed.NewParser().ParseString(string(transformed))
	if err != nil {
		// Save the raw (pre-transform) body: it is what the fixer needs to
		// diagnose, and what a new resolver Spec must learn to repair.
		p.obs().Sample(fh, body)
		p.obs().Observe(fh, observe.Observation{Outcome: observe.ParseError, Status: resp.StatusCode, ContentType: ct, Bytes: len(body), Err: err.Error(), Resolvers: chain.Names(), Sample: true})
		return 0, p.recordErr(fh, &st, err)
	}

	entries := make([]store.Entry, 0, len(parsed.Items))
	for _, it := range parsed.Items {
		// Decode HTML entities once at ingestion in *text* fields only
		// (Title, Author.Name). Summary / Content are HTML and stay raw.
		e := store.Entry{
			FeedHash:  fh,
			GUID:      it.GUID,
			Link:      it.Link,
			Title:     html.UnescapeString(it.Title),
			Summary:   it.Description,
			Content:   it.Content,
			FetchedAt: now,
		}
		if it.Author != nil {
			e.Author = html.UnescapeString(it.Author.Name)
		}
		if it.PublishedParsed != nil {
			e.Published = it.PublishedParsed.UTC()
		} else {
			e.Published = now
		}
		entries = append(entries, e)
	}
	// Assign identities for the whole batch so the D4 guid-reuse guard can
	// see repeated guids across the poll's items (see AssignEntryHashes).
	store.AssignEntryHashes(entries)

	// Webflow article-text enrichment: when this feed was synthesised from
	// a Webflow page (the resolver stamps a generator marker), fetch each
	// NEW entry's detail page and fill in the full article body from its
	// largest .w-richtext block. "New-only" keeps later polls cheap — only
	// freshly-published posts are fetched — and the fetch is best-effort,
	// so a slow or broken detail page never fails the poll.
	// Content enrichment (best-effort, new-entries-only): fetch each new
	// entry's page and fill in a real body.
	if known, kerr := p.Store.KnownHashes(fh); kerr == nil {
		isNew := func(h string) bool { return !known[h] }
		// Webflow-synthesised feeds: extract the post's .w-richtext block.
		if parsed.Generator == resolve.WebflowGenerator {
			enrichWebflowContent(ctx, p.Client, ua, entries, isNew)
		}
		// Aggregator / link-only feeds (Lobsters, Hacker News, Reddit):
		// the feed item body is just a "Comments"/"Source" link, so fetch
		// the linked external article and extract its readable content.
		enrichLinkOnlyContent(ctx, p.Client, ua, entries, isNew)
	}

	added, err := p.Store.AppendEntries(fh, entries)
	if err != nil {
		return 0, p.recordErr(fh, &st, err)
	}

	// Success — refresh conditional headers, reset error state.
	st.ETag = resp.Header.Get("ETag")
	st.LastModified = resp.Header.Get("Last-Modified")
	st.ErrorCount = 0
	st.LastError = ""
	st.LastSuccess = now
	st.RetryAfter = time.Time{}
	if err := p.Store.SaveFeedState(fh, st); err != nil {
		return len(added), err
	}
	p.obs().Observe(fh, observe.Observation{
		Outcome:     observe.Success,
		Status:      resp.StatusCode,
		ContentType: ct,
		Bytes:       len(body),
		NewEntries:  len(added),
		Resolvers:   chain.Names(),
	})
	return len(added), nil
}

// obs returns the configured Observer, or a no-op if unset. Keeps every
// call site free of nil checks.
func (p *Poller) obs() observe.Observer {
	if p.Observer == nil {
		return observe.Nop{}
	}
	return p.Observer
}

// ResetCooldown clears any RetryAfter cooldown on a feed and persists.
// Used by `harb poll-once` which must force a poll of every feed
// regardless of cooldown state.
func (p *Poller) ResetCooldown(feedURL string) error {
	fh := store.FeedHash(feedURL)
	st, err := p.Store.LoadFeedState(fh)
	if err != nil {
		return err
	}
	if st.RetryAfter.IsZero() {
		return nil
	}
	st.RetryAfter = time.Time{}
	return p.Store.SaveFeedState(fh, st)
}

func (p *Poller) recordErr(fh string, st *store.FeedState, e error) error {
	st.ErrorCount++
	st.LastError = e.Error()
	if saveErr := p.Store.SaveFeedState(fh, *st); saveErr != nil {
		return saveErr
	}
	return e
}

// defaultCooldown is applied on 429/503 responses that omit Retry-After.
// Short enough that a transient blip doesn't pin a feed for hours, long
// enough that we don't retry on the very next 1-minute Refresher tick.
const defaultCooldown = 15 * time.Minute

// parseRetryAfter parses a Retry-After header (seconds or HTTP-date) and
// returns a duration from "now". Returns 0 if unparseable.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
