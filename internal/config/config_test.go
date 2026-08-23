package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kfet/harb/internal/store"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen == "" || c.UI.Theme == "" {
		t.Fatalf("bad defaults: %+v", c)
	}
}

func TestLoadAndSave(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	c.Listen = ":7000"
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Listen != ":7000" {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadBadJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte("{bad"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected err")
	}
}

func TestLoadOtherError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.MkdirAll(p, 0o755)
	if _, err := Load(p); err == nil {
		t.Fatal("expected err")
	}
}

func TestLoadEmptyDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte("{}"), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen == "" || c.UI.Theme == "" {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestFileOPMLRoundtrip(t *testing.T) {
	dir := t.TempDir()
	f := NewFileOPML(dir)
	o, err := f.Load()
	if err != nil || len(o.Feeds) != 0 {
		t.Fatalf("empty load: %+v err=%v", o, err)
	}
	o.Feeds = []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}
	if err := f.Save(o); err != nil {
		t.Fatal(err)
	}
	o2, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(o2.Feeds) != 1 {
		t.Fatalf("feeds=%d", len(o2.Feeds))
	}
}

// TestFileOPMLLoadFromExistingFile exercises the disk-read success
// path of ensureLoaded: a valid subscriptions.opml already exists when
// the first Load is called, and that data must come back unchanged.
func TestFileOPMLLoadFromExistingFile(t *testing.T) {
	dir := t.TempDir()
	// Write a real OPML to disk via a separate FileOPML, then construct
	// a fresh one over the same path so the first Load actually reads
	// the file (rather than serving an already-populated in-mem state).
	seed := &store.OPML{Feeds: []store.Feed{
		{XMLURL: "https://a.example/feed", Title: "A", Tags: []string{"x"}},
		{XMLURL: "https://b.example/feed", Title: "B"},
	}}
	if err := NewFileOPML(dir).Save(seed); err != nil {
		t.Fatal(err)
	}
	f := NewFileOPML(dir)
	o, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Feeds) != 2 {
		t.Fatalf("feeds=%d, want 2; got %+v", len(o.Feeds), o.Feeds)
	}
	if o.Feeds[0].XMLURL != "https://a.example/feed" || o.Feeds[0].Tags[0] != "x" {
		t.Errorf("feed[0]=%+v", o.Feeds[0])
	}
}

// TestFileOPMLLoadError tests the non-NotExist error surface.
func TestFileOPMLLoadError(t *testing.T) {
	dir := t.TempDir()
	f := &FileOPML{Path: filepath.Join(dir, "sub.opml")}
	// Make path a directory → ReadOPML returns EISDIR error.
	os.MkdirAll(f.Path, 0o755)
	if _, err := f.Load(); err == nil {
		t.Fatal("expected err")
	}
}

// TestFileOPMLReadsOnce asserts the disk file is read exactly once
// across many Load calls: after loading once via the first Load,
// removing the on-disk file must NOT affect subsequent Loads.
func TestFileOPMLReadsOnce(t *testing.T) {
	dir := t.TempDir()
	f := NewFileOPML(dir)
	seed := &store.OPML{Feeds: []store.Feed{{XMLURL: "https://x/feed", Title: "X", Tags: []string{"t"}}}}
	if err := f.Save(seed); err != nil {
		t.Fatal(err)
	}
	// Populate in-mem state via first Load.
	if _, err := f.Load(); err != nil {
		t.Fatal(err)
	}
	// Yank the file from underneath; in-mem must still serve.
	if err := os.Remove(f.Path); err != nil {
		t.Fatal(err)
	}
	o, err := f.Load()
	if err != nil {
		t.Fatalf("Load after file removal: %v (must serve from memory)", err)
	}
	if len(o.Feeds) != 1 || o.Feeds[0].XMLURL != "https://x/feed" {
		t.Fatalf("in-mem load returned %+v, want seeded feed", o.Feeds)
	}
}

// TestFileOPMLLoadIsolation asserts Load returns a defensive deep copy:
// mutations to the returned value must not leak into the in-mem state or
// subsequent Loads.
func TestFileOPMLLoadIsolation(t *testing.T) {
	dir := t.TempDir()
	f := NewFileOPML(dir)
	if err := f.Save(&store.OPML{Feeds: []store.Feed{{XMLURL: "https://x", Tags: []string{"a"}}}}); err != nil {
		t.Fatal(err)
	}
	a, _ := f.Load()
	a.Feeds[0].XMLURL = "MUTATED"
	a.Feeds[0].Tags[0] = "MUTATED"
	a.Feeds = append(a.Feeds, store.Feed{XMLURL: "added"})
	b, _ := f.Load()
	if b.Feeds[0].XMLURL != "https://x" {
		t.Errorf("XMLURL mutation leaked: %q", b.Feeds[0].XMLURL)
	}
	if b.Feeds[0].Tags[0] != "a" {
		t.Errorf("Tags mutation leaked: %v", b.Feeds[0].Tags)
	}
	if len(b.Feeds) != 1 {
		t.Errorf("Feeds slice append leaked: %d entries", len(b.Feeds))
	}
}

// TestFileOPMLSaveFailureKeepsState asserts a failed disk write leaves
// the in-memory state untouched (no torn / partial state). Failure is
// induced by making the target path a directory so the atomic rename
// inside WriteOPML fails; if atomic.WriteFileMode ever changes semantics
// this trigger may need revisiting.
func TestFileOPMLSaveFailureKeepsState(t *testing.T) {
	dir := t.TempDir()
	f := NewFileOPML(dir)
	good := &store.OPML{Feeds: []store.Feed{{XMLURL: "good", Title: "G"}}}
	if err := f.Save(good); err != nil {
		t.Fatal(err)
	}
	// Replace the file path with a directory to force WriteOPML to fail.
	os.Remove(f.Path)
	if err := os.MkdirAll(f.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := &store.OPML{Feeds: []store.Feed{{XMLURL: "bad"}}}
	if err := f.Save(bad); err == nil {
		t.Fatal("expected Save to fail with path-is-dir")
	}
	got, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Feeds) != 1 || got.Feeds[0].XMLURL != "good" {
		t.Errorf("in-mem state corrupted by failed Save: %+v", got.Feeds)
	}
}

func TestSaveMarshalIsAlwaysOK(t *testing.T) {
	// MarshalIndent doesn't error on Config values; smoke-test that Save
	// surfaces atomic write errors.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blk")
	os.MkdirAll(blocker, 0o755)
	if err := Save(blocker, Default()); err == nil {
		t.Fatal("expected atomic err")
	}
}

func TestSaveMarshalError(t *testing.T) {
	orig := jsonMarshalIndent
	t.Cleanup(func() { jsonMarshalIndent = orig })
	jsonMarshalIndent = func(any, string, string) ([]byte, error) {
		return nil, &osPathError{err: "boom"}
	}
	if err := Save("/tmp/ignored", Default()); err == nil {
		t.Fatal("expected marshal err")
	}
}

type osPathError struct{ err string }

func (e *osPathError) Error() string { return e.err }

func TestLoadExplicitEmpties(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte(`{"listen":"", "ui":{"theme":""}}`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen == "" || c.UI.Theme == "" {
		t.Fatalf("defaults not re-applied: %+v", c)
	}
}

// TestFileOPMLUpdateSerializes verifies Update performs an atomic
// read-modify-write: many concurrent Add operations must not lose
// updates the way separate Load→mutate→Save calls would.
func TestFileOPMLUpdateSerializes(t *testing.T) {
	dir := t.TempDir()
	f := NewFileOPML(dir)
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			err := f.Update(func(op *store.OPML) error {
				op.Add(store.Feed{XMLURL: fmt.Sprintf("u%d", i), Title: "t"})
				return nil
			})
			if err != nil {
				t.Errorf("update %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	got, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Feeds) != n {
		t.Fatalf("lost updates: want %d feeds, got %d", n, len(got.Feeds))
	}
}

// TestFileOPMLUpdateAbort confirms that returning an error from the
// closure leaves the on-disk and in-memory state untouched.
func TestFileOPMLUpdateAbort(t *testing.T) {
	dir := t.TempDir()
	f := NewFileOPML(dir)
	if err := f.Save(&store.OPML{Feeds: []store.Feed{{XMLURL: "keep", Title: "K"}}}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("boom")
	err := f.Update(func(op *store.OPML) error {
		op.Add(store.Feed{XMLURL: "transient"})
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("want sentinel, got %v", err)
	}
	got, _ := f.Load()
	if len(got.Feeds) != 1 || got.Feeds[0].XMLURL != "keep" {
		t.Fatalf("aborted update persisted: %+v", got.Feeds)
	}
}

// TestFileOPMLUpdateLoadError surfaces ensureLoaded errors (a directory
// where the OPML file is expected).
func TestFileOPMLUpdateLoadError(t *testing.T) {
	dir := t.TempDir()
	f := &FileOPML{Path: filepath.Join(dir, "sub.opml")}
	if err := os.MkdirAll(f.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(func(*store.OPML) error { return nil }); err == nil {
		t.Fatal("expected load error from Update")
	}
}

// TestFileOPMLUpdateWriteError surfaces persistence errors from Update.
func TestFileOPMLUpdateWriteError(t *testing.T) {
	dir := t.TempDir()
	f := NewFileOPML(dir)
	if err := f.Save(&store.OPML{Feeds: []store.Feed{{XMLURL: "good", Title: "G"}}}); err != nil {
		t.Fatal(err)
	}
	os.Remove(f.Path)
	if err := os.MkdirAll(f.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	err := f.Update(func(op *store.OPML) error {
		op.Add(store.Feed{XMLURL: "bad"})
		return nil
	})
	if err == nil {
		t.Fatal("expected write error from Update")
	}
	got, _ := f.Load()
	if len(got.Feeds) != 1 || got.Feeds[0].XMLURL != "good" {
		t.Fatalf("failed Update corrupted state: %+v", got.Feeds)
	}
}

// TestLinkRewriteRoundTrip pins the ui.link_rewrite map through
// Save→Load and through a hand-written config.json, since operators
// edit that file by hand.
func TestLinkRewriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	c.UI.LinkRewrite = map[string]string{"x.com": "xcancel.com", "twitter.com": "xcancel.com"}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.UI.LinkRewrite["x.com"] != "xcancel.com" || got.UI.LinkRewrite["twitter.com"] != "xcancel.com" {
		t.Fatalf("round trip lost rules: %+v", got.UI.LinkRewrite)
	}

	// Hand-written form, and the default (absent) case.
	hand := filepath.Join(dir, "hand.json")
	if err := os.WriteFile(hand, []byte(`{"ui":{"link_rewrite":{"x.com":"xcancel.com"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := Load(hand)
	if err != nil {
		t.Fatal(err)
	}
	if h.UI.LinkRewrite["x.com"] != "xcancel.com" {
		t.Fatalf("hand-written config: %+v", h.UI)
	}
	if Default().UI.LinkRewrite != nil {
		t.Fatal("link_rewrite must be empty by default")
	}
}

// TestEffectiveLinkRewrite pins the top-level `link_rewrite` map plus
// its one-line back-compat fallback to the deprecated `ui.link_rewrite`
// spelling. Live deployments configured before v0.20.5 carry the map
// under `ui`, so the fallback is load-bearing, not decorative.
func TestEffectiveLinkRewrite(t *testing.T) {
	dir := t.TempDir()

	// Top-level round trip through Save→Load.
	p := filepath.Join(dir, "top.json")
	c := Default()
	c.LinkRewrite = map[string]string{"x.com": "xcancel.com"}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.EffectiveLinkRewrite()["x.com"] != "xcancel.com" {
		t.Fatalf("top-level round trip: %+v", got)
	}

	// Legacy ui.link_rewrite only → used as the effective map.
	legacy := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"ui":{"link_rewrite":{"x.com":"old.example"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if l.EffectiveLinkRewrite()["x.com"] != "old.example" {
		t.Fatalf("ui fallback: %+v", l)
	}

	// Both set → top level wins outright (no merging).
	both := filepath.Join(dir, "both.json")
	if err := os.WriteFile(both, []byte(
		`{"link_rewrite":{"x.com":"new.example"},"ui":{"link_rewrite":{"x.com":"old.example","twitter.com":"old.example"}}}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(both)
	if err != nil {
		t.Fatal(err)
	}
	eff := b.EffectiveLinkRewrite()
	if eff["x.com"] != "new.example" {
		t.Fatalf("top level must win: %+v", eff)
	}
	if _, ok := eff["twitter.com"]; ok {
		t.Fatalf("maps must not be merged: %+v", eff)
	}

	// Neither set → empty, so a zero-config deployment rewrites nothing.
	if len(Default().EffectiveLinkRewrite()) != 0 {
		t.Fatal("default must have no rules")
	}
}

// TestFileOPMLEnsureLoadedContended pins ensureLoaded's double-checked
// early return: a caller that passed the read-locked fast check while
// the state was still empty, then blocked on the write lock behind the
// caller doing the one-time disk read, must return without re-reading.
//
// Reaching that branch needs BOTH callers past the fast check before
// either takes the write lock — start the second one later and it parks
// on the READ lock instead, returning from the fast check. The
// slow-path hook is therefore used as a barrier: both callers are held
// on exactly that boundary until both have arrived, then released
// together. One wins the write lock and is parked inside readOPML; the
// other has the whole of that window to call f.mu.Lock() and wake into
// the double check.
func TestFileOPMLEnsureLoadedContended(t *testing.T) {
	dir := t.TempDir()
	f := NewFileOPML(dir)
	barrier := make(chan struct{})
	var arrived int32
	origSlow, origRead := ensureLoadedSlowPath, readOPML
	ensureLoadedSlowPath = func() {
		if atomic.AddInt32(&arrived, 1) == 2 {
			close(barrier)
		}
		<-barrier
	}
	// The winner holds the write lock across this read; the sleep is the
	// window the loser has to park on that lock (it needs only the few
	// instructions between the barrier and Lock, so 50ms is ample).
	readOPML = func(string) (*store.OPML, error) {
		time.Sleep(50 * time.Millisecond)
		return &store.OPML{Feeds: []store.Feed{{XMLURL: "https://x/feed", Title: "X"}}}, nil
	}
	defer func() { ensureLoadedSlowPath, readOPML = origSlow, origRead }()

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); check(t, f.ensureLoaded()) }()
	}
	wg.Wait()

	o, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Feeds) != 1 || o.Feeds[0].XMLURL != "https://x/feed" {
		t.Fatalf("loaded %+v, want the single read feed", o.Feeds)
	}
}

// check fails the test on a non-nil error from a helper goroutine.
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("ensureLoaded: %v", err)
	}
}
