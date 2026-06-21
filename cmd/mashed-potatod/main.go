// Command mashed-potatod is the mashed-potato backup daemon/CLI.
//
// Milestone 1 (current): CLI core — load config, run a restic backup job, and
// record its result to SQLite. Later milestones add the web UI, systray, and
// systemd-timer management.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"net/http"

	"github.com/bigtallbill/mashed-potato/internal/config"
	"github.com/bigtallbill/mashed-potato/internal/runner"
	"github.com/bigtallbill/mashed-potato/internal/scheduler"
	"github.com/bigtallbill/mashed-potato/internal/server"
	"github.com/bigtallbill/mashed-potato/internal/store"
	"github.com/bigtallbill/mashed-potato/internal/tray"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "run":
		err = cmdRun(args)
	case "list":
		err = cmdList(args)
	case "history":
		err = cmdHistory(args)
	case "init-repo":
		err = cmdInitRepo(args)
	case "prune":
		err = cmdPrune(args)
	case "unlock":
		err = cmdUnlock(args)
	case "mount":
		err = cmdMount(args)
	case "snapshots":
		err = cmdSnapshots(args)
	case "enable":
		err = cmdEnable(args)
	case "disable":
		err = cmdDisable(args)
	case "timers":
		err = cmdTimers(args)
	case "version", "-v", "--version":
		fmt.Println("mashed-potatod", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mashed-potatod — scheduled restic backups

usage:
  mashed-potatod serve            start the web UI (localhost only)
  mashed-potatod run <job>        run one backup job now
  mashed-potatod list             list configured jobs
  mashed-potatod history [job]    show recent run history
  mashed-potatod init-repo        initialize the restic repository
  mashed-potatod prune <job>      apply the job's retention policy now (forget --prune)
  mashed-potatod snapshots [job]  list repo snapshots (optionally just one job's)
  mashed-potatod mount            FUSE-mount the repo to browse/restore files (Ctrl-C to unmount)
  mashed-potatod unlock           remove stale repo locks (from killed processes)
  mashed-potatod enable <job>     install + start the systemd user timer for a job
  mashed-potatod disable <job>    stop + remove the systemd user timer for a job
  mashed-potatod timers           show installed mashed-potato timers
  mashed-potatod version

global flags (before the subcommand args):
  --config <path>   config file (default ~/.config/mashed-potato/config.toml)
  --restic <path>   restic binary (default: restic on PATH)
`)
}

// commonFlags builds a flag set with the shared --config/--restic flags.
func commonFlags(name string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "config file path")
	resticBin := fs.String("restic", "", "restic binary (default: from PATH)")
	return fs, cfgPath, resticBin
}

// normalizeNegations rewrites the conventional `--no-<name>` / `-no-<name>`
// spelling into `-<name>=false`, which Go's flag package understands.
func normalizeNegations(args []string, names ...string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		for _, n := range names {
			if a == "--no-"+n || a == "-no-"+n {
				out[i] = "-" + n + "=false"
			}
		}
	}
	return out
}

// parseArgs parses flags that may be interspersed with positional arguments
// (Go's flag package otherwise stops at the first positional), returning the
// positionals in order. This lets `run <job> --config X` work like `run --config X <job>`.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "mashed-potato", "config.toml")
	}
	return "config.toml"
}

func defaultStatePath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "mashed-potato", "state.db")
	}
	return "state.db"
}

func cmdServe(args []string) error {
	fs, cfgPath, resticBin := commonFlags("serve")
	addr := fs.String("addr", "127.0.0.1:8765", "listen address (localhost only by default)")
	wantTray := fs.Bool("tray", true, "show a system-tray icon (use -tray=false or --no-tray to disable)")
	if _, err := parseArgs(fs, normalizeNegations(args, "tray")); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(defaultStatePath())
	if err != nil {
		return err
	}
	defer st.Close()

	cfgAbs := absPath(*cfgPath)
	rb := resolveResticBin(*resticBin, cfg)
	srv, err := server.New(server.Options{
		Config:     cfg,
		ConfigPath: cfgAbs,
		Store:      st,
		ResticBin:  rb,
		Scheduler:  scheduler.New(resolveBinary(), cfgAbs, rb),
	})
	if err != nil {
		return err
	}

	// Advertise our address so a separate `run` process (e.g. a systemd timer)
	// can stream its progress to us. Best-effort; cleaned up on shutdown.
	writeServeAddr(*addr)
	defer removeServeAddr()

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	url := "http://" + *addr
	errc := make(chan error, 1)
	go func() {
		fmt.Printf("==> mashed-potato web UI on %s\n", url)
		if e := httpSrv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
			errc <- e
		}
	}()

	shutdown := func() error {
		srv.Unmount() // unmount the repo if we mounted it
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}

	// With a tray, systray.Run owns the main goroutine and SIGINT/quit ends it.
	if *wantTray && tray.Available() {
		go func() {
			select {
			case <-ctx.Done():
				tray.Stop()
			case e := <-errc:
				fmt.Fprintln(os.Stderr, "http error:", e)
				tray.Stop()
			}
		}()
		fmt.Println("==> tray icon active")
		tray.Run(tray.Options{
			URL:      url,
			Jobs:     srv.JobNames(),
			RunJob:   srv.TriggerJob,
			Browse:   srv.MountAndOpen,
			OnExit:   func() { _ = shutdown() },
			Register: srv.SetStatusFunc,
		})
		return nil
	}

	if *wantTray {
		fmt.Fprintln(os.Stderr, "note: no session bus found — running without a tray icon")
	}
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\nshutting down…")
		return shutdown()
	case e := <-errc:
		return e
	}
}

func cmdRun(args []string) error {
	fs, cfgPath, resticBin := commonFlags("run")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("run requires exactly one job name")
	}
	jobName := pos[0]

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	job, ok := cfg.Job(jobName)
	if !ok {
		return fmt.Errorf("no job named %q in %s", jobName, *cfgPath)
	}

	st, err := store.Open(defaultStatePath())
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// If a `serve` instance is running, stream lifecycle events to it so the
	// dashboard + tray show this (e.g. timer-triggered) run live. Best-effort.
	notifyAddr := readServeAddr()
	if notifyAddr != "" {
		notifyServe(notifyAddr, ingestEvent{Type: "started", Job: jobName})
	}

	r := runner.New(cfg, resolveResticBin(*resticBin, cfg))
	r.Logf = func(line string) { fmt.Fprintln(os.Stderr, "restic:", line) }
	lastPct := -1
	r.Progress = func(pct float64, files []string) {
		fmt.Fprintf(os.Stderr, "\r  %5.1f%%", pct*100)
		if notifyAddr != "" {
			if p := int(pct * 100); p != lastPct {
				lastPct = p
				cur := ""
				if len(files) > 0 {
					cur = files[0]
				}
				notifyServe(notifyAddr, ingestEvent{Type: "progress", Job: jobName, Percent: pct, Current: cur})
			}
		}
	}

	fmt.Printf("==> running job %q\n", jobName)
	result, runErr := r.Backup(ctx, job)
	fmt.Fprintln(os.Stderr) // end progress line

	if _, err := st.RecordRun(result); err != nil {
		fmt.Fprintln(os.Stderr, "warning: failed to record run:", err)
	}
	if notifyAddr != "" {
		notifyServe(notifyAddr, ingestEvent{Type: "done", Job: jobName, Status: result.Status})
	}

	if runErr != nil {
		return runErr
	}
	dur := result.FinishedAt.Sub(result.StartedAt).Round(time.Second)
	if result.Status == "partial" {
		fmt.Printf("⚠️  %s: partial — snapshot %s created, but some files were skipped in %s:\n",
			jobName, shortID(result.SnapshotID), dur)
		for _, line := range strings.Split(result.ErrMsg, "\n") {
			fmt.Printf("    %s\n", line)
		}
		return nil
	}
	fmt.Printf("✅ %s: snapshot %s — new=%d changed=%d unmodified=%d, added=%s in %s\n",
		jobName, shortID(result.SnapshotID), result.FilesNew, result.FilesChanged,
		result.FilesUnmodified, humanBytes(result.DataAddedBytes), dur)
	return nil
}

func cmdList(args []string) error {
	fs, cfgPath, _ := commonFlags("list")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tPATHS\tSCHEDULE\tTAGS")
	for _, j := range cfg.Jobs {
		fmt.Fprintf(w, "%s\t%d\t%s\t%v\n", j.Name, len(j.Paths), orDash(j.Schedule), j.Tags)
	}
	return w.Flush()
}

func cmdHistory(args []string) error {
	fs, _, _ := commonFlags("history")
	limit := fs.Int("n", 20, "max rows to show")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	job := ""
	if len(pos) > 0 {
		job = pos[0]
	}
	st, err := store.Open(defaultStatePath())
	if err != nil {
		return err
	}
	defer st.Close()
	runs, err := st.RecentRuns(job, *limit)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("(no runs recorded yet)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "STARTED\tJOB\tSTATUS\tSNAPSHOT\tADDED\tDURATION")
	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.StartedAt.Local().Format("2006-01-02 15:04"), r.Job, r.Status,
			orDash(shortID(r.SnapshotID)), humanBytes(r.DataAddedBytes),
			r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// Detail any errors below the table.
	for _, r := range runs {
		if r.ErrMsg == "" {
			continue
		}
		fmt.Printf("\n%s %s (%s):\n", r.StartedAt.Local().Format("2006-01-02 15:04"), r.Job, r.Status)
		for _, line := range strings.Split(r.ErrMsg, "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
	return nil
}

func cmdInitRepo(args []string) error {
	fs, cfgPath, resticBin := commonFlags("init-repo")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := runner.New(cfg, resolveResticBin(*resticBin, cfg))
	if err := r.EnsureRepo(ctx); err != nil {
		return err
	}
	fmt.Printf("✅ repository ready: %s\n", cfg.Restic.Repository)
	return nil
}

func cmdPrune(args []string) error {
	fs, cfgPath, resticBin := commonFlags("prune")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("prune requires exactly one job name")
	}
	name := pos[0]
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	job, ok := cfg.Job(name)
	if !ok {
		return fmt.Errorf("no job named %q", name)
	}
	if !runner.HasRetention(job) {
		return fmt.Errorf("job %q has no retention policy (set keep_daily/weekly/monthly)", name)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := runner.New(cfg, resolveResticBin(*resticBin, cfg))
	r.Logf = func(line string) { fmt.Fprintln(os.Stderr, line) }
	if err := r.Prune(ctx, job); err != nil {
		return err
	}
	fmt.Printf("✅ pruned %q\n", name)
	return nil
}

func cmdUnlock(args []string) error {
	fs, cfgPath, resticBin := commonFlags("unlock")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := runner.New(cfg, resolveResticBin(*resticBin, cfg))
	r.Logf = func(line string) { fmt.Fprintln(os.Stderr, line) }
	if err := r.Unlock(ctx); err != nil {
		return err
	}
	fmt.Println("✅ stale locks removed")
	return nil
}

func cmdMount(args []string) error {
	fs, cfgPath, resticBin := commonFlags("mount")
	mp := fs.String("mountpoint", defaultMountpoint(), "directory to mount the repo at")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*mp, 0o755); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := runner.New(cfg, resolveResticBin(*resticBin, cfg)).MountCommand(*mp)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Printf("==> repo mounted at %s — browse it, then press Ctrl-C to unmount\n", *mp)
	go func() {
		<-ctx.Done()
		_ = cmd.Process.Signal(syscall.SIGINT) // let restic unmount cleanly
	}()
	_ = cmd.Wait()
	fmt.Println("\nunmounted.")
	return nil
}

func cmdSnapshots(args []string) error {
	fs, cfgPath, resticBin := commonFlags("snapshots")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	tag := ""
	if len(pos) > 0 {
		tag = runner.JobTag(pos[0])
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	snaps, err := runner.New(cfg, resolveResticBin(*resticBin, cfg)).Snapshots(ctx, tag)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Println("(no snapshots)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SNAPSHOT\tWHEN\tSIZE\tFILES\tPATHS")
	for _, s := range snaps {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", s.ID, s.Time.Local().Format("2006-01-02 15:04"),
			humanBytes(s.Size), s.Files, strings.Join(s.Paths, ", "))
	}
	return w.Flush()
}

func defaultMountpoint() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "mashed-potato", "mnt")
	}
	return filepath.Join(os.TempDir(), "mashed-potato-mnt")
}

func cmdEnable(args []string) error {
	fs, cfgPath, resticBin := commonFlags("enable")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("enable requires exactly one job name")
	}
	name := pos[0]
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	job, ok := cfg.Job(name)
	if !ok {
		return fmt.Errorf("no job named %q", name)
	}
	if job.Schedule == "" {
		return fmt.Errorf("job %q has no schedule set (add one with `mashed-potatod` config or the web UI)", name)
	}
	sched := scheduler.New(resolveBinary(), absPath(*cfgPath), resolveResticBin(*resticBin, cfg))
	if err := sched.Enable(name, job.Schedule); err != nil {
		return err
	}
	fmt.Printf("✅ enabled timer for %q (OnCalendar=%s)\n", name, job.Schedule)
	return nil
}

func cmdDisable(args []string) error {
	fs, cfgPath, _ := commonFlags("disable")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("disable requires exactly one job name")
	}
	name := pos[0]
	sched := scheduler.New(resolveBinary(), absPath(*cfgPath), "")
	if err := sched.Disable(name); err != nil {
		return err
	}
	fmt.Printf("✅ disabled timer for %q\n", name)
	return nil
}

func cmdTimers(args []string) error {
	fs, cfgPath, _ := commonFlags("timers")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	sched := scheduler.New(resolveBinary(), absPath(*cfgPath), "")
	out, err := sched.ListTimers()
	fmt.Print(out)
	if err != nil && out == "" {
		return err
	}
	return nil
}

// ---- serve discovery + run→serve notification ----

type ingestEvent struct {
	Type    string  `json:"type"`
	Job     string  `json:"job"`
	Percent float64 `json:"percent,omitempty"`
	Current string  `json:"current,omitempty"`
	Status  string  `json:"status,omitempty"`
}

func serveAddrPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "mashed-potato", "serve.addr")
	}
	return ""
}

func writeServeAddr(addr string) {
	p := serveAddrPath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(addr), 0o644)
}

func removeServeAddr() {
	if p := serveAddrPath(); p != "" {
		_ = os.Remove(p)
	}
}

// readServeAddr returns the running serve's address, or "" if none is advertised.
func readServeAddr() string {
	p := serveAddrPath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// notifyServe posts an event to a running serve. Best-effort: short timeout,
// errors ignored (serve may not be running).
func notifyServe(addr string, ev ingestEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post("http://"+addr+"/api/ingest", "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}

// resolveBinary returns a stable path to this program for units' ExecStart.
//
// Under Nix, os.Executable() resolves to the wrapProgram *inner* binary
// (".mashed-potatod-wrapped"), which bypasses the PATH wrapper that puts restic
// and ssh on PATH — so a timer running it directly fails with "restic not found".
// Prefer the system-profile wrapper (stable across rebuilds and GC), then the
// sibling wrapper in the store, then the executable itself.
func resolveBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "mashed-potatod"
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	dir, base := filepath.Split(exe)
	if strings.HasPrefix(base, ".") && strings.HasSuffix(base, "-wrapped") {
		name := strings.TrimSuffix(strings.TrimPrefix(base, "."), "-wrapped")
		if p := filepath.Join("/run/current-system/sw/bin", name); fileExists(p) {
			return p // stable system-profile wrapper
		}
		if cand := filepath.Join(dir, name); fileExists(cand) {
			return cand // sibling wrapper in the same store path
		}
	}
	return exe
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func resolveResticBin(flagVal string, cfg *config.Config) string {
	if flagVal != "" {
		return flagVal
	}
	return cfg.Restic.ResticBin
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
