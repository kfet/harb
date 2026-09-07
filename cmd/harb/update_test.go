package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kfet/distkit"
	"github.com/kfet/distkit/installsh"
	"github.com/kfet/harb"
)

// Distribution tests. `harb update` and install.sh are both implemented by
// distkit (the library carries the download / verify / swap behaviour and its
// own tests); what has to be pinned *here* is the wiring — that harb's config
// names the assets kfet/harb actually publishes, that the tarball layout is
// unpacked, and that the generated installer still honours the invocations
// this repo's README documents.

// releaseAssetName is the naming rule kfet/harb releases actually use, e.g.
// harb-0.20.5-linux-armv6.tar.gz. Written out independently of the distkit
// template so a change to either side has to be a deliberate change to both.
func releaseAssetName(tag, goos, goarch string) string {
	if goarch == "arm" {
		goarch = "armv6"
	}
	return fmt.Sprintf("harb-%s-%s-%s.tar.gz", strings.TrimPrefix(tag, "v"), goos, goarch)
}

func TestUpdateConfigNamesPublishedAssets(t *testing.T) {
	cfg := updateConfig([]string{}, os.Stdout, os.Stderr)
	if got, want := cfg.AssetName("v0.20.5"), releaseAssetName("v0.20.5", runtime.GOOS, runtime.GOARCH); got != want {
		t.Errorf("AssetName = %q, want %q", got, want)
	}
	// The tag is also accepted without its leading v (VERSION file form).
	if got, want := cfg.AssetName("0.20.5"), releaseAssetName("v0.20.5", runtime.GOOS, runtime.GOARCH); got != want {
		t.Errorf("AssetName(no v) = %q, want %q", got, want)
	}
	if cfg.Repo != "kfet/harb" || cfg.Binary != "harb" {
		t.Errorf("repo/binary = %q/%q", cfg.Repo, cfg.Binary)
	}
	if cfg.ArmSuffix != "armv6" {
		t.Errorf("ArmSuffix = %q — harb publishes 32-bit ARM as armv6", cfg.ArmSuffix)
	}
	if cfg.Version != harb.Version {
		t.Errorf("Version = %q, want the compiled-in %q", cfg.Version, harb.Version)
	}
	if cfg.RestartHint == "" {
		t.Error("RestartHint must tell the operator how to recycle: a swapped binary is inert until re-exec")
	}
}

// TestInstallShSpecMatchesUpdateConfig pins the two definitions of asset
// naming — the one the binary self-updates with and the one install.sh is
// generated from — to each other.
func TestInstallShSpecMatchesUpdateConfig(t *testing.T) {
	spec, err := installsh.LoadSpec("../../install.sh.json")
	if err != nil {
		t.Fatal(err)
	}
	want := installsh.FromConfig(updateConfig([]string{}, os.Stdout, os.Stderr))
	if spec.Repo != want.Repo || spec.Binary != want.Binary ||
		spec.AssetTemplate != want.AssetTemplate || spec.ArmSuffix != want.ArmSuffix ||
		spec.AssetStem != want.AssetStem {
		t.Errorf("install.sh.json %+v disagrees with updateConfig %+v", spec, want)
	}
}

// TestInstallShIsNotDrifted fails when the checked-in install.sh is no longer
// what the distkit template produces for install.sh.json — i.e. someone hand
// edited the generated file. `make install.sh` regenerates it.
func TestInstallShIsNotDrifted(t *testing.T) {
	spec, err := installsh.LoadSpec("../../install.sh.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := installsh.CheckDrift("../../install.sh", spec); err != nil {
		t.Fatalf("install.sh has drifted: %v\nrun `make install.sh`", err)
	}
}

// fakeRelease is a stand-in for the GitHub REST API serving one harb release:
// the real two-step shape (resolve the release, then GET each asset's API URL)
// plus the plain releases/download path install.sh uses without a token.
type fakeRelease struct {
	*httptest.Server
	tag    string
	assets map[string][]byte

	mu   sync.Mutex
	seen []string // request paths, for asserting which asset was fetched
}

func (f *fakeRelease) requested(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.seen {
		if strings.HasSuffix(p, "/"+name) {
			return true
		}
	}
	return false
}

// aim points a Config at this fake release and forbids authentication. Both
// halves matter: since distkit v0.1.5 the anonymous path skips the REST API
// entirely and fetches from DownloadBase, so setting APIBase alone leaves the
// download going to the real github.com — and Anonymous pins which path is
// taken, so an ambient GITHUB_TOKEN cannot steer the test onto the API and
// mask that. Without this these tests downloaded and installed the actual
// published harb release on any token-less machine, i.e. CI.
func (f *fakeRelease) aim(cfg *distkit.Config) {
	cfg.APIBase, cfg.DownloadBase, cfg.Anonymous = f.URL, f.URL, true
}

func newFakeRelease(t *testing.T, tag string, assets map[string][]byte) *fakeRelease {
	t.Helper()
	f := &fakeRelease{tag: tag, assets: assets}
	// Unstarted, so f.Server (and therefore f.URL) is set before the first
	// request goroutine reads it.
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.Path)
		f.mu.Unlock()
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		// The anonymous path resolves `latest` from this redirect rather
		// than the API, exactly as the download host does.
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, strings.TrimSuffix(r.URL.Path, "latest")+"tag/"+f.tag, http.StatusFound)
			return
		}
		if data, ok := f.assets[name]; ok {
			_, _ = w.Write(data)
			return
		}
		if strings.Contains(r.URL.Path, "/releases/") {
			if i := strings.Index(r.URL.Path, "/tags/"); i >= 0 && r.URL.Path[i+len("/tags/"):] != f.tag {
				http.NotFound(w, r)
				return
			}
			var list []map[string]string
			for n := range f.assets {
				list = append(list, map[string]string{"name": n, "url": f.URL + "/assets/" + n})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag, "assets": list})
			return
		}
		http.NotFound(w, r)
	}))
	f.Start()
	t.Cleanup(f.Close)
	return f
}

// harbTarball builds a release tarball in harb's published layout:
// harb-<version>-<os>-<arch>/harb, with sibling README/LICENSE files that
// must not be mistaken for the binary.
func harbTarball(t *testing.T, tag, payload string) []byte {
	t.Helper()
	dir := fmt.Sprintf("harb-%s-%s-%s", strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{
			Name: dir + "/" + name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("README.md", "# harb\n")
	add("harb", payload)
	add("LICENSE", "MIT\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func checksumsFor(assets map[string][]byte) []byte {
	var b strings.Builder
	for name, data := range assets {
		h := sha256.Sum256(data)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), name)
	}
	return []byte(b.String())
}

// TestUpdateReplacesBinaryFromTarball is the behaviour the deleted
// internal/selfupdate package existed for: resolve the latest release, verify
// its sha256, pull "harb" out of the nested tarball, and atomically replace
// the running binary — end to end against a fake GitHub.
func TestUpdateReplacesBinaryFromTarball(t *testing.T) {
	const tag = "v99.0.0"
	asset := releaseAssetName(tag, runtime.GOOS, runtime.GOARCH)
	assets := map[string][]byte{asset: harbTarball(t, tag, "#!/bin/sh\necho new-harb\n")}
	assets["checksums.txt"] = checksumsFor(assets)
	srv := newFakeRelease(t, tag, assets)

	dir := t.TempDir()
	exe := filepath.Join(dir, "harb")
	if err := os.WriteFile(exe, []byte("old binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	cfg := updateConfig([]string{}, &out, &errOut)
	srv.aim(&cfg)
	cfg.Version = "v0.1.0"
	cfg.ExecPath = func() (string, error) { return exe, nil }
	cfg.DisableBrew = true

	if code := distkit.Main(cfg); code != 0 {
		t.Fatalf("update exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "#!/bin/sh\necho new-harb\n" {
		t.Fatalf("binary not replaced: %q", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: %v", fi.Mode())
	}
	for _, want := range []string{"checksum verified", tag, "recycle"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// TestUpdateRejectsCorruptedAsset: a tarball whose bytes do not match
// checksums.txt must abort before anything is swapped.
func TestUpdateRejectsCorruptedAsset(t *testing.T) {
	const tag = "v99.0.0"
	asset := releaseAssetName(tag, runtime.GOOS, runtime.GOARCH)
	assets := map[string][]byte{asset: harbTarball(t, tag, "#!/bin/sh\necho new-harb\n")}
	assets["checksums.txt"] = []byte(strings.Repeat("0", 64) + "  " + asset + "\n")
	srv := newFakeRelease(t, tag, assets)

	dir := t.TempDir()
	exe := filepath.Join(dir, "harb")
	if err := os.WriteFile(exe, []byte("old binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cfg := updateConfig([]string{}, &out, &errOut)
	srv.aim(&cfg)
	cfg.Version, cfg.DisableBrew = "v0.1.0", true
	cfg.ExecPath = func() (string, error) { return exe, nil }

	if code := distkit.Main(cfg); code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "checksum mismatch") {
		t.Errorf("stderr:\n%s", errOut.String())
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
		t.Errorf("binary was replaced despite a bad checksum: %q", got)
	}
}

// TestUpdateCheckOnly: -check reports without touching the binary, and exits
// 3 when a different release exists so a timer can act on it. `harb update
// -check` on an up-to-date host still exits 0.
func TestUpdateCheckOnly(t *testing.T) {
	const tag = "v99.0.0"
	asset := releaseAssetName(tag, runtime.GOOS, runtime.GOARCH)
	assets := map[string][]byte{asset: harbTarball(t, tag, "x")}
	assets["checksums.txt"] = checksumsFor(assets)
	srv := newFakeRelease(t, tag, assets)

	dir := t.TempDir()
	exe := filepath.Join(dir, "harb")
	if err := os.WriteFile(exe, []byte("old binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(version string, args ...string) (int, string) {
		var out, errOut bytes.Buffer
		cfg := updateConfig(args, &out, &errOut)
		srv.aim(&cfg)
		cfg.Version, cfg.DisableBrew = version, true
		cfg.ExecPath = func() (string, error) { return exe, nil }
		return distkit.Main(cfg), out.String() + errOut.String()
	}

	code, out := run("v0.1.0", "-check")
	if code != 3 {
		t.Errorf("-check with an update available exited %d, want 3\n%s", code, out)
	}
	if !strings.Contains(out, "update available") {
		t.Errorf("output:\n%s", out)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
		t.Errorf("-check must not touch the binary: %q", got)
	}

	if code, out := run(tag, "-check"); code != 0 {
		t.Errorf("-check when up to date exited %d, want 0\n%s", code, out)
	}

	// -version pins a release; an unknown tag fails rather than silently
	// installing latest.
	if code, _ := run("v0.1.0", "-version", "v1.2.3"); code != 1 {
		t.Errorf("pinning a nonexistent tag exited %d, want 1", code)
	}
	// Bad flags are a usage error.
	if code, _ := run("v0.1.0", "-nope"); code != 2 {
		t.Errorf("bad flag exited %d, want 2", code)
	}
	// -version is pasted into a release URL path, so a traversing value would
	// fetch another repo's asset *and* its self-agreeing checksums.txt. It
	// must be refused, not resolved.
	for _, bad := range []string{"../../kfet/other/releases/download/v1", "release/v1"} {
		code, out := run("v0.1.0", "-version", bad)
		if code != 1 || !strings.Contains(out, "bad version") {
			t.Errorf("-version %q exited %d, want 1 with a 'bad version' message\n%s", bad, code, out)
		}
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
		t.Errorf("a refused run must not touch the binary: %q", got)
	}
}

// TestUpdateRefusesManagedInstall: a binary under a package-manager prefix
// must be refused up front — before any network round-trip — with the advice
// to use that package manager. (The sibling ownership guard, for a directory
// belonging to another user, cannot be fabricated without root and is covered
// upstream in distkit.)
func TestUpdateRefusesManagedInstall(t *testing.T) {
	var out, errOut bytes.Buffer
	cfg := updateConfig([]string{}, &out, &errOut)
	cfg.Version, cfg.DisableBrew = "v0.1.0", true
	// Neither the API nor the download host may be dialled: the refusal
	// happens before any network work.
	cfg.APIBase, cfg.DownloadBase = "http://127.0.0.1:1", "http://127.0.0.1:1"
	cfg.Anonymous = true
	cfg.ExecPath = func() (string, error) { return "/usr/bin/harb", nil }

	if code := distkit.Main(cfg); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "package manager") {
		t.Errorf("a managed install must be refused with advice:\n%s", errOut.String())
	}
}

// TestInstallShHonoursDocumentedInvocations runs the *generated* installer
// against a fake GitHub: the default BIN_DIR form, and the legacy
// PREFIX=$HOME/.local form the README and existing runbooks use, which must
// keep resolving to $PREFIX/bin.
func TestInstallShHonoursDocumentedInvocations(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	const tag = "v99.0.0"
	// The installer maps uname itself; pin OS/ARCH so the asset name is
	// predictable on any test host.
	asset := releaseAssetName(tag, "linux", "amd64")
	payload := "#!/bin/sh\necho harb-from-installer\n"
	dirName := fmt.Sprintf("harb-%s-linux-amd64", strings.TrimPrefix(tag, "v"))

	stage := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "harb"), []byte(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	tgz := filepath.Join(t.TempDir(), "asset.tar.gz")
	if out, err := exec.Command("tar", "-czf", tgz, "-C", filepath.Dir(stage), dirName).CombinedOutput(); err != nil {
		t.Skipf("tar unavailable: %v %s", err, out)
	}
	data, err := os.ReadFile(tgz)
	if err != nil {
		t.Fatal(err)
	}
	assets := map[string][]byte{asset: data}
	assets["checksums.txt"] = checksumsFor(assets)
	srv := newFakeRelease(t, tag, assets)

	run := func(t *testing.T, env ...string) string {
		t.Helper()
		cmd := exec.Command("sh", "../../install.sh")
		cmd.Env = append(os.Environ(),
			"GITHUB_API="+srv.URL, "GITHUB_HOST="+srv.URL,
			"OS=linux", "ARCH=amd64", "GITHUB_TOKEN=", "BIN_DIR=", "PREFIX=")
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}
		return string(out)
	}

	assertInstalled := func(t *testing.T, path string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("nothing installed at %s: %v", path, err)
		}
		if string(got) != payload {
			t.Fatalf("installed %q", got)
		}
	}

	t.Run("BIN_DIR", func(t *testing.T) {
		dir := t.TempDir()
		run(t, "BIN_DIR="+filepath.Join(dir, "bin"))
		assertInstalled(t, filepath.Join(dir, "bin", "harb"))
	})

	// The README documents `... | PREFIX=$HOME/.local sh`; the generated
	// template keeps PREFIX as a legacy alias for $PREFIX/bin.
	t.Run("legacy PREFIX", func(t *testing.T) {
		prefix := t.TempDir()
		run(t, "PREFIX="+prefix)
		assertInstalled(t, filepath.Join(prefix, "bin", "harb"))
	})

	t.Run("VERSION pin", func(t *testing.T) {
		dir := t.TempDir()
		out := run(t, "BIN_DIR="+dir, "VERSION="+tag)
		assertInstalled(t, filepath.Join(dir, "harb"))
		if !strings.Contains(out, tag) {
			t.Errorf("output does not mention the installed version:\n%s", out)
		}
	})
}

// TestInstallShMapsArmSpellings: every 32-bit ARM uname (armv6l/armv7l/armv8l)
// must collapse to the single armv6 asset harb publishes.
func TestInstallShMapsArmSpellings(t *testing.T) {
	script, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	for _, spelling := range []string{"armv6l", "armv7l", "armv8l"} {
		if !strings.Contains(s, spelling) {
			t.Errorf("install.sh does not map %s", spelling)
		}
	}
	if !strings.Contains(s, "armv6") {
		t.Error("install.sh does not name the armv6 asset")
	}
}

// TestInstallShRejectsTraversingVersion: VERSION is pasted into two URL paths,
// so a value carrying a slash used to walk out of this repo entirely —
// `../../other/repo/releases/download/v1` installs another project's binary,
// and since checksums.txt is fetched from that same traversed location it
// verifies against itself and prints "checksum ok". The guard must refuse the
// value up front, before any request leaves the machine, hence the hit count.
func TestInstallShRejectsTraversingVersion(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	for _, bad := range []string{
		"../../other/repo/releases/download/v1",
		"v1.0.0/../../../etc",
		"release/v1",
		"v1 v2",
		"-oops",
	} {
		t.Run(bad, func(t *testing.T) {
			cmd := exec.Command("sh", "../../install.sh")
			cmd.Env = append(os.Environ(),
				"GITHUB_API="+srv.URL, "GITHUB_HOST="+srv.URL,
				"OS=linux", "ARCH=amd64", "GITHUB_TOKEN=",
				"BIN_DIR="+t.TempDir(), "PREFIX=", "VERSION="+bad)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("install.sh accepted VERSION=%q:\n%s", bad, out)
			}
			if !strings.Contains(string(out), "bad VERSION") {
				t.Errorf("VERSION=%q rejected without saying why:\n%s", bad, out)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("install.sh made %d request(s) before rejecting a bad VERSION", n)
	}
}

// TestUpdateRunsInstallsPinnedVersion: `-version` installs the pinned release,
// with or without a leading v, and reports success.
func TestUpdateInstallsPinnedVersion(t *testing.T) {
	const tag = "v99.0.0"
	asset := releaseAssetName(tag, runtime.GOOS, runtime.GOARCH)
	assets := map[string][]byte{asset: harbTarball(t, tag, "#!/bin/sh\necho pinned\n")}
	assets["checksums.txt"] = checksumsFor(assets)
	srv := newFakeRelease(t, tag, assets)

	for _, spelling := range []string{tag, strings.TrimPrefix(tag, "v")} {
		dir := t.TempDir()
		exe := filepath.Join(dir, "harb")
		if err := os.WriteFile(exe, []byte("old binary\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		cfg := updateConfig([]string{"-version", spelling}, &out, &errOut)
		srv.aim(&cfg)
		cfg.Version, cfg.DisableBrew = "v0.1.0", true
		cfg.ExecPath = func() (string, error) { return exe, nil }
		if code := distkit.Main(cfg); code != 0 {
			t.Fatalf("-version %s exited %d\n%s%s", spelling, code, out.String(), errOut.String())
		}
		if got, _ := os.ReadFile(exe); string(got) != "#!/bin/sh\necho pinned\n" {
			t.Errorf("-version %s did not install the pinned release: %q", spelling, got)
		}
	}
}

// TestUpdateRejectsUnlistedAsset: a release whose checksums.txt has no entry
// for our asset — what a half-broken release workflow publishes — must abort
// rather than install unverified bytes.
func TestUpdateRejectsUnlistedAsset(t *testing.T) {
	const tag = "v99.0.0"
	asset := releaseAssetName(tag, runtime.GOOS, runtime.GOARCH)
	assets := map[string][]byte{
		asset:           harbTarball(t, tag, "#!/bin/sh\necho new\n"),
		"checksums.txt": []byte(strings.Repeat("a", 64) + "  harb-0.0.0-nope-nope.tar.gz\n"),
	}
	srv := newFakeRelease(t, tag, assets)

	dir := t.TempDir()
	exe := filepath.Join(dir, "harb")
	if err := os.WriteFile(exe, []byte("old binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cfg := updateConfig([]string{}, &out, &errOut)
	srv.aim(&cfg)
	cfg.Version, cfg.DisableBrew = "v0.1.0", true
	cfg.ExecPath = func() (string, error) { return exe, nil }

	if code := distkit.Main(cfg); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out.String(), errOut.String())
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
		t.Errorf("binary replaced without a checksum entry: %q", got)
	}
}

// TestCmdUpdateDispatch pins the wiring the migration added: `harb update …`
// reaches distkit with the arguments that followed the subcommand word, and
// not with the test binary's own flags.
func TestCmdUpdateDispatch(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"update", "-not-a-flag"}, &out, &errOut); code != 2 {
		t.Errorf("harb update -not-a-flag exited %d, want 2\n%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "not-a-flag") {
		t.Errorf("the flag error must name the offending flag:\n%s", errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"update", "-h"}, &out, &errOut); code != 0 {
		t.Errorf("harb update -h exited %d, want 0", code)
	}
	if !strings.Contains(errOut.String(), "-restart-cmd") {
		t.Errorf("help does not list the distkit flags:\n%s", errOut.String())
	}
}

// TestInstallShMapsArmUnameToArmv6 runs the generated installer on a host
// pretending to be a 32-bit Pi: `uname -m` says armv7l, and the asset it must
// fetch is the single armv6 tarball harb publishes. This is the arch mapping
// the Raspberry Pi fleet depends on.
func TestInstallShMapsArmUnameToArmv6(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	const tag = "v99.0.0"
	for _, uname := range []string{"armv6l", "armv7l", "armv8l"} {
		t.Run(uname, func(t *testing.T) {
			asset := releaseAssetName(tag, "linux", "arm")
			if !strings.Contains(asset, "armv6") {
				t.Fatalf("asset %q does not name armv6", asset)
			}
			assets := map[string][]byte{asset: []byte("not a real tarball")}
			assets["checksums.txt"] = checksumsFor(assets)
			srv := newFakeRelease(t, tag, assets)

			// A fake uname earlier on PATH than the real one.
			shim := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo Linux ;; -m) echo %s ;; *) echo Linux ;; esac\n", uname)
			if err := os.WriteFile(filepath.Join(shim, "uname"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("sh", "../../install.sh")
			cmd.Env = append(os.Environ(),
				"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_API="+srv.URL, "GITHUB_HOST="+srv.URL,
				"BIN_DIR="+t.TempDir(), "GITHUB_TOKEN=", "OS=", "ARCH=", "PREFIX=")
			// The payload is not a real tarball, so the install fails at
			// extraction — after it has asked for an asset by name, which is
			// the thing under test.
			out, _ := cmd.CombinedOutput()
			if !srv.requested(asset) {
				t.Errorf("uname -m = %s did not fetch %s\n%s", uname, asset, out)
			}
		})
	}
}
