// Package config holds the on-disk configuration for harb and the
// concrete OPMLProvider used by the UI and Reader API.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kfet/harb/internal/atomic"
	"github.com/kfet/harb/internal/auth"
	"github.com/kfet/harb/internal/passkey"
	"github.com/kfet/harb/internal/store"
)

// Config is the on-disk configuration loaded from `config.json` in the
// data dir. v0.1 is plain JSON; switching to TOML is a v0.2 conversation.
type Config struct {
	Listen   string         `json:"listen"`
	Auth     auth.Config    `json:"auth"`
	UI       UIConfig       `json:"ui"`
	WebAuthn passkey.Config `json:"webauthn,omitempty"`

	// LinkRewrite maps a link host to a replacement host for outbound
	// links harb SERVES — the web UI's entry bodies and source links,
	// and the <a href> values inside article bodies served by the
	// Google Reader API. Empty by default: harb stays neutral and this
	// is strictly opt-in. Canonical example:
	//
	//	"link_rewrite": {"x.com": "xcancel.com", "twitter.com": "xcancel.com"}
	//
	// Keys and values are bare hosts (no scheme, no path, no port).
	// Junk entries are ignored at use rather than failing config load.
	// A Reader item's `id` and `alternate[].href` are deliberately NOT
	// rewritten: clients treat them as identity-adjacent (dedupe / read
	// state). Nothing on disk is rewritten either — this is serve-time
	// only, so removing the map restores the original links.
	LinkRewrite map[string]string `json:"link_rewrite,omitempty"`
}

// EffectiveLinkRewrite returns the link-rewrite rules in force, falling
// back to the deprecated UI-scoped map when the top-level one is unset.
// The setting was UI-only in v0.20.4 and lives at the top level from
// v0.20.5 on; existing configs keep working untouched.
func (c Config) EffectiveLinkRewrite() map[string]string {
	if len(c.LinkRewrite) == 0 {
		return c.UI.LinkRewrite
	}
	return c.LinkRewrite
}

// UIConfig governs the web UI presentation.
type UIConfig struct {
	Theme  string `json:"theme,omitempty"`
	Secure bool   `json:"secure,omitempty"`

	// LinkRewrite is the DEPRECATED UI-scoped spelling of the top-level
	// Config.LinkRewrite; it was UI-only in v0.20.4. It is still honoured
	// when the top-level map is unset (see Config.EffectiveLinkRewrite),
	// but new configs should set `link_rewrite` at the top level, which
	// also covers the Reader API's article bodies.
	LinkRewrite map[string]string `json:"link_rewrite,omitempty"`
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{Listen: ":8088", UI: UIConfig{Theme: "auto"}}
}

// Load reads the config from path. Missing → Default(), no error.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return Config{}, err
	}
	c := Default()
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse: %w", err)
	}
	if c.Listen == "" {
		c.Listen = ":8088"
	}
	if c.UI.Theme == "" {
		c.UI.Theme = "auto"
	}
	return c, nil
}

// Hookable for testing.
var jsonMarshalIndent = json.MarshalIndent

// Hookable for testing: lets a test hold the one-time OPML read open
// long enough to exercise ensureLoaded's contended path.
var readOPML = store.ReadOPML

// Hookable for testing: a no-op in production, called on ensureLoaded's
// slow path after the read-locked fast check and before the write lock
// is taken. A test uses it to line two callers up on exactly that
// boundary, which is the only way to reach the double-checked return
// deterministically.
var ensureLoadedSlowPath = func() {}

// Save atomically writes the config.
func Save(path string, c Config) error {
	data, err := jsonMarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomic.WriteFileMode(path, data, 0o600)
}

// FileOPML is a concrete store.OPMLProvider. The authoritative
// subscription state lives in memory as a *store.OPML; the on-disk
// subscriptions.opml file is the persistence layer.
//
// Lifecycle:
//   - First Load (or Save) populates the in-memory state. On first
//     Load this means reading and parsing the file once (missing file
//     → empty OPML). After that, no further disk reads ever occur.
//   - Load returns a defensive deep copy of the in-memory state so
//     callers can mutate freely without aliasing.
//   - Save serializes the supplied OPML, atomic-writes it to disk,
//     and on success replaces the in-memory state with a defensive
//     copy. A failed write leaves the in-memory state untouched.
//
// FileOPML is the sole writer of subscriptions.opml in-process.
type FileOPML struct {
	Path string
	mu   sync.RWMutex
	cur  *store.OPML // nil until first Load/Save populates it
}

// ensureLoaded reads the file once into memory. Caller must hold no
// lock. Safe to call concurrently.
func (f *FileOPML) ensureLoaded() error {
	f.mu.RLock()
	if f.cur != nil {
		f.mu.RUnlock()
		return nil
	}
	f.mu.RUnlock()
	ensureLoadedSlowPath()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cur != nil {
		return nil
	}
	o, err := readOPML(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			f.cur = &store.OPML{}
			return nil
		}
		return err
	}
	f.cur = o
	return nil
}

// Load returns a defensive deep copy of the in-memory OPML. Triggers a
// one-time disk read on the first call; never reads disk again.
func (f *FileOPML) Load() (*store.OPML, error) {
	if err := f.ensureLoaded(); err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return cloneOPML(f.cur), nil
}

// Save writes the OPML atomically to disk and, on success, replaces
// the in-memory state. A failed disk write leaves the in-memory state
// untouched.
func (f *FileOPML) Save(o *store.OPML) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := o.WriteOPML(f.Path); err != nil {
		return err
	}
	f.cur = cloneOPML(o)
	return nil
}

// Update performs a serialized read-modify-write on the OPML. It holds
// the FileOPML lock across the entire load→mutate→save cycle, so
// concurrent mutators (from the UI and Reader API, which share one
// *FileOPML) can no longer interleave and clobber each other's edits.
//
// fn receives a private deep copy of the current in-memory OPML and may
// mutate it freely. If fn returns an error, nothing is persisted and
// the in-memory state is left untouched. On success the mutated OPML is
// atomic-written to disk and becomes the new in-memory state.
//
// All OPML mutations in-process must go through Update; using Load then
// Save separately reintroduces the lost-update race this method exists
// to close.
func (f *FileOPML) Update(fn func(*store.OPML) error) error {
	if err := f.ensureLoaded(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	work := cloneOPML(f.cur)
	if err := fn(work); err != nil {
		return err
	}
	if err := work.WriteOPML(f.Path); err != nil {
		return err
	}
	f.cur = cloneOPML(work)
	return nil
}

// cloneOPML returns a deep copy with independent Feeds and Feed.Tags
// slices so callers can mutate the returned value without aliasing the
// in-memory state. Callers must pass a non-nil *store.OPML.
func cloneOPML(o *store.OPML) *store.OPML {
	cp := *o
	if len(o.Feeds) > 0 {
		cp.Feeds = make([]store.Feed, len(o.Feeds))
		copy(cp.Feeds, o.Feeds)
		for i, f := range o.Feeds {
			if len(f.Tags) > 0 {
				cp.Feeds[i].Tags = append([]string(nil), f.Tags...)
			}
		}
	}
	return &cp
}

// NewFileOPML returns a FileOPML for a data dir.
func NewFileOPML(dataDir string) *FileOPML {
	return &FileOPML{Path: filepath.Join(dataDir, "subscriptions.opml")}
}
