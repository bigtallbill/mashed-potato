# mashed-potato — developer guide

A small, single-binary backup manager: scheduled [restic](https://restic.net) backups
with an embedded web UI and a system-tray icon. Daemon/CLI binary: **`mashed-potatod`**.

This file is the orientation for anyone (human or AI) continuing development. Read it
before making changes — it captures decisions and non-obvious gotchas that aren't
visible from the code alone.

---

## What it is / philosophy

The app is a **control plane over restic**, not a sync engine. restic handles
dedup/encryption/incremental/snapshots; mashed-potato adds:
- a config + job model (which dirs, excludes, schedule, retention),
- scheduling via **systemd user timers**,
- a **web UI** (dashboard, job editor, live progress, snapshot list),
- a **tray icon** (status, run, browse, quit),
- run history + metrics in SQLite.

**Design rule:** don't reinvent what restic already does. Shell out to `restic` and
parse its `--json` output.

## Architecture

One Go binary, two run "modes":
- **`serve`** — long-running: web UI + HTTP API + SSE + tray. Started as a systemd
  *user* service (see Deployment). Runs backups in-process for "Run now"/tray.
- **`run <job>`** — one-shot: executes a single backup. This is what systemd **timers**
  invoke, and also the manual CLI path. It best-effort notifies a running `serve`
  (see "Cross-process live status") so scheduled runs stream to the UI/tray too.

`systemd owns the schedule; the binary owns the work.` Both the timer and the UI button
ultimately run the same backup code.

### Packages (`internal/`)
| Package | Responsibility |
|---|---|
| `config` | TOML config load/validate **and save** (the app owns `config.toml`). Job model. |
| `store` | SQLite run history (`runs` table). Pure-Go `modernc.org/sqlite` (no cgo). |
| `runner` | Executes `restic backup/forget/snapshots/unlock/mount`, parses `--json`. |
| `scheduler` | Renders + installs/removes `~/.config/systemd/user` timers (`systemctl --user`). |
| `server` | HTTP handlers, HTMX templates, SSE hub, live-run state, tray status feed, mount lifecycle. |
| `tray` | `fyne.io/systray` menu + status updates + icon. |

`cmd/mashed-potatod/main.go` is the CLI dispatch + the `serve` wiring.

### Data / state locations (under `$HOME`)
- `~/.config/mashed-potato/config.toml` — jobs + restic settings (app-owned, rewritten on UI edits).
- `~/.config/mashed-potato/repo-password` — restic passphrase (mode 0600).
- `~/.config/mashed-potato/state.db` — SQLite run history (WAL).
- `~/.config/mashed-potato/serve.addr` — written by `serve` so `run` can find it.
- `~/.config/systemd/user/mashed-potato-<job>.{service,timer}` — app-generated timers.
- `~/.cache/mashed-potato/mnt` — default `restic mount` mountpoint.

---

## Build / dev workflow

```sh
go build -o mashed-potatod ./cmd/mashed-potatod
go vet ./...
gofmt -w ./cmd ./internal

# Nix (flake):
nix build            # -> ./result/bin/mashed-potatod (restic + openssh wrapped onto PATH)
nix run . -- serve
nix develop          # shell with go, gopls, restic
```

- **Web assets (templates + static) are embedded** via `embed.FS`. You **must rebuild
  and restart** to see template/CSS/JS changes — editing the files on disk does nothing
  for a running binary.
- htmx + its SSE extension are **vendored** under `internal/server/static/` (no CDN).
- Go module path: `github.com/bigtallbill/mashed-potato`. `go.mod` pins **`go 1.23`**
  (deliberately below the toolchain so the default nixpkgs `go` builds it).

## Testing approach (no live NAS needed)

There's no formal test suite yet; features are verified with **fake binaries** and
bounded HTTP probes. Patterns used:
- **Fake `restic`**: a bash script that emits the `--json` messages the runner parses
  (status/summary/error/exit_error), passed via `--restic /path/to/fake`. Use it to test
  backup parsing, progress, partial (exit 3), retention `forget` args, snapshots.
- **Fake `systemctl`**: `MASHED_POTATO_SYSTEMCTL=/path/to/fake` + `MASHED_POTATO_UNIT_DIR=$TMP`
  let you test enable/disable/timer generation without touching the real user session.
- **Temp `HOME`** (`export HOME=$(mktemp -d)`) isolates config/state/units.
- **Probing `serve`**: start it on a throwaway port, poll with
  `curl --retry N --retry-delay 1 --retry-connrefused --max-time 2` (NOT a bare
  `--retry` loop — curl returns *instantly* on connection-refused and you'll lose the
  startup race). Bound any blocking command with `timeout`.

When adding a feature, add a fake-restic/fake-systemctl smoke test in the same style.

---

## Conventions & gotchas (read these)

- **Nix wrapProgram path trap.** `wrapProgram` makes `bin/mashed-potatod` a wrapper that
  sets PATH (restic+ssh) and execs `bin/.mashed-potatod-wrapped`. So `os.Executable()`
  returns the **inner** `.mashed-potatod-wrapped`. If a timer's `ExecStart` points there,
  it **bypasses the PATH wrapper → "restic: not found"**. `resolveBinary()` (in main.go)
  fixes this: it prefers `/run/current-system/sw/bin/mashed-potatod` (stable across
  rebuilds/GC), else un-wraps the `.X-wrapped` name. Don't regress this.
- **vendorHash.** `nix/package.nix` pins `vendorHash` (hash of the Go module set, nixpkgs-
  independent). If you change `go.mod`/`go.sum`, set it to
  `sha256-AAAA...AAAA=`, run `nix build`, and paste the "got:" hash back. Note: if the
  hash string is unchanged, Nix reuses the cached (stale) module set — always change it
  to force a refetch.
- **Flakes only see git-tracked files.** New files won't be picked up by `nix build`
  until `git add`ed.
- **restic exit code 3 = `partial`**, not failure: a snapshot was created but some files
  were unreadable. `run` exits 0 so timers don't flag it. Status values: `success` /
  `partial` / `failed`.
- **Retention scoping.** Every backup is auto-tagged `mashed-potato-job:<name>`
  (`runner.JobTag`). `forget --prune` filters on that tag so jobs in the shared repo
  don't prune each other. Keep fields: `keep_last`, `keep_hourly`, `keep_daily`,
  `keep_weekly`, `keep_monthly` (0 = rule off; all 0 = never prune).
- **Locks.** A killed restic leaves a stale lock. `Backup` runs `restic unlock` first
  (only removes dead-process locks, safe) and uses `--retry-lock 5m` on backup+forget
  (for concurrent same-minute timers). `mashed-potatod unlock` clears manually.
- **Web UI is localhost-only, no auth.** Same for `POST /api/ingest`. Fine for a single
  user; add a token if that ever changes.
- **Cross-process live status.** `serve` writes its addr to `serve.addr`; `run` POSTs
  `started`/`progress`/`done` to `serve`'s `/api/ingest`, which feeds the *same*
  `liveStart/liveProgress/liveDone` path as in-process runs → dashboard SSE + tray.
  Best-effort: if `serve` isn't up, `run` silently skips.
- **Tray needs a session bus.** `tray.Available()` guards it; headless/systemd contexts
  skip the tray and just serve the web UI. Linux backend is cgo-free (DBus SNI).
- **Schedules are app-managed (imperative).** The schedule toggle / `enable`/`disable`
  write user units at runtime. The *serve* service itself is declarative (NixOS module),
  but per-job timers are not. Editing/saving a job **regenerates its timer**.

## HTTP routes (server)
`GET /{$}` dashboard · `GET /job` editor · `POST /api/run` · `POST /api/job[/delete|/addpath|/delpath]`
· `POST /api/schedule` · `GET /api/browse` (dir picker, `hidden=1` shows dotfiles) ·
`GET /api/snapshots` · `GET /api/events` (SSE) · `POST /api/ingest` (from `run`) · `GET /static/`.

SSE: server pushes named events `jobs` / `history` carrying rendered HTML *fragments*;
htmx's `sse-swap` swaps them in. Progress is throttled to whole-percent changes.

## CLI
`serve · run <job> · list · history [job] · init-repo · prune <job> · snapshots [job] ·
mount · unlock · enable <job> · disable <job> · timers · version`.
Global flags: `--config`, `--restic`. `serve` adds `--addr`, `--tray`/`--no-tray`.

---

## Deployment (this machine: `nix-potato`)

- The user's NixOS config is **channels-based** (no system flake): they edit
  `/etc/nixos/configuration.nix`, `sudo nixos-rebuild switch`, and the
  `~/Documents/repos/nix-configs` repo mirrors `/etc/nixos` (via `update-switch.sh`;
  `restore-switch.sh` deploys repo → `/etc/nixos` + rebuild + commit/push).
- The flake exposes `nixosModules.default` (= `nix/module.nix`), imported **by absolute
  path** from `configuration.nix`:
  `imports = [ /home/bigtallbill/Documents/repos/mashed-potato/nix/module.nix ];`
  `services.mashed-potato.enable = true;`
  So the repo must stay at that path (its source is copied to the store on build).
- It runs as a **systemd user service** (`systemd.user.services.mashed-potato`,
  `wantedBy = graphical-session.target`) so the tray works and it can manage user timers.
  Options: `services.mashed-potato.{enable,package,addr,tray,extraArgs}`.
- **`nixos-rebuild switch` does NOT restart running user services.** After a rebuild,
  restart manually: `systemctl --user restart mashed-potato` (or re-login). This is
  expected NixOS behavior, not a bug.

## Backup target (NAS)
- restic repo: `sftp:nas-backup:/srv/dev-disk-by-uuid-5a47138d-2e34-446b-8c5f-c37849d737f8/backups/restic-repo`
- `nas-backup` is an `~/.ssh/config` alias (key `~/.ssh/nas_backup`) created by
  `scripts/setup-nas-key.sh`. restic's sftp backend invokes `ssh`, so key auth is
  required (it runs non-interactively — no password prompt).
- NAS quirk: the `btb` account historically had no home dir, so authorized_keys needed a
  home created on the NAS side once (OpenMediaVault).

## UI palette (for logo/theme work)
bg `#14110f` · panel `#1d1916` · accent/gold `#d8a657` · text `#ece3da` ·
ok/green `#5fa564` · fail/red `#d4623f`. Tray icon states: gold idle / green running / red error.

---

## Status & ideas for next

**Done:** CLI core, web UI (HTMX+SSE), app-owned config + dir browser + job CRUD,
systemd user timers, retention (incl. `keep_last`/`keep_hourly`), tray (status + browse),
cross-process live status, snapshot list + mount, Nix flake + module.

**Possible next:**
- `keep_yearly`; cap/prune the `runs` history table (currently append-only).
- Prune concurrency: same-minute timers contend on the exclusive prune lock
  (mitigated by `--retry-lock`, not eliminated) — consider staggering or a prune queue.
- Optional auth/token if ever exposed beyond loopback.
- A `keep_*`-aware "what would prune keep?" preview.
- home-manager module variant; or switch the NixOS import from a local path to a flake
  input / `fetchGit` of the GitHub repo.
