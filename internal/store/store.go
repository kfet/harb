package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Entry is one feed item, stored as one JSON object per line in NDJSON.
type Entry struct {
	Hash      string    `json:"hash"`
	FeedHash  string    `json:"feed"`
	GUID      string    `json:"guid,omitempty"`
	Link      string    `json:"link,omitempty"`
	Title     string    `json:"title,omitempty"`
	Author    string    `json:"author,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Content   string    `json:"content,omitempty"`
	Published time.Time `json:"published"`
	FetchedAt time.Time `json:"fetched_at"`
}

// FeedState is the per-feed conditional-GET state. v0.4.18 dropped the
// adaptive scheduler: there are no NextFetch / Interval fields any more.
// Refresh cadence is driven by the pull-side Refresher (one cycle =
// every feed, sequentially), not by per-feed timers.
//
// RetryAfter is set when a feed responds with 429 / 503 + Retry-After;
// the Refresher / Poll skip the feed until time.Now() >= RetryAfter.
// Zero RetryAfter means no cooldown.
//
// LastFetched records the time of the most recent poll *attempt*,
// success or failure. LastSuccess records the most recent *successful*
// sync (a 2xx with entries applied, or a 304 not-modified — both mean
// the feed is reachable and healthy). The two diverge once a feed
// starts failing: LastFetched keeps advancing each cycle while
// LastSuccess stays pinned to the last good poll, which is what the web
// UI surfaces as "last succeeded". Zero LastSuccess means the feed has
// never synced successfully (e.g. a legacy state file written before
// the field existed, until its next good poll).
//
// Legacy on-disk state files may still carry next_fetch / interval_s
// fields written by v0.4.17 and earlier. Those tags are not declared
// here, so encoding/json silently ignores them on Load; the next
// SaveFeedState rewrites the file without them.
type FeedState struct {
	URL          string    `json:"url"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	LastFetched  time.Time `json:"last_fetched,omitempty"`
	LastSuccess  time.Time `json:"last_success,omitempty"`
	RetryAfter   time.Time `json:"retry_after,omitempty"`
	ErrorCount   int       `json:"error_count,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

// EntryState is the read/starred state for a single entry, derived by
// folding the append-only logs on startup.
type EntryState struct {
	Read    bool
	Starred bool
	// UpdatedAt is the time of the most recent fold-relevant event for
	// this entry.
	UpdatedAt time.Time
}

// Store is the in-memory + on-disk single-user store.
type Store struct {
	Dir string

	mu    sync.RWMutex
	state map[string]EntryState // entryHash -> state
	readN int                   // count of "r" lines currently on disk
	starN int                   // count of "s/S" lines currently on disk
	liveR int                   // live read entries (read=true)
	liveS int                   // live starred entries (starred=true)
	now   func() time.Time

	// In-memory entry index. Built at Open and maintained on
	// AppendEntries. Lets Reader handlers skip ReadDir + ndjson reparse
	// per request (the ~200ms-per-call hot path that made Reeder sync
	// slow).
	//
	// idx[feedHash]    → entries sorted by Published descending.
	// byHash[entryHash] → the same Entry (by-value copy).
	//
	// Guarded by mu (shared with state). Index callers take RLock for
	// reads and return defensive copies, so callers that mutate the
	// returned slice (e.g. filtering in place) cannot corrupt the cache.
	//
	// Consistency: AppendEntries writes to disk before taking mu to
	// update the index, so freshly-appended entries become visible on
	// disk a few µs before the index reflects them. Reader handlers
	// never read disk, so from their point of view the new batch
	// arrives atomically when the index lock is released.
	idx    map[string][]Entry
	byHash map[string]Entry

	// contentVer is the validator for state-dependent reader
	// endpoints (`unread-count`). It bumps on any mutation that
	// changes a content-shaped response:
	//   - SetRead / SetStarred (state flag flips)
	//   - AppendEntries (new entries observable in unread counts)
	// Persists across restarts because it is rebuilt from on-disk
	// state-log UpdatedAt timestamps in Open; new-entry-only
	// bumps after that point are in-process only, which is fine —
	// after a restart the validator necessarily differs (it starts
	// from a different `now()`), so clients pick up changes via
	// the next 200 response.
	contentVer time.Time
}

// Open opens (and lazily creates) a data dir, folding state logs.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := migrateEntryHashes(dir); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, state: map[string]EntryState{}, idx: map[string][]Entry{}, byHash: map[string]Entry{}, now: time.Now}
	if err := s.foldLog(filepath.Join(dir, "read.log"), 'r'); err != nil {
		return nil, err
	}
	if err := s.foldLog(filepath.Join(dir, "starred.log"), 's'); err != nil {
		return nil, err
	}
	s.recountLive()
	if err := s.buildIndex(); err != nil {
		return nil, err
	}
	return s, nil
}

// buildIndex scans every entries/<feedHash>/*.ndjson on disk and
// populates the in-memory index. Called once at Open after migration.
func (s *Store) buildIndex() error {
	root := filepath.Join(s.Dir, "entries")
	feeds, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, fd := range feeds {
		if !fd.IsDir() {
			continue
		}
		fh := fd.Name()
		dir := filepath.Join(root, fh)
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		var list []Entry
		seen := make(map[string]bool)
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".ndjson") {
				continue
			}
			if err := scanEntries(filepath.Join(dir, e.Name()), func(en Entry) error {
				en.Hash = StoreEntryHash(en.Hash)
				if en.FeedHash == "" {
					en.FeedHash = fh
				}
				// Dedup by canonical hash: legacy unmasked hashes and
				// their masked re-poll collapse to the same id, so the
				// same article can appear on disk more than once. Keep
				// the first occurrence; later ones only refresh byHash.
				if !seen[en.Hash] {
					seen[en.Hash] = true
					list = append(list, en)
				}
				s.byHash[en.Hash] = en
				if en.FetchedAt.After(s.contentVer) {
					// Fold entry-arrival time into the validator so it
					// survives restarts. AppendEntries bumps contentVer
					// in-process when new entries land, but Open only
					// rebuilds it from the state logs — so without this
					// the validator regresses below the last append on
					// restart. A client whose cached unread-count ETag
					// predates the new entries would then be wrongly
					// served 304 (it never re-fetches; the UI shows the
					// items but the client never sees them). Seeding from
					// the newest FetchedAt keeps the post-restart
					// validator >= the pre-restart new-entry bump.
					s.contentVer = en.FetchedAt
				}
				return nil
			}); err != nil {
				return err
			}
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].Published.After(list[j].Published)
		})
		s.idx[fh] = list
	}
	return nil
}

// IndexedEntries returns a snapshot of the in-memory entries for a feed,
// already sorted by Published descending. The returned slice is a copy;
// callers may filter/mutate it freely without affecting the cache.
func (s *Store) IndexedEntries(feedHash string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.idx[feedHash]
	out := make([]Entry, len(src))
	copy(out, src)
	return out
}

// EntryByHash looks up an indexed entry by hash. Second return is false
// if the hash is unknown.
func (s *Store) EntryByHash(hash string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byHash[StoreEntryHash(hash)]
	return e, ok
}

func (s *Store) recountLive() {
	s.liveR, s.liveS = 0, 0
	for _, st := range s.state {
		if st.Read {
			s.liveR++
		}
		if st.Starred {
			s.liveS++
		}
	}
}

// foldLog reads a state log and applies it. Lines: "<rfc3339> <op> <hash>".
// kind 'r' uses ops {r,u} (read/unread); kind 's' uses ops {s,S} (star/unstar).
func (s *Store) foldLog(path string, kind byte) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, parts[0])
		if err != nil {
			continue
		}
		op, hash := parts[1], parts[2]
		hash = StoreEntryHash(hash)
		st := s.state[hash]
		st.UpdatedAt = ts
		switch {
		case kind == 'r' && op == "r":
			st.Read = true
			s.readN++
		case kind == 'r' && op == "u":
			st.Read = false
			s.readN++
		case kind == 's' && op == "s":
			st.Starred = true
			s.starN++
		case kind == 's' && op == "S":
			st.Starred = false
			s.starN++
		default:
			continue
		}
		s.state[hash] = st
		if ts.After(s.contentVer) {
			s.contentVer = ts
		}
	}
	return sc.Err()
}

// EntryState returns the current state for an entry hash.
func (s *Store) EntryState(hash string) EntryState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state[StoreEntryHash(hash)]
}

// StateVersion returns the most-recent content-mutation timestamp —
// the ETag-validator for state-dependent reader endpoints. Bumped on
// SetRead, SetStarred, and AppendEntries (any change that affects an
// unread-count response). Zero iff none of those have ever run on
// this data dir.
//
// Restart semantics: rebuilt in Open from the state-log UpdatedAt
// timestamps AND the newest entry FetchedAt across all feeds (see
// buildIndex). Folding in FetchedAt keeps the validator >= the last
// in-process AppendEntries bump, so it does not regress below entries
// that are still on disk after a restart. Without it a client whose
// cached ETag predated the new entries would be wrongly served 304.
func (s *Store) StateVersion() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contentVer
}

// SetRead records a read/unread mutation. Idempotent.
func (s *Store) SetRead(hash string, read bool) error {
	return s.setFlag(hash, read, true)
}

// SetStarred records a starred/unstarred mutation. Idempotent.
func (s *Store) SetStarred(hash string, starred bool) error {
	return s.setFlag(hash, starred, false)
}

func (s *Store) setFlag(hash string, want, isRead bool) error {
	hash = StoreEntryHash(hash)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state[hash]
	now := s.now().UTC()
	var op byte
	var path string
	// Compute the mutation but DO NOT apply it to in-memory state until
	// the persist succeeds. Otherwise a persist failure leaves the
	// reader showing a flag set that survives only until restart.
	newLiveR, newLiveS := s.liveR, s.liveS
	if isRead {
		if st.Read == want {
			return nil
		}
		if want {
			op = 'r'
			newLiveR++
		} else {
			op = 'u'
			newLiveR--
		}
		path = filepath.Join(s.Dir, "read.log")
	} else {
		if st.Starred == want {
			return nil
		}
		if want {
			op = 's'
			newLiveS++
		} else {
			op = 'S'
			newLiveS--
		}
		path = filepath.Join(s.Dir, "starred.log")
	}
	line := fmt.Sprintf("%s %c %s\n", now.Format(time.RFC3339), op, hash)
	if err := appendLine(path, line); err != nil {
		return err
	}
	// Persist succeeded — now apply the in-memory mutation.
	if isRead {
		st.Read = want
		s.liveR = newLiveR
		s.readN++
	} else {
		st.Starred = want
		s.liveS = newLiveS
		s.starN++
	}
	st.UpdatedAt = now
	s.state[hash] = st
	s.contentVer = now
	// Compact when log is 10× live set (and at least 32 entries to avoid churn).
	if isRead && s.readN > 32 && s.readN > 10*s.liveR {
		return s.compactLocked(path, 'r')
	}
	if !isRead && s.starN > 32 && s.starN > 10*s.liveS {
		return s.compactLocked(path, 's')
	}
	return nil
}

// compactLocked rewrites a state log from the current in-memory state.
// Caller must hold s.mu.
func (s *Store) compactLocked(path string, kind byte) error {
	hashes := make([]string, 0, len(s.state))
	for h, st := range s.state {
		if kind == 'r' && st.Read {
			hashes = append(hashes, h)
		} else if kind == 's' && st.Starred {
			hashes = append(hashes, h)
		}
	}
	sort.Strings(hashes)
	var b strings.Builder
	op := byte('r')
	if kind == 's' {
		op = 's'
	}
	now := s.now().UTC().Format(time.RFC3339)
	for _, h := range hashes {
		fmt.Fprintf(&b, "%s %c %s\n", now, op, h)
	}
	if err := atomicWriteFile(path, []byte(b.String())); err != nil {
		return err
	}
	if kind == 'r' {
		s.readN = len(hashes)
	} else {
		s.starN = len(hashes)
	}
	return nil
}

// AssignEntryHashesForFeed sets .Hash on every entry in the incoming batch,
// applying the D4 guid-reuse guard driven by the feed's PERSISTENT sticky
// reuse set. The sticky set is the single source of D4 and is maintained here:
//
//  1. Detect guids that co-occur (appear >=2 times) WITHIN this batch
//     (reusedInBatch) — publisher-proof of reuse, computed with no title, no
//     link, no on-disk and no cross-time input.
//  2. Merge any newly-detected guids into the feed's persisted sticky set and
//     rewrite the sidecar if it grew. Once a guid is marked reused for a feed
//     it stays marked forever (monotone), so an item's basis can never flip
//     D4->D1 when a same-guid sibling scrolls out of the feed window.
//  3. Assign each entry's hash: D4 (link mixed in) iff its normalised guid is
//     in the sticky set, else D1 (guid-only). Fallbacks (link, title+published)
//     are unchanged when the guid is absent.
//
// The assigned hash is a PURE function of (entry fields, sticky set): batch
// composition and ordering affect only which guids get ADDED to the sticky set,
// never the hash computed from a given (fields, set) pair. That is the
// invariant that ends the re-dup bug class.
func (s *Store) AssignEntryHashesForFeed(feedHash string, entries []Entry) error {
	sticky, err := s.loadStickyGUIDs(feedHash)
	if err != nil {
		return err
	}
	grew := false
	for g := range reusedInBatch(entries) {
		if !sticky[g] {
			sticky[g] = true
			grew = true
		}
	}
	if grew {
		if err := s.saveStickyGUIDs(feedHash, sticky); err != nil {
			return err
		}
	}
	assignWithReused(entries, sticky)
	return nil
}

// stickyGUIDsPath is the per-feed sidecar holding the sticky reuse set: a JSON
// array of normalised guids that route to D4 forever.
func (s *Store) stickyGUIDsPath(feedHash string) string {
	return filepath.Join(s.Dir, "entries", feedHash, "reused-guids.json")
}

// loadStickyGUIDs reads a feed's persistent sticky reuse set. Missing → empty.
func (s *Store) loadStickyGUIDs(feedHash string) (map[string]bool, error) {
	data, err := os.ReadFile(s.stickyGUIDsPath(feedHash))
	if err != nil {
		// Missing sidecar → no sticky set. ENOTDIR (a path component is not a
		// directory, e.g. the feed's entries dir does not exist yet) is treated
		// the same: there is simply no persisted set to load, and any real
		// structural problem surfaces later when AppendEntries writes.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(list))
	for _, g := range list {
		set[g] = true
	}
	return set, nil
}

// saveStickyGUIDs atomically rewrites a feed's sticky reuse set. The guids are
// sorted so the file is stable across writes (deterministic, review-friendly).
func (s *Store) saveStickyGUIDs(feedHash string, set map[string]bool) error {
	list := make([]string, 0, len(set))
	for g := range set {
		list = append(list, g)
	}
	sort.Strings(list)
	data, err := jsonMarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(s.Dir, "entries", feedHash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return atomicWriteFile(s.stickyGUIDsPath(feedHash), data)
}

// AppendEntries appends entries to feed's current.ndjson. Returns the
// subset that was actually new (de-duplicated by entry hash within the
// feed against the in-memory index — no per-poll disk scan).
//
// Dedup source: a snapshot of s.idx[feedHash]'s hashes taken under s.mu.
// s.idx is a complete superset of the on-disk known-hash set: buildIndex
// seeds it from the same entries/<feedHash>/*.ndjson glob knownHashes
// scans, AppendEntries only ever appends to it, and RolloverArchives
// merely relocates entries between on-disk files without pruning idx. So
// deduping against idx can never reintroduce a duplicate that a disk scan
// would have caught. The snapshot is taken under the read lock and the
// lock is released before any file I/O, since AppendEntries is called
// concurrently for different feeds (v0.8.0 bounded fan-out) and s.idx is
// a shared map.
func (s *Store) AppendEntries(feedHash string, entries []Entry) ([]Entry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	known := s.indexedHashes(feedHash)
	dir := filepath.Join(s.Dir, "entries", feedHash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "current.ndjson")
	f, err := osOpenAppend(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var added []Entry
	// writeErr captures a mid-stream marshal/write failure. We must NOT
	// early-return on it: any entries already written to current.ndjson are
	// in `added`, and since dedup now reads only the in-memory index, they
	// MUST be committed to s.idx — otherwise the next poll (no restart) would
	// re-append them and duplicate on disk. So we break out and fall through
	// to the idx-commit block, then return the partial result with the error.
	var writeErr error
	for _, e := range entries {
		if e.Hash == "" {
			e.Hash = EntryHash(e.GUID, e.Link, e.Title, e.Published)
		} else {
			e.Hash = StoreEntryHash(e.Hash)
		}
		if e.FeedHash == "" {
			e.FeedHash = feedHash
		}
		if known[e.Hash] {
			continue
		}
		known[e.Hash] = true
		line, err := jsonMarshal(e)
		if err != nil {
			writeErr = err
			break
		}
		line = append(line, '\n')
		if _, err := f.Write(line); err != nil {
			writeErr = err
			break
		}
		added = append(added, e)
	}
	if len(added) > 0 {
		s.mu.Lock()
		list := s.idx[feedHash]
		for _, e := range added {
			list = append(list, e)
			s.byHash[e.Hash] = e
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].Published.After(list[j].Published)
		})
		s.idx[feedHash] = list
		// Bump the content version — new entries change the
		// unread-count payload and must invalidate cached ETags.
		// This in-process bump is also made durable across restarts:
		// Open seeds contentVer from the newest entry FetchedAt (see
		// buildIndex), so the validator does not regress below entries
		// still on disk.
		if now := s.now().UTC(); now.After(s.contentVer) {
			s.contentVer = now
		}
		s.mu.Unlock()
	}
	return added, writeErr
}

// indexedHashes returns the set of entry hashes currently in the
// in-memory index for a feed. It takes a snapshot under the read lock so
// the caller can release the lock before doing file I/O. s.idx is a
// complete superset of the on-disk known-hash set (see AppendEntries),
// so this is a safe per-feed dedup source. Per-feed isolation is
// preserved: only s.idx[feedHash] is consulted, never the cross-feed
// s.byHash map.
func (s *Store) indexedHashes(feedHash string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.idx[feedHash]
	set := make(map[string]bool, len(list))
	for _, e := range list {
		set[e.Hash] = true
	}
	return set
}

// KnownHashes returns the set of entry hashes currently persisted for a
// feed (current + all archives). It is exported for the poll-stage Webflow
// content enrichment, which must distinguish brand-new entries from ones
// already stored *before* AppendEntries runs, so that later polls only
// fetch detail pages for newly-published posts.
func (s *Store) KnownHashes(feedHash string) (map[string]bool, error) {
	return s.knownHashes(feedHash)
}

// knownHashes returns the set of entry hashes currently known for a feed
// across current + all archives.
func (s *Store) knownHashes(feedHash string) (map[string]bool, error) {
	set := map[string]bool{}
	dir := filepath.Join(s.Dir, "entries", feedHash)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return set, nil
		}
		return nil, err
	}
	for _, ent := range ents {
		if !strings.HasSuffix(ent.Name(), ".ndjson") {
			continue
		}
		if err := scanEntries(filepath.Join(dir, ent.Name()), func(e Entry) error {
			set[StoreEntryHash(e.Hash)] = true
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// scanEntries calls fn for each JSON object in the file.
func scanEntries(path string, fn func(Entry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return sc.Err()
}

// ListEntries returns all entries for a feed (current + all archives),
// most-recently-published first.
func (s *Store) ListEntries(feedHash string) ([]Entry, error) {
	dir := filepath.Join(s.Dir, "entries", feedHash)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	seen := make(map[string]bool)
	for _, ent := range ents {
		if !strings.HasSuffix(ent.Name(), ".ndjson") {
			continue
		}
		if err := scanEntries(filepath.Join(dir, ent.Name()), func(e Entry) error {
			e.Hash = StoreEntryHash(e.Hash)
			if seen[e.Hash] {
				return nil
			}
			seen[e.Hash] = true
			out = append(out, e)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Published.After(out[j].Published)
	})
	return out, nil
}

// RolloverArchives splits current.ndjson into quarterly archives for entries
// older than the cutoff. The hot file keeps everything newer-than cutoff.
// Returns the number of entries archived.
func (s *Store) RolloverArchives(feedHash string, cutoff time.Time) (int, error) {
	dir := filepath.Join(s.Dir, "entries", feedHash)
	curPath := filepath.Join(dir, "current.ndjson")
	if _, err := os.Stat(curPath); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	var keep []Entry
	byQuarter := map[string][]Entry{}
	if err := scanEntries(curPath, func(e Entry) error {
		t := e.Published
		if t.IsZero() {
			t = e.FetchedAt
		}
		if t.Before(cutoff) {
			q := fmt.Sprintf("%04d-Q%d", t.Year(), (int(t.Month())-1)/3+1)
			byQuarter[q] = append(byQuarter[q], e)
		} else {
			keep = append(keep, e)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	if len(byQuarter) == 0 {
		return 0, nil
	}
	archived := 0
	for q, items := range byQuarter {
		path := filepath.Join(dir, q+".ndjson")
		f, err := osOpenAppend(path)
		if err != nil {
			return archived, err
		}
		for _, e := range items {
			line, err := jsonMarshal(e)
			if err != nil {
				f.Close()
				return archived, err
			}
			line = append(line, '\n')
			if _, err := f.Write(line); err != nil {
				f.Close()
				return archived, err
			}
			archived++
		}
		if err := f.Close(); err != nil {
			return archived, err
		}
	}
	// Rewrite current atomically.
	var b strings.Builder
	for _, e := range keep {
		line, err := jsonMarshal(e)
		if err != nil {
			return archived, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := atomicWriteFile(curPath, []byte(b.String())); err != nil {
		return archived, err
	}
	return archived, nil
}

// FeedState helpers --------------------------------------------------------

// LoadFeedState reads a per-feed state file. Missing → zero value, no error.
func (s *Store) LoadFeedState(feedHash string) (FeedState, error) {
	path := filepath.Join(s.Dir, "state", feedHash+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FeedState{}, nil
		}
		return FeedState{}, err
	}
	var fs FeedState
	if err := json.Unmarshal(data, &fs); err != nil {
		return FeedState{}, err
	}
	return fs, nil
}

// SaveFeedState atomically writes the per-feed state.
func (s *Store) SaveFeedState(feedHash string, fs FeedState) error {
	data, err := jsonMarshalIndent(fs, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(s.Dir, "state", feedHash+".json"), data)
}

// AllStates returns a snapshot of in-memory entry states. Mainly for tests.
func (s *Store) AllStates() map[string]EntryState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]EntryState, len(s.state))
	for k, v := range s.state {
		out[k] = v
	}
	return out
}

// CountRead returns the number of live read entries.
func (s *Store) CountRead() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.liveR
}

// CountStarred returns the number of live starred entries.
func (s *Store) CountStarred() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.liveS
}

// appendLine appends a single line to path via O_APPEND. Assumes < PIPE_BUF.
func appendLine(path string, line string) error {
	if len(line) > 4000 {
		return fmt.Errorf("appendLine: line too large: %d", len(line))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := osOpenAppend(path)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
