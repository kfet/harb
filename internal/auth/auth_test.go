package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeCfg(t *testing.T, user, pass string) Config {
	t.Helper()
	h, err := HashPassword(pass)
	if err != nil {
		t.Fatal(err)
	}
	return Config{Username: user, PasswordHash: h}
}

func TestVerifyAndIssue(t *testing.T) {
	dir := t.TempDir()
	cfg := makeCfg(t, "alice", "hunter2")
	s, err := OpenStore(filepath.Join(dir, "tokens.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Verify("alice", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Verify("alice", "nope"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("want invalid, got %v", err)
	}
	if err := cfg.Verify("eve", "hunter2"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("want invalid, got %v", err)
	}
	tok, err := s.IssueAPIToken("alice", "hunter2")
	if err != nil || tok == "" {
		t.Fatalf("issue: %v / %q", err, tok)
	}
	if !s.CheckAPIToken(tok) {
		t.Fatal("CheckAPIToken false")
	}
	if s.CheckAPIToken("bogus") || s.CheckAPIToken("") {
		t.Fatal("CheckAPIToken should be false")
	}
	// Sessions
	sess, err := s.IssueSession("alice", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !s.CheckSession(sess) || s.CheckSession("nope") || s.CheckSession("") {
		t.Fatal("session check")
	}
	// Revoke
	if err := s.RevokeSession(sess); err != nil {
		t.Fatal(err)
	}
	if s.CheckSession(sess) {
		t.Fatal("still valid after revoke")
	}
	// Issue fails on bad creds
	if _, err := s.IssueAPIToken("alice", "x"); err == nil {
		t.Fatal("expected creds error")
	}
	if _, err := s.IssueSession("alice", "x"); err == nil {
		t.Fatal("expected creds error")
	}
}

func TestVerifyMalformedHash(t *testing.T) {
	cases := []string{"", "no-dollars", "bad$x$y", "sha256$nothex$y", "sha256$00$nothex"}
	for _, h := range cases {
		c := Config{Username: "u", PasswordHash: h}
		if err := c.Verify("u", "p"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("hash=%q got %v", h, err)
		}
	}
}

func TestStorePersistAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	cfg := makeCfg(t, "u", "p")
	s, err := OpenStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := s.IssueAPIToken("u", "p")
	sess, _ := s.IssueSession("u", "p")
	s2, err := OpenStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.CheckAPIToken(tok) || !s2.CheckSession(sess) {
		t.Fatal("not restored")
	}
}

func TestOpenStoreBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	os.WriteFile(path, []byte("{bad"), 0o644)
	if _, err := OpenStore(path, Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenStoreOtherError(t *testing.T) {
	dir := t.TempDir()
	// Make the path a directory → ReadFile returns EISDIR.
	p := filepath.Join(dir, "tokens.json")
	os.MkdirAll(p, 0o755)
	if _, err := OpenStore(p, Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenStoreMissingIsOK(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "no.json"), Config{})
	if err != nil || s == nil {
		t.Fatalf("err=%v", err)
	}
}

func TestExtractAPIToken(t *testing.T) {
	r := httptest.NewRequest("GET", "/?T=fromform", nil)
	r.Header.Set("Authorization", `GoogleLogin auth="bearer"`)
	if got := ExtractAPIToken(r); got != "bearer" {
		t.Fatalf("got %q", got)
	}
	r2 := httptest.NewRequest("GET", "/?T=fromform", nil)
	if got := ExtractAPIToken(r2); got != "fromform" {
		t.Fatalf("form got %q", got)
	}
	r3 := httptest.NewRequest("GET", "/", nil)
	if got := ExtractAPIToken(r3); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	// Authorization without prefix
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.Header.Set("Authorization", "Basic abcdef")
	if got := ExtractAPIToken(r4); got != "" {
		t.Fatalf("got %q", got)
	}
	// Authorization with prefix but malformed pair
	r5 := httptest.NewRequest("GET", "/", nil)
	r5.Header.Set("Authorization", "GoogleLogin noequals")
	if got := ExtractAPIToken(r5); got != "" {
		t.Fatalf("got %q", got)
	}
	// Authorization with multi-param
	r6 := httptest.NewRequest("GET", "/", nil)
	r6.Header.Set("Authorization", "GoogleLogin SID=x, auth=tok")
	if got := ExtractAPIToken(r6); got != "tok" {
		t.Fatalf("got %q", got)
	}
}

func TestSessionCookieRoundtrip(t *testing.T) {
	w := httptest.NewRecorder()
	SetSessionCookie(w, "v1", true)
	res := w.Result()
	req := &http.Request{Header: http.Header{"Cookie": res.Header.Values("Set-Cookie")}}
	if got := SessionFromRequest(req); got != "v1" {
		t.Fatalf("got %q", got)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	if got := SessionFromRequest(r2); got != "" {
		t.Fatalf("got %q", got)
	}
	w2 := httptest.NewRecorder()
	ClearSessionCookie(w2)
	if !strings.Contains(w2.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("clear cookie: %q", w2.Header().Get("Set-Cookie"))
	}
}

func TestHashPasswordReadError(t *testing.T) {
	orig := randRead
	t.Cleanup(func() { randRead = orig })
	randRead = func([]byte) (int, error) { return 0, errors.New("rand-boom") }
	if _, err := HashPassword("p"); err == nil {
		t.Fatal("expected rand error")
	}
}

func TestIssueRandError(t *testing.T) {
	dir := t.TempDir()
	cfg := makeCfg(t, "u", "p")
	s, _ := OpenStore(filepath.Join(dir, "tokens.json"), cfg)
	orig := randRead
	t.Cleanup(func() { randRead = orig })
	randRead = func([]byte) (int, error) { return 0, errors.New("rand-boom") }
	// API tokens are deterministic now (no randomness), so IssueAPIToken
	// does not depend on randRead. Only sessions do.
	if _, err := s.IssueSession("u", "p"); err == nil {
		t.Fatal("expected rand error sess")
	}
}

func TestPersistMarshalAndMkdirFail(t *testing.T) {
	dir := t.TempDir()
	cfg := makeCfg(t, "u", "p")
	s, _ := OpenStore(filepath.Join(dir, "tokens.json"), cfg)
	// Marshal fail
	origJ := jsonMarshalIndent
	t.Cleanup(func() { jsonMarshalIndent = origJ })
	jsonMarshalIndent = func(any, string, string) ([]byte, error) { return nil, errors.New("m-boom") }
	if _, err := s.IssueSession("u", "p"); err == nil {
		t.Fatal("expected marshal err")
	}
	jsonMarshalIndent = origJ
	// Mkdir fail
	origM := osMkdirAll
	t.Cleanup(func() { osMkdirAll = origM })
	osMkdirAll = func(string, os.FileMode) error { return errors.New("mk-boom") }
	if _, err := s.IssueSession("u", "p"); err == nil {
		t.Fatal("expected mkdir err")
	}
}

// IssueAPIToken / IssueSession persist-locked failure: make tokens.json
// parent dir non-writable so atomic write fails.
func TestIssuePersistFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypass")
	}
	dir := t.TempDir()
	cfg := makeCfg(t, "u", "p")
	s, _ := OpenStore(filepath.Join(dir, "tokens.json"), cfg)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	// IssueAPIToken no longer persists (deterministic token), so only the
	// session paths can hit a persist failure.
	if _, err := s.IssueSession("u", "p"); err == nil {
		t.Fatal("expected persist err")
	}
	if err := s.RevokeSession("anything"); err == nil {
		t.Fatal("expected persist err")
	}
}

func TestStoreVerifyAndSetPasswordHash(t *testing.T) {
	hOrig, err := HashPassword("orig")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := OpenStore(filepath.Join(t.TempDir(), "tokens.json"), Config{Username: "u", PasswordHash: hOrig})
	if err := s.Verify("u", "orig"); err != nil {
		t.Fatalf("verify orig: %v", err)
	}
	hNew, _ := HashPassword("new!!")
	s.SetPasswordHash(hNew)
	if err := s.Verify("u", "orig"); err == nil {
		t.Fatal("orig should no longer verify")
	}
	if err := s.Verify("u", "new!!"); err != nil {
		t.Fatalf("new should verify: %v", err)
	}
}

func TestRevokeAllSessions(t *testing.T) {
	h, _ := HashPassword("p")
	s, _ := OpenStore(filepath.Join(t.TempDir(), "tokens.json"), Config{Username: "u", PasswordHash: h})
	t1, _ := s.IssueSession("u", "p")
	t2, _ := s.IssueSession("u", "p")
	if !s.CheckSession(t1) || !s.CheckSession(t2) {
		t.Fatal("sessions should exist pre-revoke")
	}
	if err := s.RevokeAllSessions(); err != nil {
		t.Fatal(err)
	}
	if s.CheckSession(t1) || s.CheckSession(t2) {
		t.Fatal("sessions should be gone")
	}
}

// TestOpenStoreSweepsExpired verifies that expired SESSIONS on disk are
// evicted at open and the file rewritten, while legacy API tokens survive
// regardless of age (they no longer expire).
func TestOpenStoreSweepsExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	old := time.Now().UTC().Add(-2 * TokenLifetime)
	fresh := time.Now().UTC()
	disk := struct {
		API      map[string]time.Time `json:"api"`
		Sessions map[string]time.Time `json:"sessions"`
	}{
		API:      map[string]time.Time{"old-api": old, "fresh-api": fresh},
		Sessions: map[string]time.Time{"old-sess": old, "fresh-sess": fresh},
	}
	data, _ := json.MarshalIndent(disk, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path, makeCfg(t, "a", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if s.CheckSession("old-sess") {
		t.Fatal("expired session should have been swept")
	}
	if !s.CheckSession("fresh-sess") {
		t.Fatal("fresh session should survive the sweep")
	}
	// Legacy API tokens are non-expiring: both must remain valid.
	if !s.CheckAPIToken("old-api") || !s.CheckAPIToken("fresh-api") {
		t.Fatal("legacy API tokens must survive regardless of age")
	}
	// The on-disk file must have been rewritten without the expired
	// session (reopen and re-check) but keep the legacy API tokens.
	s2, err := OpenStore(path, makeCfg(t, "a", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.sessions["old-sess"]; ok {
		t.Fatal("expired session still on disk")
	}
	if _, ok := s2.api["old-api"]; !ok {
		t.Fatal("legacy api token should still be on disk")
	}
}

// TestIssueSweepsExpired confirms the opportunistic sweep on issuing a
// session drops a session that has since expired, and that legacy API
// tokens are left untouched (non-expiring).
func TestIssueSweepsExpired(t *testing.T) {
	dir := t.TempDir()
	cfg := makeCfg(t, "a", "p")
	s, err := OpenStore(filepath.Join(dir, "tokens.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Plant an already-expired session and a legacy API token directly.
	s.sessions["stale-sess"] = time.Now().UTC().Add(-2 * TokenLifetime)
	s.api["legacy"] = time.Now().UTC().Add(-2 * TokenLifetime)
	if _, err := s.IssueSession("a", "p"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.sessions["stale-sess"]; ok {
		t.Fatal("issuing a session should have swept the stale one")
	}
	if _, ok := s.api["legacy"]; !ok {
		t.Fatal("legacy API token must not be swept")
	}
}

// TestOpenStoreSweepPersistError surfaces a persist failure from the
// open-time (session) sweep.
func TestOpenStoreSweepPersistError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	old := time.Now().UTC().Add(-2 * TokenLifetime)
	disk := struct {
		Sessions map[string]time.Time `json:"sessions"`
	}{Sessions: map[string]time.Time{"old": old}}
	data, _ := json.MarshalIndent(disk, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	origJ := jsonMarshalIndent
	t.Cleanup(func() { jsonMarshalIndent = origJ })
	jsonMarshalIndent = func(any, string, string) ([]byte, error) { return nil, errors.New("persist-boom") }
	if _, err := OpenStore(path, makeCfg(t, "a", "p")); err == nil {
		t.Fatal("expected persist error from open-time sweep")
	}
}

// TestAPITokenDeterministic verifies the API token is a pure function of
// the stored credentials: stable across independent Store loads/restarts,
// equal to what IssueAPIToken returns, and accepted by CheckAPIToken while
// garbage is rejected.
func TestAPITokenDeterministic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	cfg := makeCfg(t, "alice", "hunter2")

	s1, err := OpenStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s1.IssueAPIToken("alice", "hunter2")
	if err != nil || tok == "" {
		t.Fatalf("issue: %v / %q", err, tok)
	}
	// Prefixed with username + "/" (miniflux/FreshRSS shape).
	if !strings.HasPrefix(tok, "alice/") {
		t.Fatalf("token %q missing username/ prefix", tok)
	}
	// IssueAPIToken must return exactly the deterministic token.
	if tok != s1.apiToken() {
		t.Fatalf("IssueAPIToken=%q != apiToken()=%q", tok, s1.apiToken())
	}
	if !s1.CheckAPIToken(tok) {
		t.Fatal("deterministic token should pass CheckAPIToken")
	}
	if s1.CheckAPIToken("garbage") || s1.CheckAPIToken("alice/deadbeef") || s1.CheckAPIToken("") {
		t.Fatal("wrong/garbage tokens must fail CheckAPIToken")
	}

	// A completely independent Store load (simulating a restart) with the
	// same config yields the identical token and accepts the earlier one.
	s2, err := OpenStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.apiToken(); got != tok {
		t.Fatalf("token not stable across restart: %q != %q", got, tok)
	}
	if !s2.CheckAPIToken(tok) {
		t.Fatal("token from first load rejected after restart")
	}
}

// TestIssueAPITokenBadCreds ensures wrong credentials are rejected.
func TestIssueAPITokenBadCreds(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "tokens.json"), makeCfg(t, "u", "p"))
	if _, err := s.IssueAPIToken("u", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
	if _, err := s.IssueAPIToken("eve", "p"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
}

// TestAPITokenNonExpiring verifies the deterministic API token survives an
// arbitrarily large clock advance, while a UI session issued at the same
// time still expires at TokenLifetime.
func TestAPITokenNonExpiring(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "tokens.json"), makeCfg(t, "u", "p"))
	base := time.Now().UTC()
	s.now = func() time.Time { return base }

	tok, _ := s.IssueAPIToken("u", "p")
	sess, _ := s.IssueSession("u", "p")
	if !s.CheckAPIToken(tok) || !s.CheckSession(sess) {
		t.Fatal("both should be valid at issue time")
	}

	// Advance well past TokenLifetime.
	s.now = func() time.Time { return base.Add(60 * 24 * time.Hour) }
	if !s.CheckAPIToken(tok) {
		t.Fatal("API token must NOT expire")
	}
	if s.CheckSession(sess) {
		t.Fatal("UI session MUST still expire at TokenLifetime")
	}
}

// TestLegacyAPITokenAcceptedThenClearedOnPasswordChange verifies a legacy
// random token already on disk is honoured without expiry, and that a
// password change both invalidates it and rotates the deterministic token.
func TestLegacyAPITokenAcceptedThenClearedOnPasswordChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	cfg := makeCfg(t, "u", "p")
	s, err := OpenStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-seed a legacy random token with an ancient issued-at.
	legacy := "legacy-random-token"
	s.api[legacy] = time.Now().UTC().Add(-10 * TokenLifetime)
	if !s.CheckAPIToken(legacy) {
		t.Fatal("legacy token should be accepted without expiry")
	}

	before := s.apiToken()

	// Change the password: legacy tokens die, deterministic token rotates.
	newHash, err := HashPassword("newpass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPasswordHash(newHash); err != nil {
		t.Fatal(err)
	}
	if s.CheckAPIToken(legacy) {
		t.Fatal("legacy token must be cleared after password change")
	}
	after := s.apiToken()
	if before == after {
		t.Fatal("deterministic token must rotate on password change")
	}
	if !s.CheckAPIToken(after) {
		t.Fatal("new deterministic token should be valid")
	}
	// The cleared map must have been persisted.
	s2, err := OpenStore(path, Config{Username: "u", PasswordHash: newHash})
	if err != nil {
		t.Fatal(err)
	}
	if s2.CheckAPIToken(legacy) {
		t.Fatal("legacy token must not survive on disk after password change")
	}
}

// TestSetPasswordHashPersistError covers the persist-failure path of
// SetPasswordHash.
func TestSetPasswordHashPersistError(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "tokens.json"), makeCfg(t, "u", "p"))
	origJ := jsonMarshalIndent
	t.Cleanup(func() { jsonMarshalIndent = origJ })
	jsonMarshalIndent = func(any, string, string) ([]byte, error) { return nil, errors.New("persist-boom") }
	if err := s.SetPasswordHash("sha256$00$00"); err == nil {
		t.Fatal("expected persist error from SetPasswordHash")
	}
}

// TestDeterministicTokenSurvivesExtraction confirms the username/"..." token
// (containing a "/") round-trips through ExtractAPIToken via the GoogleLogin
// Authorization header and the T= form field.
func TestDeterministicTokenSurvivesExtraction(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "tokens.json"), makeCfg(t, "u", "p"))
	tok, _ := s.IssueAPIToken("u", "p")

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "GoogleLogin auth="+tok)
	if got := ExtractAPIToken(r); got != tok {
		t.Fatalf("auth header: got %q want %q", got, tok)
	}
	r2 := httptest.NewRequest("GET", "/?T="+tok, nil)
	if got := ExtractAPIToken(r2); got != tok {
		t.Fatalf("T form: got %q want %q", got, tok)
	}
}
