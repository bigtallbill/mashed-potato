# mashed-potato

A small, single-binary backup manager for `nix-potato`: scheduled [restic](https://restic.net)
backups to the NAS, driven by systemd timers, with a tray icon + web UI for visibility.

The daemon/CLI binary is `mashed-potatod`.

## Status

**Milestone 1 — CLI core (current).** Loads config, runs a restic backup job, and
records each run to SQLite. No web UI / tray / timer generation yet.

Roadmap:
- **M1** CLI core: config + `run <job>` + SQLite history ✅
- **M2** Embedded web UI (HTMX + SSE): dashboard, history, live progress, "Run now" ✅
- **M2.5** App-owned config: create/edit/delete jobs, browse + pick directories, all from the UI ✅
- **M3** systemd user-timer management (`enable`/`disable`/`timers`) ✅ + retention (auto `forget --prune` after each backup, `prune` command) ✅
- **M4** system-tray icon (Open dashboard / Run ▸ job / Quit) ✅
- **M5** Nix packaging: flake + standalone `nix/` files for channels configs ✅

All core milestones complete.

## Design

One binary, several modes — systemd owns the *schedule*, the binary owns the *work*:

| Command | Purpose |
|---|---|
| `mashed-potatod serve` | start the web UI (localhost only, default `127.0.0.1:8765`) |
| `mashed-potatod run <job>` | execute one backup (also what timers will invoke) |
| `mashed-potatod list` | list configured jobs |
| `mashed-potatod history [job]` | recent run history |
| `mashed-potatod init-repo` | initialize the restic repository |
| `mashed-potatod prune <job>` | apply the job's retention policy now (`forget --prune`) |
| `mashed-potatod snapshots [job]` | list repo snapshots (optionally one job's) |
| `mashed-potatod mount` | FUSE-mount the repo to browse/restore files (Ctrl-C to unmount) |
| `mashed-potatod unlock` | remove stale repo locks left by a killed process |
| `mashed-potatod enable <job>` | install + start the job's systemd **user** timer |
| `mashed-potatod disable <job>` | stop + remove the job's timer |
| `mashed-potatod timers` | show installed mashed-potato timers |

### Config ownership

The app **owns** `config.toml`: creating/editing/deleting jobs and adding directories
(from the web UI or a future CLI) rewrites the file. Hand edits still work, but
comments may be lost on the next app write. A job may temporarily have **no
directories** (you add them after creating it); that's only an error at run time.

### Run statuses & errors

Runs are recorded with one of three statuses:
- **success** — clean backup.
- **partial** — restic exit code 3: a snapshot **was** created, but some source files
  couldn't be read (e.g. permission-denied on a container-owned dir). Not a hard failure;
  `run` exits 0 so a timer doesn't flag it.
- **failed** — restic failed and no usable snapshot resulted.

Error text is captured (restic's JSON errors are de-JSON'd and deduplicated) and shown
in the dashboard (a collapsible "error detail" row under the run) and in
`mashed-potatod history` (printed below the table).

### Browsing & restoring snapshots

- The **job editor** shows a read-only **Snapshots** list (id / time / size / files /
  paths) for that job — one `restic snapshots` query, loaded on open.
- To browse *files* and restore, **mount** the repo: the tray's **Browse snapshots**
  item (or `mashed-potatod mount`) FUSE-mounts the whole repo and opens it in your file
  manager — every snapshot appears as a folder; copy files out to restore. The tray
  mount is unmounted automatically when you quit; `mount` unmounts on Ctrl-C.
- One-off file restore without mounting: `restic dump <snapshot> /path/to/file > file`.

### Retention

Each backup is auto-tagged `mashed-potato-job:<name>`, and after a successful backup
the app runs `restic forget --prune` **scoped to that tag**, honoring the job's
`keep_last`/`keep_hourly`/`keep_daily`/`keep_weekly`/`keep_monthly` (any set to 0 is
omitted). With no keep-* values, nothing is pruned. For sub-daily schedules (hourly /
every-30-min), use `keep_last` or `keep_hourly` to retain intra-day snapshots —
otherwise the daily rule collapses each day to a single snapshot. Run it manually
anytime with `mashed-potatod prune <job>`.
A prune failure never fails the backup itself — it's recorded in the run's error note.

### Scheduling on NixOS

System units under `/etc` are read-only/Nix-owned, so mashed-potato manages its own
**systemd user timers** in `~/.config/systemd/user/` instead (`mashed-potato-<job>.{service,timer}`).
Timers are session-scoped (no linger) but use `Persistent=true`, so a run missed
while logged out is caught up at next login. The unit's `ExecStart` points at the
resolved binary path — install via `nix profile`/home-manager for a stable path.
Set `restic_bin` in `[restic]` if restic isn't on the unit's PATH (the Nix-wrapped
binary already bundles it).

### Web UI

`mashed-potatod serve` exposes a dashboard at `http://127.0.0.1:8765` (override with
`--addr`). It is **server-rendered HTML driven by [HTMX]**: the server emits HTML
fragments and pushes updated fragments over Server-Sent Events, so backup progress
updates live with almost no client-side JavaScript. htmx and its SSE extension are
vendored under `internal/server/static/` and embedded into the binary — no CDN, works
offline. Bound to loopback only; no auth.

[HTMX]: https://htmx.org

### Tray icon

`mashed-potatod serve` also shows a system-tray icon by default (menu: a live
**status line**, **Open dashboard**, **Run ▸ <job>**, **Quit**). It uses
`fyne.io/systray`, whose Linux backend speaks the StatusNotifierItem D-Bus protocol
directly — **no cgo, no GTK/AppIndicator deps** — so the binary stays static. It needs
a session bus and an SNI-capable tray (KDE/most desktops). With no session bus
(headless/systemd context) the tray is **skipped automatically** and the web UI still
runs. Disable explicitly with `-tray=false` or `--no-tray`.

The status line, tooltip, and **icon color update live**: gold = idle, green =
running (`running: <job> NN%`), red = last run failed.

### Live status across processes

`serve` writes its address to `~/.config/mashed-potato/serve.addr`. A separate
`mashed-potatod run <job>` (e.g. a systemd-timer backup, or a manual CLI run)
best-effort POSTs `started`/`progress`/`done` to `serve`'s `POST /api/ingest`, so
**scheduled and CLI runs also stream live** to the dashboard and tray — not just the
"Run now" button. If `serve` isn't running, `run` silently skips the notification.

- **Jobs** live in `~/.config/mashed-potato/config.toml` (human-editable, see
  `config.example.toml`).
- **Run history + metrics** live in SQLite at `~/.config/mashed-potato/state.db`
  (pure-Go `modernc.org/sqlite`, no cgo — keeps the binary static and nix-friendly).
- **Engine** is restic over its SFTP backend, authenticating with a dedicated SSH key.

## Setup

1. **NAS SSH key** (needed for unattended timer runs — a 2am job can't type a password):

   ```sh
   ./scripts/setup-nas-key.sh
   ```

   Generates `~/.ssh/nas_backup`, installs it on the NAS, and adds a `nas-backup`
   host alias to `~/.ssh/config`. If the NAS reports the `btb` account has no
   writable home, fix that on the NAS side (see the script's output) and re-run.

2. **Build / run.** With the flake (restic + ssh are bundled onto the binary's PATH):

   ```sh
   nix build           # -> ./result/bin/mashed-potatod
   nix run . -- list    # run without installing
   nix develop          # dev shell with go, gopls, restic
   ```

   Or plain Go (needs `restic` on PATH yourself, e.g. `nix-shell -p restic`):

   ```sh
   go build -o mashed-potatod ./cmd/mashed-potatod
   ```

3. **Config + repo password:**

   ```sh
   mkdir -p ~/.config/mashed-potato
   cp config.example.toml ~/.config/mashed-potato/config.toml
   install -m600 /dev/null ~/.config/mashed-potato/repo-password
   printf '%s' 'your-passphrase' > ~/.config/mashed-potato/repo-password   # save it in your password manager!
   ```

4. **Initialize + first backup:**

   ```sh
   ./result/bin/mashed-potatod init-repo
   ./result/bin/mashed-potatod run documents
   ./result/bin/mashed-potatod history
   ```

## Run it from your NixOS config (no home-manager)

`nix/module.nix` is a standalone NixOS module defining a **user** service
(`systemd.user.services.mashed-potato`), so the tray + web UI run in your graphical
session. It works with both flakes and a channels-based `configuration.nix`.

**Channels-based config** (import the module by path):

```nix
# configuration.nix
{
  imports = [
    ./hardware-configuration.nix
    /home/bigtallbill/Documents/stystem-maintenance/mashed-potato/nix/module.nix
  ];

  services.mashed-potato.enable = true;   # options: package, addr, tray, extraArgs
}
```

Then `sudo nixos-rebuild switch`. The repo must exist at that path at build time
(its source is copied into the store). The module builds the package with your
channel's nixpkgs via `callPackage`, installs the `mashed-potatod` CLI system-wide,
and the service starts at your next graphical login
(`systemctl --user status mashed-potato`).

**Flake-based config** (if you ever migrate): the flake exposes
`nixosModules.default` — add this repo as an input and put it in your `modules` list.

Notes:
- The **serve service is declarative**, but per-job **backup schedules stay app-managed**
  (the Enable/Disable toggle writes `~/.config/systemd/user` timers at runtime). They
  coexist fine.
- KDE Plasma 6 (your DE) populates `graphical-session.target`, so the tray binding works.
  On a DE that doesn't, change the unit's `wantedBy` to `default.target` or ensure
  `systemctl --user import-environment` runs.
- For a **headless** box, set `services.mashed-potato.tray = false` (web UI only). A true
  no-login system service would need `systemd.services.*` + `loginctl enable-linger`.
