// Command harb is the single-binary entry point.
//
// Subcommands:
//
//	harb init [-data DIR] [-username NAME] [-password PASS]
//	              [-listen ADDR] [-theme NAME] [-force]
//	harb serve [-data DIR] [-config FILE]
//	harb import [-data DIR] OPML
//	harb poll-once [-data DIR]
//	harb hashpass PASSWORD
//	harb passwd [-data DIR] [-password NEW]
//	harb update [-check] [-version vX.Y.Z]
//	harb version
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kfet/harb"
	"github.com/kfet/harb/internal/accesslog"
	"github.com/kfet/harb/internal/auth"
	"github.com/kfet/harb/internal/config"
	"github.com/kfet/harb/internal/feedpreview"
	"github.com/kfet/harb/internal/passkey"
	"github.com/kfet/harb/internal/poll"
	"github.com/kfet/harb/internal/poll/observe"
	"github.com/kfet/harb/internal/reader"
	"github.com/kfet/harb/internal/selfupdate"
	"github.com/kfet/harb/internal/store"
	uipkg "github.com/kfet/harb/internal/ui"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "\nfirst time? run `harb init` to bootstrap a config.")
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "harb %s (commit %s, built %s)\n", harb.Version, harb.Commit, harb.BuildDate)
		return 0
	case "init":
		return cmdInit(rest, stdout, stderr)
	case "serve":
		return cmdServe(rest, stdout, stderr)
	case "import":
		return cmdImport(rest, stdout, stderr)
	case "poll-once":
		return cmdPollOnce(rest, stdout, stderr)
	case "migrate":
		return cmdMigrate(rest, stdout, stderr)
	case "hashpass":
		return cmdHashpass(rest, stdout, stderr)
	case "passwd":
		return cmdPasswd(rest, stdout, stderr)
	case "update":
		return cmdUpdate(rest, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n%s\n", cmd, usage)
		return 2
	}
}

const usage = `harb — single-binary RSS server

usage:
  harb init      [-data DIR] [-username NAME] [-password PASS]
                     [-listen ADDR] [-theme NAME] [-force]
  harb serve     [-data DIR] [-config FILE]
  harb import    [-data DIR] OPMLFILE
  harb poll-once [-data DIR]
  harb migrate   --identity [--dry-run] [-data DIR]
  harb hashpass  PASSWORD
  harb passwd    [-data DIR] [-password NEW]
  harb update    [-check] [-version vX.Y.Z]
  harb version

bootstrap:
  harb init                  # writes <data>/config.json, prints a generated password
  harb serve                 # start serving on the configured address

data dir defaults to $HARB_DATA, then $XDG_DATA_HOME/harb,
then ~/.local/share/harb.`

func defaultDataDir() string {
	if v := os.Getenv("HARB_DATA"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "harb")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./harb-data"
	}
	return filepath.Join(home, ".local", "share", "harb")
}

// commonFlags parses -data + -config and returns the resolved values.
func commonFlags(fs *flag.FlagSet) (*string, *string) {
	data := fs.String("data", defaultDataDir(), "data directory")
	cfg := fs.String("config", "", "path to config.json (defaults to <data>/config.json)")
	return data, cfg
}

// installRootRedirects wires the two root-level redirects on mux:
//
//   - GET / → 303 to "ui/" (relative, so it works under any URL prefix).
//   - GET /ui → 301 to "ui/" (relative; pre-empts http.ServeMux's
//     auto-canonicalisation to /ui/, which would emit an absolute-path
//     Location and break the prefix-agnostic UI under e.g. Tailscale
//     Funnel --set-path=/rss).
//
// Both Location headers are verbatim relative references — never
// leading-slash absolute. ui.RelRedirect enforces this with a panic.
func installRootRedirects(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			uipkg.RelRedirect(w, r, "ui/", http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		uipkg.RelRedirect(w, r, "ui/", http.StatusMovedPermanently)
	})
}

func cmdServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataPtr, cfgPtr := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	data := *dataPtr
	cfgPath := *cfgPtr
	if cfgPath == "" {
		cfgPath = filepath.Join(data, "config.json")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return 1
	}
	if cfg.Auth.PasswordHash == "" {
		fmt.Fprintf(stderr, "no auth.password_hash in %s\n", cfgPath)
		fmt.Fprintln(stderr, "run `harb init -data", data, "` to bootstrap, or set it manually with `harb hashpass <password>`.")
		return 1
	}
	st, err := store.Open(data)
	if err != nil {
		fmt.Fprintln(stderr, "store:", err)
		return 1
	}
	as, err := auth.OpenStore(filepath.Join(data, "tokens.json"), cfg.Auth)
	if err != nil {
		fmt.Fprintln(stderr, "auth:", err)
		return 1
	}
	op := config.NewFileOPML(data)
	mux := http.NewServeMux()
	linkRules := cfg.EffectiveLinkRewrite()
	readSrv := reader.New(st, as, op)
	readSrv.Version = harb.Version
	readSrv.Commit = harb.Commit
	readSrv.BuildDate = harb.BuildDate
	readSrv.LinkRewrite = linkRules
	readHandler := readSrv.Routes(mux)
	uiSrv, err := uipkg.New(st, as, op, cfg.UI.Theme, data)
	if err != nil {
		fmt.Fprintln(stderr, "ui:", err)
		return 1
	}
	uiSrv.Secure = cfg.UI.Secure
	uiSrv.LinkRewrite = linkRules
	uiSrv.StaticVer = harb.Commit
	uiSrv.Version = harb.Version
	uiSrv.ConfigPath = cfgPath
	uiSrv.Previewer = feedpreview.New(data)
	// Passkey (WebAuthn) login, enabled only when configured with an RP
	// ID + origin. Credentials live alongside tokens.json in the data dir.
	if cfg.WebAuthn.Enabled() {
		creds, err := passkey.OpenCredentialStore(filepath.Join(data, "credentials.json"))
		if err != nil {
			fmt.Fprintln(stderr, "passkey:", err)
			return 1
		}
		uiSrv.Passkey = passkey.New(cfg.WebAuthn, cfg.Auth.Username, creds)
	}
	uiSrv.Routes(mux)
	// Root + /ui redirects. Extracted so the test in main_test.go can
	// exercise the exact handler wiring used at runtime.
	installRootRedirects(mux)

	// Optional access log. Off by default; opt-in via
	// HARB_ACCESS_LOG=1 in the unit-file environment. Output goes
	// to stderr so systemd journal picks it up. Redaction contract
	// lives in internal/accesslog: Authorization/Cookie headers never
	// read, bodies never inspected, query allow-listed.
	accessLogOn := accesslog.EnabledFromEnv()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	poller := poll.New(st)
	poller.UserAgent = "harb/" + harb.Version
	// Record every poll outcome under <data-dir>/observe so an
	// out-of-process fixer can diagnose breakage and write resolver
	// sidecars. Pure observability — harb reacts to nothing here.
	poller.Observer = observe.NewDiskObserver(st.Dir)
	feeds := func() []string {
		o, err := op.Load()
		if err != nil {
			return nil
		}
		urls := make([]string, 0, len(o.Feeds))
		for _, f := range o.Feeds {
			urls = append(urls, f.XMLURL)
		}
		return urls
	}
	refresher := poll.NewRefresher(poller, feeds)
	// API/UI requests trigger a refresh fire-and-forget; the response
	// itself serves whatever is in the store right now. Refreshed
	// entries land in time for the next sync. Pair with the background
	// ticker so an idle process still polls.
	triggered := poll.TriggerMiddleware(refresher, readHandler,
		"/reader/api/0/", "/ui/")
	handler := accesslog.New(triggered, accessLogOn, stderr)
	refresher.Start(ctx, 0)
	defer refresher.Stop()

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if accessLogOn {
			// Print the access-log enabled banner to the same stream
			// the access log itself uses (stderr), so a human tailing
			// the journal sees both together.
			fmt.Fprintln(stderr, "harb access log enabled (HARB_ACCESS_LOG=1)")
		}
		fmt.Fprintf(stdout, "harb listening on %s\n", listenURL(cfg.Listen))
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdown)
		return 0
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(stderr, "serve:", err)
			return 1
		}
		return 0
	}
}

func cmdImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataPtr, _ := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: harb import [-data DIR] OPMLFILE")
		return 2
	}
	src := fs.Arg(0)
	data := *dataPtr
	inc, err := store.ReadOPML(src)
	if err != nil {
		fmt.Fprintln(stderr, "read:", err)
		return 1
	}
	op := config.NewFileOPML(data)
	cur, err := op.Load()
	if err != nil {
		fmt.Fprintln(stderr, "load:", err)
		return 1
	}
	added := 0
	for _, f := range inc.Feeds {
		if cur.Add(f) {
			added++
		}
	}
	if err := op.Save(cur); err != nil {
		fmt.Fprintln(stderr, "save:", err)
		return 1
	}
	fmt.Fprintf(stdout, "imported %d new feed(s), %d total\n", added, len(cur.Feeds))
	return 0
}

func cmdPollOnce(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("poll-once", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataPtr, _ := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	data := *dataPtr
	st, err := store.Open(data)
	if err != nil {
		fmt.Fprintln(stderr, "store:", err)
		return 1
	}
	op := config.NewFileOPML(data)
	o, err := op.Load()
	if err != nil {
		fmt.Fprintln(stderr, "opml:", err)
		return 1
	}
	p := poll.New(st)
	p.UserAgent = "harb/" + harb.Version
	p.Observer = observe.NewDiskObserver(st.Dir)
	total := 0
	for _, f := range o.Feeds {
		// poll-once must force a poll even if the feed is in a
		// 429/503 cooldown — clear RetryAfter before each attempt.
		_ = p.ResetCooldown(f.XMLURL)
		n, err := p.Poll(context.Background(), f.XMLURL)
		if err != nil {
			fmt.Fprintf(stderr, "poll %s: %v\n", f.XMLURL, err)
			continue
		}
		total += n
		fmt.Fprintf(stdout, "%s: %d new\n", f.XMLURL, n)
	}
	fmt.Fprintf(stdout, "total new entries: %d\n", total)
	return 0
}

// cmdMigrate runs the one-time entry-identity migration (sticky reuse-set
// model). --dry-run computes and prints the full human-reviewable report —
// every collapse group, every sticky marking, and an old→new hash map artifact
// — WITHOUT touching any data. Without --dry-run it performs the real,
// version-gated migration (idempotent: a second run is a no-op).
func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataPtr, _ := commonFlags(fs)
	identity := fs.Bool("identity", false, "run the entry-identity (sticky reuse-set) migration")
	dryRun := fs.Bool("dry-run", false, "compute and print the report without modifying data")
	mapPath := fs.String("map-out", "", "also write the old→new hash map JSON to this path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*identity {
		fmt.Fprintln(stderr, "migrate: pass --identity to select the identity migration")
		return 2
	}
	data := *dataPtr
	rep, err := store.MigrateIdentity(data, *dryRun)
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return 1
	}
	if err := printMigrationReport(stdout, rep); err != nil {
		fmt.Fprintln(stderr, "migrate: report:", err)
		return 1
	}
	if *mapPath != "" {
		data, err := json.MarshalIndent(rep.HashRemap, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, "migrate: map:", err)
			return 1
		}
		if err := os.WriteFile(*mapPath, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintln(stderr, "migrate: map:", err)
			return 1
		}
		fmt.Fprintf(stdout, "\nold→new hash map written to %s (%d entries)\n", *mapPath, len(rep.HashRemap))
	}
	return 0
}

// printMigrationReport renders the migration report as a full JSON document
// (stable, machine- and human-reviewable) plus a one-line human summary.
func printMigrationReport(stdout io.Writer, rep *store.IdentityReport) error {
	if rep.Skipped {
		fmt.Fprintf(stdout, "identity migration already applied (version %s) — no-op\n", rep.Version)
		return nil
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return err
	}
	mode := "APPLIED"
	if rep.DryRun {
		mode = "DRY-RUN (no data modified)"
	}
	t := rep.Totals
	fmt.Fprintf(stdout,
		"\n%s: %d entries → %d survivors (%d intended collapses), %d sticky markings; "+
			"read=%d→%d starred=%d→%d\n",
		mode, t.OldHashCount, t.SurvivorCount, t.IntendedCollapses, t.StickyMarkings,
		t.ReadBefore, t.ReadAfter, t.StarBefore, t.StarAfter)
	return nil
}

// genPassword returns a 16-char URL-safe random password.
func genPassword() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", defaultDataDir(), "data directory")
	user := fs.String("username", "admin", "login username")
	pass := fs.String("password", "", "login password (generated if empty)")
	listen := fs.String("listen", ":8088", "listen address")
	theme := fs.String("theme", "auto", "ui theme: auto|light|dark|sepia")
	force := fs.Bool("force", false, "overwrite an existing config")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	cfgPath := filepath.Join(*data, "config.json")
	if _, err := os.Stat(cfgPath); err == nil && !*force {
		fmt.Fprintf(stderr, "config already exists: %s\nuse -force to overwrite, or edit it directly.\n", cfgPath)
		return 1
	}
	generated := false
	if *pass == "" {
		p, err := genPassword()
		if err != nil {
			fmt.Fprintln(stderr, "init:", err)
			return 1
		}
		*pass = p
		generated = true
	}
	h, err := auth.HashPassword(*pass)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	cfg := config.Default()
	cfg.Listen = *listen
	cfg.UI.Theme = *theme
	cfg.Auth.Username = *user
	cfg.Auth.PasswordHash = h
	if err := config.Save(cfgPath, cfg); err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	passwordLine := "  password:   (as supplied via -password)"
	if generated {
		passwordLine = fmt.Sprintf("  password:   %s  (generated — save it now, it won't be shown again)", *pass)
	}
	fmt.Fprintf(stdout, `✓ harb initialised
  data dir:   %s
  config:     %s
  listen:     %s
  username:   %s
%s

next steps:
  harb import <your.opml>   # optional: import existing subscriptions
  harb serve                # start the server
then point a FreshRSS-compatible client (or a browser) at http://localhost%s/
`, *data, cfgPath, *listen, *user, passwordLine, *listen)
	return 0
}

func cmdHashpass(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: harb hashpass PASSWORD")
		return 2
	}
	h, err := auth.HashPassword(args[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, h)
	return 0
}

func cmdUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "report whether an update is available, do not install")
	ver := fs.String("version", "", "install a specific version (e.g. v0.2.0); default: latest")
	repo := fs.String("repo", selfupdate.DefaultRepo, "github owner/repo to update from")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	err := selfupdate.Run(harb.Version, selfupdate.Options{
		Repo:      *repo,
		Version:   *ver,
		CheckOnly: *check,
		Stdout:    stdout,
		Stderr:    stderr,
	})
	if err != nil {
		fmt.Fprintln(stderr, "update:", err)
		return 1
	}
	return 0
}

// cmdPasswd changes the configured single-user password. It rewrites
// only auth.password_hash in <data>/config.json (preserving every other
// field) and prints a confirmation. The hot harb process (if any
// is running) won't pick up the change until restart — there is no
// signal-reload mechanism in v0.2.
//
// New password sources (first non-empty wins):
//
//	-password NEW         flag
//	stdin (when piped)    one line, trailing newline stripped
//	tty prompt            "new password: " — echoed; we don't link
//	                      term-state libs from outside the stdlib.
func cmdPasswd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("passwd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", defaultDataDir(), "data directory")
	pass := fs.String("password", "", "new password (otherwise read from stdin)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfgPath := filepath.Join(*data, "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "passwd: load config:", err)
		return 1
	}
	if cfg.Auth.PasswordHash == "" {
		fmt.Fprintln(stderr, "passwd: no existing password to replace — run `harb init` first.")
		return 1
	}
	newp := *pass
	if newp == "" {
		newp, err = readPasswordFromStdin(stdout)
		if err != nil {
			fmt.Fprintln(stderr, "passwd:", err)
			return 1
		}
	}
	if len(newp) < 8 {
		fmt.Fprintln(stderr, "passwd: new password must be at least 8 characters")
		return 1
	}
	h, err := auth.HashPassword(newp)
	if err != nil {
		fmt.Fprintln(stderr, "passwd:", err)
		return 1
	}
	cfg.Auth.PasswordHash = h
	if err := config.Save(cfgPath, cfg); err != nil {
		fmt.Fprintln(stderr, "passwd: save config:", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ password updated in %s\n", cfgPath)
	fmt.Fprintln(stdout, "  restart any running `harb serve` to pick up the change.")
	return 0
}

// readPasswordFromStdin reads a single line from os.Stdin. If stdin is
// a terminal we prompt to stdout first. Either way, the input is
// echoed — adding non-echo without dragging in golang.org/x/term needs
// the aside-advisor escalation per AGENTS.md.
var readPasswordFromStdin = func(stdout io.Writer) (string, error) {
	st, _ := os.Stdin.Stat()
	if st != nil && st.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(stdout, "new password: ")
	}
	buf := make([]byte, 0, 128)
	one := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				break
			}
			if one[0] != '\r' {
				buf = append(buf, one[0])
			}
		}
		if err != nil {
			if len(buf) == 0 {
				return "", fmt.Errorf("read password: %w", err)
			}
			break
		}
		if len(buf) > 1024 {
			return "", fmt.Errorf("password too long")
		}
	}
	return string(buf), nil
}

// listenURL turns a Go-style listen address ("[host]:port") into a
// clickable http://… URL. Terminals (Terminal.app, iTerm2, gnome-
// terminal, VS Code) auto-link bare http URLs but not "host:port"
// strings; printing the full scheme + path means cmd-click works.
//
// Rules:
//
//	:8088, 0.0.0.0:8088, [::]:8088  → http://localhost:8088/
//	127.0.0.1:8088                   → http://127.0.0.1:8088/
//	example.com:8088                 → http://example.com:8088/
//
// If parsing fails for any reason we return the raw addr so the user
// still sees something useful.
func listenURL(addr string) string {
	host, port, ok := splitHostPort(addr)
	if !ok {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%s/", host, port)
}

// splitHostPort handles plain ":8088" (which net.SplitHostPort accepts)
// and IPv6 in brackets. Returns ok=false on malformed input.
func splitHostPort(addr string) (host, port string, ok bool) {
	if addr == "" {
		return "", "", false
	}
	// Strip an optional leading "[host]" form.
	if addr[0] == ':' {
		return "", addr[1:], len(addr) > 1
	}
	// Find the last ':' that isn't inside brackets.
	last := -1
	depth := 0
	for i, c := range addr {
		switch c {
		case '[':
			depth++
		case ']':
			depth--
		case ':':
			if depth == 0 {
				last = i
			}
		}
	}
	if last < 0 || last == len(addr)-1 {
		return "", "", false
	}
	host = addr[:last]
	port = addr[last+1:]
	host = trimBrackets(host)
	return host, port, true
}

func trimBrackets(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}
