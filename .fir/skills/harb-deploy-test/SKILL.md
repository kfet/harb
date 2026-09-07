---
name: harb-deploy-test
description: Stage a harb release on the prod host (sea-racknerd) by selfupdating the on-disk binary and smoke-testing it OUT-OF-BAND on a scratch port + scratch data dir — WITHOUT restarting the live prod process. The verify-before-prod gate for harb-promote-prod.
---

# harb-deploy-test

Bring a published `harb` release onto the prod host's disk and prove the
new binary is good **before** it touches the running prod service. The
trick: `harb update` atomically replaces `~/.local/bin/harb` on disk,
but the running prod process keeps the OLD binary mapped in memory until
it is restarted. So you can selfupdate, validate the new binary on a
throwaway port against a throwaway data dir, and only then (via
`harb-promote-prod`) restart prod onto it. If the new binary is bad you
roll the disk file back and prod never noticed.

This is the gate; `harb-promote-prod` does the actual cutover.

## Host + layout (sea-racknerd, the prod host)

- Host:        `sea-racknerd` (Linux/x86_64, systemd **user** units).
  SSH alias `sea-racknerd` (Tailscale `sea-racknerd.tail77d32.ts.net`).
- Prod unit:   `harb.service` — `ExecStart=%h/.local/bin/harb serve`,
  `HARB_DATA=%h/.local/share/harb`, `HARB_ACCESS_LOG=1`, listens `:8088`.
- Funnel:      `https://sea-racknerd.tail77d32.ts.net/rss` → `127.0.0.1:8088`.
- Binary:      `~/.local/bin/harb` (selfupdate target).
- Backups:     `harb-backup.service` (event-driven rsync to `kopione`).
- Logs:        `journalctl --user -u harb.service`.

There is **no standing test unit** (the old `harborrs-test.service` on
`:8089` was a pre-rename relic and was removed). The "test instance" here
is an ephemeral `harb serve` you start by hand on `:8099` and tear down.

## Inputs

- **Target version** — default: the latest published `vX.Y.Z` release
  (what `harb update -check` reports as `latest`). Override only to pin.

## Recipe

1. **Confirm a release is published** for the target version (GitHub
   release with tarballs + checksums). From the repo:

   ```bash
   gh release view vX.Y.Z --json tagName,assets -q '{tag:.tagName,assets:[.assets[].name]}'
   ssh sea-racknerd '~/.local/bin/harb update -check; echo rc=$?'  # rc=3 → update available
   ```

2. **Back up the current binary, then selfupdate the on-disk binary.**
   The running prod process is untouched (still the old binary in RAM):

   ```bash
   ssh sea-racknerd '
     cp -a ~/.local/bin/harb ~/.local/bin/harb.bak-$(date -u +%Y%m%dT%H%M%SZ)
     ~/.local/bin/harb update            # checksum-verified, atomic replace
     ~/.local/bin/harb version           # = target, on disk'
   ```

3. **Smoke the NEW binary out-of-band** on a scratch port + scratch data
   dir (never the prod data dir, never the prod port):

   ```bash
   ssh sea-racknerd 'set -e
     D=$(mktemp -d)
     HARB_DATA=$D ~/.local/bin/harb init -listen 127.0.0.1:8099 >/dev/null 2>&1 || true
     HARB_DATA=$D ~/.local/bin/harb serve >/tmp/harb-smoke.log 2>&1 &
     PID=$!; sleep 2
     ~/.local/bin/harb version
     curl -sS -o /dev/null -w "/ui/login -> %{http_code}\n"   http://127.0.0.1:8099/ui/login
     curl -sS -o /dev/null -w "/ -> %{http_code} %{redirect_url}\n" http://127.0.0.1:8099/
     tail -3 /tmp/harb-smoke.log
     kill $PID 2>/dev/null; rm -rf "$D"'
   ```

   Expect `/ui/login` → `200`, `/` → `303` to `…/ui/`, and the listen
   line in the log. (Anything that needs an auth session — e.g.
   `/ui/version` — 303-redirects to login; that's expected.)

4. **Targeted regression checks** for the specific change shipping —
   e.g. for a UI/layout change, fetch the page that changed and grep for
   the expected markup; for a redirect fix, walk the redirect paths.

5. **If the binary is BAD, roll the disk file back now** (prod was never
   restarted, so this is a no-op for prod):

   ```bash
   ssh sea-racknerd 'cp -a ~/.local/bin/harb.bak-<ts> ~/.local/bin/harb && ~/.local/bin/harb version'
   ```

6. **Report**: target version, the smoke HTTP codes, what specifically
   was regression-checked, and a clear GO / NO-GO for `harb-promote-prod`.

## Notes

- **Prod is never restarted by this skill.** Only `harb-promote-prod`
  restarts `harb.service`. That separation is the whole point of the gate.
- The `~/.local/bin/harb.bak-<ts>` copies are the rollback artefacts —
  `harb-promote-prod` relies on them. Prune old ones occasionally; keep
  at least the last one or two.
- For a hotfix that isn't a published release, cross-build instead of
  selfupdating: `make build-linux-amd64` in the repo, `scp` the binary
  to `~/.local/bin/harb` on the host (after the same backup step), then
  run the step-3 smoke.
- Don't loop `harb poll-once` against a scratch instance — upstream feed
  servers would see double polls.
