package store

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// migrateEntryHashes canonicalises the FORMAT of all persisted entry hashes to
// the current EntryHashLen before state logs are folded. It rewrites both entry
// NDJSON files and read/starred logs so the data dir never mixes legacy 20-char
// entry hashes with current 16-char hashes after a successful Open.
//
// It is FORMAT-only and identity-preserving: it applies StoreEntryHash (legacy
// 20→16 char truncation + high-bit mask) and collapses the resulting exact
// duplicates — a legacy unmasked hash and its high-bit-masked re-poll. It does
// NOT recompute identities, route D1/D4, or strip volatile guids. Those are
// data-affecting decisions reserved for the one-time, supervisor-gated
// identity migration (MigrateIdentity), which is previewable via a dry-run
// report before it touches live data.
func migrateEntryHashes(dir string) error {
	if err := migrateEntryFiles(filepath.Join(dir, "entries")); err != nil {
		return err
	}
	for _, name := range []string{"read.log", "starred.log"} {
		if err := migrateStateLog(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func migrateEntryFiles(root string) error {
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".ndjson") {
			return nil
		}
		return migrateEntryFile(path)
	}); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

// migrateEntryFile canonicalises hash FORMAT in one ndjson file and collapses
// exact-canonical duplicates (legacy unmasked hash vs its masked re-poll).
// Identity recompute + collapse is the job of MigrateIdentity, not this path.
func migrateEntryFile(path string) error {
	var all []Entry
	if err := scanEntries(path, func(e Entry) error {
		all = append(all, e)
		return nil
	}); err != nil {
		return err
	}

	var entries []Entry
	changed := false
	emitted := make(map[string]bool) // canonical hashes already kept in THIS file
	for _, e := range all {
		old := e.Hash
		canon := StoreEntryHash(old)
		if canon != old {
			e.Hash = canon
			changed = true
		}
		// Drop intra-file duplicates: a legacy unmasked hash and its
		// masked re-poll collapse to the same canonical id, so the same
		// article can sit in the file twice. Keep the first, prune the rest.
		if emitted[canon] {
			changed = true
			continue
		}
		emitted[canon] = true
		entries = append(entries, e)
	}
	if !changed {
		return nil
	}
	var b strings.Builder
	for _, e := range entries {
		line, err := jsonMarshal(e)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return atomicWriteFile(path, []byte(b.String()))
}

func migrateStateLog(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	changed := false
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 {
			// Format-only: canonicalise the hash column (legacy 20→16 char +
			// high-bit mask). Identity remaps are the job of MigrateIdentity.
			if canon := StoreEntryHash(parts[2]); canon != parts[2] {
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
