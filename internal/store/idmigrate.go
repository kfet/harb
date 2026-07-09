package store

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// identityMigrationVersion tags the one-time sticky-identity migration. The
// marker file records it so a completed real migration is a no-op on re-run
// (version-gated). Bump only if the migration logic itself must run again.
const identityMigrationVersion = "0.19.0"

// identityMarkerName is the per-data-dir marker written after a successful real
// migration.
const identityMarkerName = "identity-migration.json"

// IdentityReport is the human-reviewable result of MigrateIdentity. The dry-run
// path returns it fully populated WITHOUT touching any data, so the supervisor
// can eyeball every collapse and sticky marking (and the old→new hash map)
// before running the real migration.
type IdentityReport struct {
	Version string `json:"version"`
	DryRun  bool   `json:"dry_run"`
	// Skipped is true when a real run finds the version marker already present
	// (idempotent no-op). Dry runs never skip.
	Skipped bool         `json:"skipped"`
	Feeds   []FeedReport `json:"feeds,omitempty"`
	Totals  ReportTotals `json:"totals"`
	// HashRemap is the full canonical old-hash → new-hash artifact. Entries
	// whose identity is unchanged map to themselves (omitted from the report
	// body but present here) — callers filter as needed.
	HashRemap map[string]string `json:"hash_remap,omitempty"`
}

// ReportTotals is the accounting summary. All invariants below are asserted by
// MigrateIdentity before it writes anything; a violation aborts with an error
// and leaves the data dir untouched.
type ReportTotals struct {
	OldHashCount      int `json:"old_hash_count"`
	SurvivorCount     int `json:"survivor_count"`
	IntendedCollapses int `json:"intended_collapses"`
	StickyMarkings    int `json:"sticky_markings"`
	StarBefore        int `json:"star_before"`
	StarAfter         int `json:"star_after"`
	ReadBefore        int `json:"read_before"`
	ReadAfter         int `json:"read_after"`
}

// FeedReport is one feed's slice of the migration.
type FeedReport struct {
	FeedHash     string          `json:"feed_hash"`
	StickyMarked []StickyMark    `json:"sticky_marked,omitempty"`
	Collapses    []CollapseGroup `json:"collapses,omitempty"`
}

// StickyMark records a guid newly added to a feed's sticky reuse set during
// seeding, with the full evidence group so the supervisor can verify it is a
// genuine opaque-token reuse (CBC) and not an incident dup.
type StickyMark struct {
	GUID    string   `json:"guid"`
	Members []Member `json:"members"`
}

// CollapseGroup records a set of on-disk entries that recompute to one identity
// and are merged to a single survivor.
type CollapseGroup struct {
	NewHash  string   `json:"new_hash"`
	Survivor Member   `json:"survivor"`
	Members  []Member `json:"members"`
}

// Member is one on-disk entry's identity-relevant fields plus its state.
type Member struct {
	OldHash   string `json:"old_hash"`
	GUID      string `json:"guid"`
	Link      string `json:"link"`
	Title     string `json:"title"`
	Published string `json:"published"`
	FetchedAt string `json:"fetched_at"`
	Read      bool   `json:"read"`
	Starred   bool   `json:"starred"`
}

// migEntry pairs an on-disk entry with its source file and derived hashes for
// the migration pass.
type migEntry struct {
	e       Entry
	file    string
	oldHash string
	newHash string
}

// MigrateIdentity runs (or, with dryRun, previews) the one-time sticky-identity
// migration on a data dir:
//
//  1. Seed each feed's sticky reuse set CONSERVATIVELY from on-disk data: a
//     same-guid group with >=2 distinct links whose guid is an OPAQUE TOKEN
//     (not an absolute URI) is genuine publisher reuse (CBC) → mark the guid
//     sticky, keep members distinct (D4). A URI-form guid (WordPress ?p=N,
//     Tumblr URL, tag:/urn:) is a stable identity contract → do NOT mark;
//     link/title drift is the same item edited → collapse (D1).
//  2. Recompute every entry hash under the new scheme (D4 iff guid sticky, else
//     D1 / link / title+published) and collapse resulting duplicates to one
//     survivor per identity. Survivor FetchedAt = EARLIEST member's FetchedAt
//     so timestampUsec does not resurface it as new in Reeder.
//  3. Remap read.log / starred.log hash columns IN PLACE so read/starred state
//     follows each entry to its survivor.
//
// The assigned hash is a pure function of (entry fields, sticky set); no batch,
// disk, title or published input enters it. dryRun computes and returns the
// full report (including the old→new map) without modifying anything. A real
// run is version-gated: if the marker is already present it is a no-op
// (Skipped). The migration is also structurally idempotent — a second run over
// migrated data finds single-member groups and a fixed-point recompute.
func MigrateIdentity(dir string, dryRun bool) (*IdentityReport, error) {
	rep := &IdentityReport{Version: identityMigrationVersion, DryRun: dryRun, HashRemap: map[string]string{}}

	if !dryRun {
		if done, err := identityMarkerPresent(dir); err != nil {
			return nil, err
		} else if done {
			rep.Skipped = true
			return rep, nil
		}
	}

	// Fold the current (pre-migration) state over canonical old hashes.
	readBefore, err := foldStateFile(filepath.Join(dir, "read.log"), 'r')
	if err != nil {
		return nil, err
	}
	starBefore, err := foldStateFile(filepath.Join(dir, "starred.log"), 's')
	if err != nil {
		return nil, err
	}

	feeds, err := feedHashDirs(dir)
	if err != nil {
		return nil, err
	}
	sort.Strings(feeds)

	// Plans built here; on a real run they are applied after accounting passes.
	st := &Store{Dir: dir}
	stickyPlan := map[string]map[string]bool{}     // feedHash -> sticky set (merged)
	survivors := map[string]map[string]*migEntry{} // feedHash -> newHash -> survivor
	fileEntries := map[string][]migEntry{}         // file path -> recomputed entries (in order)
	// collapseMembers maps a collapse-survivor newHash -> the canonical old
	// hashes that fold into it (only for groups with >1 member). Used to force
	// OR-semantics on the state remap: if ANY member was read/starred, the
	// survivor must end read/starred even if a losing member's final op was a
	// later unread/unstar.
	collapseMembers := map[string][]string{}

	for _, fh := range feeds {
		fr := FeedReport{FeedHash: fh}
		entries, err := gatherFeedEntries(dir, fh)
		if err != nil {
			return nil, err
		}
		sticky, err := st.loadStickyGUIDs(fh)
		if err != nil {
			return nil, err
		}

		// Seed sticky set from on-disk same-guid groups.
		byGUID := map[string][]migEntry{}
		for _, m := range entries {
			g := NormalizeGUID(strings.TrimSpace(m.e.GUID))
			if g == "" {
				continue
			}
			byGUID[g] = append(byGUID[g], m)
		}
		guids := make([]string, 0, len(byGUID))
		for g := range byGUID {
			guids = append(guids, g)
		}
		sort.Strings(guids)
		for _, g := range guids {
			group := byGUID[g]
			if distinctLinks(group) < 2 || IsAbsoluteURIGUID(g) || sticky[g] {
				continue
			}
			sticky[g] = true
			fr.StickyMarked = append(fr.StickyMarked, StickyMark{
				GUID:    g,
				Members: membersOf(group, readBefore, starBefore),
			})
			rep.Totals.StickyMarkings++
		}
		stickyPlan[fh] = sticky

		// Recompute hashes and build collapse groups.
		byNew := map[string][]migEntry{}
		for i := range entries {
			g := NormalizeGUID(strings.TrimSpace(entries[i].e.GUID))
			entries[i].newHash = entryHashKey(entries[i].e.GUID, entries[i].e.Link,
				entries[i].e.Title, entries[i].e.Published, g != "" && sticky[g])
			rep.HashRemap[entries[i].oldHash] = entries[i].newHash
			byNew[entries[i].newHash] = append(byNew[entries[i].newHash], entries[i])
		}

		survivors[fh] = map[string]*migEntry{}
		newHashes := make([]string, 0, len(byNew))
		for nh := range byNew {
			newHashes = append(newHashes, nh)
		}
		sort.Strings(newHashes)
		for _, nh := range newHashes {
			group := byNew[nh]
			surv := chooseSurvivor(group)
			survivors[fh][nh] = surv
			if len(group) > 1 {
				members := make([]string, len(group))
				for i, m := range group {
					members[i] = m.oldHash
				}
				collapseMembers[nh] = members
				fr.Collapses = append(fr.Collapses, CollapseGroup{
					NewHash:  nh,
					Survivor: membersOf([]migEntry{*surv}, readBefore, starBefore)[0],
					Members:  membersOf(group, readBefore, starBefore),
				})
				rep.Totals.IntendedCollapses += len(group) - 1
			}
		}

		rep.Totals.OldHashCount += len(entries)
		rep.Totals.SurvivorCount += len(byNew)
		if len(fr.StickyMarked) > 0 || len(fr.Collapses) > 0 {
			rep.Feeds = append(rep.Feeds, fr)
		}

		// Stage per-file rewrites (survivors only, in first-seen order).
		stageFileRewrites(entries, survivors[fh], fileEntries)
	}

	// Accounting: survivor_count = old_count - intended_collapses.
	if rep.Totals.SurvivorCount != rep.Totals.OldHashCount-rep.Totals.IntendedCollapses {
		return nil, fmt.Errorf("migration accounting: survivors=%d old=%d collapses=%d",
			rep.Totals.SurvivorCount, rep.Totals.OldHashCount, rep.Totals.IntendedCollapses)
	}

	// State accounting: every old read/starred hash must map to a survivor.
	readAfter := remapState(readBefore, rep.HashRemap)
	starAfter := remapState(starBefore, rep.HashRemap)
	rep.Totals.StarBefore = len(starBefore)
	rep.Totals.StarAfter = len(starAfter)
	rep.Totals.ReadBefore = len(readBefore)
	rep.Totals.ReadAfter = len(readAfter)
	if err := assertStatePreserved(starBefore, rep.HashRemap, survivors); err != nil {
		return nil, err
	}
	if err := assertStatePreserved(readBefore, rep.HashRemap, survivors); err != nil {
		return nil, err
	}

	if dryRun {
		return rep, nil
	}

	// Apply: rewrite entry files, persist sticky sets, remap state logs.
	if err := applyFileRewrites(fileEntries); err != nil {
		return nil, err
	}
	for fh, set := range stickyPlan {
		if len(set) == 0 {
			continue
		}
		if err := st.saveStickyGUIDs(fh, set); err != nil {
			return nil, err
		}
	}
	// Compute survivors whose OR of member states is positive, per kind, so the
	// in-place remap can drop losing negative ops and guarantee OR-semantics.
	readPositive := orPositiveSurvivors(collapseMembers, readBefore)
	starPositive := orPositiveSurvivors(collapseMembers, starBefore)
	if err := remapStateLog(filepath.Join(dir, "read.log"), rep.HashRemap, "u", readPositive); err != nil {
		return nil, err
	}
	if err := remapStateLog(filepath.Join(dir, "starred.log"), rep.HashRemap, "S", starPositive); err != nil {
		return nil, err
	}
	if err := writeIdentityMarker(dir); err != nil {
		return nil, err
	}
	return rep, nil
}

// distinctLinks counts the distinct (trimmed) links in a same-guid group.
func distinctLinks(group []migEntry) int {
	seen := map[string]bool{}
	for _, m := range group {
		seen[strings.TrimSpace(m.e.Link)] = true
	}
	return len(seen)
}

// chooseSurvivor picks the earliest-FetchedAt member (ties broken by oldHash),
// and sets its FetchedAt to the earliest in the group so timestampUsec cannot
// resurface it as new.
func chooseSurvivor(group []migEntry) *migEntry {
	idx := 0
	earliest := group[0].e.FetchedAt
	for i, m := range group {
		if m.e.FetchedAt.Before(earliest) ||
			(m.e.FetchedAt.Equal(earliest) && m.oldHash < group[idx].oldHash) {
			earliest = m.e.FetchedAt
			idx = i
		}
	}
	surv := group[idx]
	surv.e.Hash = surv.newHash
	surv.e.FetchedAt = earliest
	return &surv
}

// stageFileRewrites records, per source file, the survivor entries that belong
// to it, in the order they first appear in that file. Non-survivors are dropped.
func stageFileRewrites(entries []migEntry, surv map[string]*migEntry, out map[string][]migEntry) {
	emitted := map[string]bool{}
	for _, m := range entries {
		s := surv[m.newHash]
		if s == nil || emitted[m.newHash] || s.file != m.file {
			continue
		}
		emitted[m.newHash] = true
		rewritten := m
		rewritten.e = s.e // survivor's fields (recomputed hash + earliest fetched_at)
		out[m.file] = append(out[m.file], rewritten)
	}
}

// applyFileRewrites atomically rewrites each staged ndjson file. Files that end
// up empty (all members collapsed into a survivor living in another file) are
// truncated to empty rather than deleted, preserving the layout.
func applyFileRewrites(files map[string][]migEntry) error {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		var b strings.Builder
		for _, m := range files[p] {
			line, err := jsonMarshal(m.e)
			if err != nil {
				return err
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		if err := atomicWriteFile(p, []byte(b.String())); err != nil {
			return err
		}
	}
	return nil
}

// membersOf renders migEntries as report Members with folded state.
func membersOf(group []migEntry, read, star map[string]bool) []Member {
	out := make([]Member, 0, len(group))
	for _, m := range group {
		out = append(out, Member{
			OldHash:   m.oldHash,
			GUID:      m.e.GUID,
			Link:      m.e.Link,
			Title:     m.e.Title,
			Published: m.e.Published.UTC().Format(time.RFC3339),
			FetchedAt: m.e.FetchedAt.UTC().Format(time.RFC3339),
			Read:      read[m.oldHash],
			Starred:   star[m.oldHash],
		})
	}
	return out
}

// gatherFeedEntries reads every ndjson file for a feed, canonicalising each
// entry's stored hash. Returned in stable (file, line) order.
func gatherFeedEntries(dir, feedHash string) ([]migEntry, error) {
	feedDir := filepath.Join(dir, "entries", feedHash)
	ents, err := os.ReadDir(feedDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []migEntry
	for _, name := range names {
		path := filepath.Join(feedDir, name)
		if err := scanEntries(path, func(e Entry) error {
			out = append(out, migEntry{e: e, file: path, oldHash: StoreEntryHash(e.Hash)})
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// feedHashDirs lists the feed-hash subdirectories under entries/.
func feedHashDirs(dir string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(dir, "entries"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// foldStateFile folds one state log to the set of hashes whose final state is
// "live true" (read / starred), keyed by canonical old hash.
func foldStateFile(path string, kind byte) (map[string]bool, error) {
	live := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return live, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), " ", 3)
		if len(parts) != 3 {
			continue
		}
		hash := StoreEntryHash(parts[2])
		switch {
		case kind == 'r' && parts[1] == "r":
			live[hash] = true
		case kind == 'r' && parts[1] == "u":
			delete(live, hash)
		case kind == 's' && parts[1] == "s":
			live[hash] = true
		case kind == 's' && parts[1] == "S":
			delete(live, hash)
		}
	}
	return live, sc.Err()
}

// remapState maps a live-true set through the old→new hash remap.
func remapState(live map[string]bool, remap map[string]string) map[string]bool {
	out := make(map[string]bool, len(live))
	for h := range live {
		if nh, ok := remap[h]; ok {
			out[nh] = true
		} else {
			out[h] = true
		}
	}
	return out
}

// assertStatePreserved verifies that every live-true old hash maps to an actual
// survivor identity, so no read/starred state is orphaned by the collapse.
func assertStatePreserved(live map[string]bool, remap map[string]string, survivors map[string]map[string]*migEntry) error {
	known := map[string]bool{}
	for _, byNew := range survivors {
		for nh := range byNew {
			known[nh] = true
		}
	}
	for h := range live {
		nh, ok := remap[h]
		if !ok {
			// A state-log entry for a hash with no on-disk entry (e.g. an
			// archived-out item). Leave it untouched — it is not part of any
			// collapse and its identity is unchanged.
			continue
		}
		if !known[nh] {
			return fmt.Errorf("state remap: hash %s -> %s has no survivor", h, nh)
		}
	}
	return nil
}

// identityMarkerPresent reports whether the sticky-identity migration has
// already run on this data dir.
func identityMarkerPresent(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, identityMarkerName))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// writeIdentityMarker records that the migration completed, version-gating
// future real runs to a no-op.
func writeIdentityMarker(dir string) error {
	data, err := jsonMarshalIndent(struct {
		Version    string    `json:"version"`
		MigratedAt time.Time `json:"migrated_at"`
	}{identityMigrationVersion, time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(dir, identityMarkerName), data)
}

// orPositiveSurvivors returns the set of collapse-survivor new-hashes for which
// ANY member is live-true in the given state set. These survivors must fold to
// TRUE regardless of a losing member's later negative op.
func orPositiveSurvivors(collapseMembers map[string][]string, live map[string]bool) map[string]bool {
	out := map[string]bool{}
	for nh, members := range collapseMembers {
		for _, m := range members {
			if live[m] {
				out[nh] = true
				break
			}
		}
	}
	return out
}

// remapStateLog rewrites a state log's hash column IN PLACE (copy + atomic
// swap): each line's hash is canonicalised and remapped to its survivor. To
// enforce OR-semantics on a collapse, a NEGATIVE op line (negOp = "u" for
// read.log, "S" for starred.log) whose survivor is in orPositive is DROPPED —
// so only positive ops remain for that survivor and the fold yields true. All
// other lines are preserved verbatim (original timestamps intact; no new lines
// appended, which would shift GReader ordering/cutoff).
func remapStateLog(path string, remap map[string]string, negOp string, orPositive map[string]bool) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	var b strings.Builder
	changed := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 {
			canon := StoreEntryHash(parts[2])
			if rec, ok := remap[canon]; ok {
				canon = rec
			}
			// Drop a losing negative op that would otherwise turn an
			// OR-positive survivor false under last-op-wins folding.
			if parts[1] == negOp && orPositive[canon] {
				changed = true
				continue
			}
			if canon != parts[2] {
				changed = true
				line = parts[0] + " " + parts[1] + " " + canon
			}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return atomicWriteFile(path, []byte(b.String()))
}
