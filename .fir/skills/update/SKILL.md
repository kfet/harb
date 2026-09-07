---
name: update
description: Update harb on a single host to the latest released version and restart its supervisor (systemd or launchd) so the new binary is live. Verifies on a side-by-side test unit if one exists.
---

# Update Skill

Upgrade `harb` on **one** host (local or remote) and restart the
supervisor. Use after a release publishes or when a specific host is
stale.

> Releasing lives in `.fir/skills/release/SKILL.md` (the kfet stdlib-Go
> release flow). For multi-host rollouts, repeat this skill per host.

> **Canonical prod host:** `sea-racknerd` (Linux/x86_64, systemd **user**
> units). Prod = `harb.service` (ExecStart `~/.local/bin/harb serve`,
> `HARB_DATA=~/.local/share/harb`), funnel `/rss` → `127.0.0.1:8088`,
> backed up by `harb-backup.service`. The path that actually works there
> is selfupdate (`harb update`) + `systemctl --user restart harb` — see
> path (a). The dedicated `harb-deploy-test` / `harb-promote-prod`
> skills wrap that with a verify-before-prod gate.

## Inputs

Confirm with the user before acting:

1. **Host** — `local` or `user@host`. Default to local if omitted.
2. **Target version** — default: latest `vX.Y.Z` tag on `origin`.
   Override only if the user asks (e.g. pinning a known-good version).

## Steps

### 1. Determine target version

```bash
git fetch --tags origin
git tag --sort=-v:refname | head -1
```

If `VERSION` (the file at the repo root) is ahead of every pushed tag,
an unpublished release exists — stop and run the `release` skill first.

### 2. Probe the host

Detect installed version, install method, and supervisor. For remote
prefix every command with `ssh <host>`; for local run directly.

```bash
harb version 2>/dev/null || echo not-installed
brew list --versions harb 2>/dev/null         # brew install?
ls -l ~/.local/bin/harb /usr/local/bin/harb 2>/dev/null  # install.sh / hotfix?
systemctl --user is-active harb 2>/dev/null   # Linux supervisor
launchctl list 2>/dev/null | grep -i harb     # macOS supervisor
ls ~/Library/LaunchAgents/dev.*.harb.plist 2>/dev/null
```

Note the supervisor label exactly — it embeds the deploying user
(`dev.<user>.harb`). If installed version already equals target,
tell the user and stop unless they want a forced restart.

### 3. Pick the upgrade path

Pick the path that matches the **install method actually pinned by the
running supervisor's `ExecStart`**. Mixed installs are common; trust
the unit, not `which harb`.

**(a) `harb update` — selfupdate, the default path:**

```bash
ssh <host> 'harb update -check'        # prints current vs latest; exit 3 = update available
ssh <host> 'harb update'               # downloads + atomic-replaces the binary
```

Self-update is implemented by [distkit](https://github.com/kfet/distkit)
and writes to the same path as the running binary. Note the exit codes:
`-check` exits **3** when an update is available, 0 when up to date, 1 on
failure/refusal — a non-zero exit from `-check` is not necessarily an
error. A brew-managed install is upgraded via `brew upgrade` by `harb
update` itself; an install owned by another user is refused up front with
the command to use instead. After a successful update, restart the supervisor (the running
process still holds the old binary in memory):

```bash
# Linux
ssh <host> 'systemctl --user restart harb'

# macOS
ssh <host> "launchctl kickstart -k gui/\$UID/dev.<user>.harb"
```

If `harb update` fails because the binary lives in a path the user
can't write (e.g. `/usr/local/bin` on a locked-down box), fall back to
`install.sh` with `PREFIX=$HOME/.local` or to `brew upgrade`.

**(b) Brew (typical macOS):**

```bash
ssh <host> 'brew update && brew upgrade kfet/tap/harb'
ssh <host> "launchctl kickstart -k gui/\$UID/dev.<user>.harb"
```

If `brew upgrade` reports "already up-to-date" but the version still
lags, the tap index is stale — re-run `brew update`. Persistent miss
→ fall back to selfupdate.

**(c) install.sh re-run (Linux, hotfix):**

```bash
ssh <host> 'curl -fsSL https://raw.githubusercontent.com/kfet/harb/main/install.sh | sh'
ssh <host> 'systemctl --user restart harb'
```

**(d) Direct deploy from the dev tree (cross-build hotfix):**

```bash
make build-linux-arm64               # or matching host arch
scp harb-linux-arm64 <host>:~/.local/bin/harb
ssh <host> 'systemctl --user restart harb'
```

### 4. Optional: side-by-side verify on the test unit

If a `harb-test` unit (from the deploy skill) is still installed,
upgrade and restart **it first**, exercise it, then upgrade prod:

```bash
ssh <host> 'systemctl --user restart harb-test'
curl -sf https://<host>.<tailnet>.ts.net/rss-test/accounts/ClientLogin \
  -X POST -d 'Email=<user>&Passwd=<test-password>' | head
```

Only proceed to prod if the test unit comes back clean (`ClientLogin`
returns `SID=`/`Auth=`, browser flow lands on the login page under
the prefix). With the relative-URL UI (v0.3.1+), the prefix mount is
transparent — no special-casing required. This catches storage
incompatibilities that would otherwise corrupt prod data.

### 5. Verify prod

```bash
ssh <host> 'harb version'                            # equals target
ssh <host> 'systemctl --user is-active harb'         # → active   (Linux)
ssh <host> "launchctl print gui/\$UID/dev.<user>.harb | grep state"  # → state = running  (macOS)
```

If the host has a known public Funnel URL, optional smoke from your
workstation:

```bash
curl -sf -o /dev/null -w '%{http_code}\n' \
  https://<host>.<tailnet>.ts.net/
```

Expect `200`.

### 6. Report

One-line summary: `<host>: <old> → <new>, supervisor active`. If
anything failed, surface the error and stop — do not paper over.

## Pitfalls

- **Stale tap** — `brew upgrade` is a no-op until `brew update`
  refreshes the tap index.
- **Missed restart** — replacing the binary on disk does not reload
  the running process. Always restart the supervisor.
- **launchd label varies** — embeds the deploying user
  (`dev.<user>.harb`). Read it from
  `~/Library/LaunchAgents/dev.*.harb.plist`, don't guess.
- **Mixed install methods** — a host may have `~/.local/bin/harb`
  *and* a brew copy *and* `/usr/local/bin/harb`. The supervisor's
  `ExecStart` pins one. Upgrade whichever the unit/plist points at,
  not `command -v harb`.
- **`harb update` write permissions** — selfupdate writes to the
  binary's own path. If the unit ExecStarts `/usr/local/bin/harb`
  but the user can't write there, selfupdate fails silently from the
  user's POV (errors only on stdout). Switch to `brew upgrade` or
  re-run `install.sh` with a writable `PREFIX`.
- **Active client polling drops** — restart kills in-flight HTTP;
  RSS clients will simply retry on their next sync interval. Avoid
  during a manual sync if avoidable.
- **Storage compatibility** — harb uses on-disk NDJSON / append-
  logs. Major version bumps may add migration steps; check the
  CHANGELOG before jumping more than one minor version, and prefer the
  test-unit verify path (step 4) for those.

## Checklist

- [ ] Target version confirmed (latest pushed tag).
- [ ] Install method + supervisor identified on the host (from the
      unit's `ExecStart`, not `which`).
- [ ] If a test unit exists: upgraded + smoke-tested first.
- [ ] Binary upgraded via the matching path.
- [ ] Supervisor restarted.
- [ ] `harb version` matches target.
- [ ] Service active.
