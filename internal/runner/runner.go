// Package runner executes restic backups and parses their --json output.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bigtallbill/mashed-potato/internal/config"
	"github.com/bigtallbill/mashed-potato/internal/store"
)

// Runner executes restic for a given configuration.
type Runner struct {
	cfg     *config.Config
	resticB string // path to the restic binary
	// Progress, if set, is called for each restic "status" message (0..1 fraction).
	Progress func(percent float64, currentFiles []string)
	// Logf, if set, receives non-JSON restic stderr lines for surfacing/logging.
	Logf func(line string)
}

// New returns a Runner. resticBin may be "" to use "restic" from PATH.
func New(cfg *config.Config, resticBin string) *Runner {
	if resticBin == "" {
		resticBin = "restic"
	}
	return &Runner{cfg: cfg, resticB: resticBin}
}

// summaryMsg is restic's final --json message (message_type == "summary").
type summaryMsg struct {
	MessageType         string  `json:"message_type"`
	FilesNew            int64   `json:"files_new"`
	FilesChanged        int64   `json:"files_changed"`
	FilesUnmodified     int64   `json:"files_unmodified"`
	DataAdded           int64   `json:"data_added"`
	TotalBytesProcessed int64   `json:"total_bytes_processed"`
	TotalDuration       float64 `json:"total_duration"`
	SnapshotID          string  `json:"snapshot_id"`
}

type statusMsg struct {
	MessageType  string   `json:"message_type"`
	PercentDone  float64  `json:"percent_done"`
	CurrentFiles []string `json:"current_files"`
}

// Backup runs `restic backup` for job and returns a populated store.Run.
// The returned Run is filled even on failure (Status="failed", ErrMsg set).
func (r *Runner) Backup(ctx context.Context, job config.Job) (store.Run, error) {
	run := store.Run{Job: job.Name, StartedAt: time.Now(), Status: "failed"}

	if len(job.Paths) == 0 {
		run.FinishedAt = time.Now()
		run.ErrMsg = "no directories configured for this job"
		return run, fmt.Errorf("job %q has no directories to back up", job.Name)
	}

	// Clear stale locks left by a killed restic process (best-effort; restic
	// unlock only removes locks whose owning process is dead, so this is safe
	// even if another backup is genuinely running).
	_ = r.Unlock(ctx)

	args := []string{"backup", "--json", "--retry-lock", "5m"}
	args = append(args, r.cfg.Restic.ExtraArgs...)
	// Auto-tag every snapshot with the job name so retention (forget --prune) can
	// scope to just this job's snapshots in a shared repo.
	args = append(args, "--tag", jobTag(job.Name))
	for _, t := range job.Tags {
		args = append(args, "--tag", t)
	}
	for _, e := range job.Excludes {
		args = append(args, "--exclude", e)
	}
	args = append(args, job.Paths...)

	cmd := exec.CommandContext(ctx, r.resticB, args...)
	cmd.Env = r.env()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		run.FinishedAt = time.Now()
		run.ErrMsg = err.Error()
		return run, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		run.FinishedAt = time.Now()
		run.ErrMsg = err.Error()
		return run, err
	}

	if err := cmd.Start(); err != nil {
		run.FinishedAt = time.Now()
		run.ErrMsg = fmt.Sprintf("start restic: %v", err)
		return run, err
	}

	// Drain stderr concurrently so restic never blocks on a full pipe.
	stderrDone := make(chan []string, 1)
	go func() { stderrDone <- r.drainStderr(stderr) }()

	summary, stdoutErrs, parseErr := r.parseStdout(stdout)

	waitErr := cmd.Wait()
	stderrErrs := <-stderrDone
	run.FinishedAt = time.Now()

	if summary != nil {
		run.SnapshotID = summary.SnapshotID
		run.FilesNew = summary.FilesNew
		run.FilesChanged = summary.FilesChanged
		run.FilesUnmodified = summary.FilesUnmodified
		run.DataAddedBytes = summary.DataAdded
		run.BytesProcessed = summary.TotalBytesProcessed
	}

	errText := strings.Join(dedupeStrings(append(stdoutErrs, stderrErrs...)), "\n")

	if waitErr != nil {
		exitCode := -1
		if exit, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exit.ExitCode()
		}
		run.ExitCode = exitCode
		run.ErrMsg = errText
		if run.ErrMsg == "" {
			run.ErrMsg = waitErr.Error()
		}

		// restic exit code 3 = "backup completed, but some source files could not
		// be read". A snapshot WAS created, so treat it as partial, not failed, and
		// still apply retention.
		if exitCode == 3 && summary != nil {
			run.Status = "partial"
			r.pruneNote(ctx, job, &run)
			return run, nil
		}
		return run, fmt.Errorf("restic backup failed (exit %d)", exitCode)
	}
	if parseErr != nil {
		run.ErrMsg = parseErr.Error()
		return run, parseErr
	}

	run.Status = "success"
	r.pruneNote(ctx, job, &run)
	return run, nil
}

// pruneNote applies retention and records a non-fatal note if pruning fails.
func (r *Runner) pruneNote(ctx context.Context, job config.Job, run *store.Run) {
	if err := r.Prune(ctx, job); err != nil {
		note := "prune failed: " + err.Error()
		if run.ErrMsg != "" {
			run.ErrMsg += "\n" + note
		} else {
			run.ErrMsg = note
		}
		if r.Logf != nil {
			r.Logf(note)
		}
	}
}

// Prune runs `restic forget --prune` scoped to the job's auto-tag, honoring its
// keep-daily/weekly/monthly policy. It's a no-op when no retention is configured.
func (r *Runner) Prune(ctx context.Context, job config.Job) error {
	if !HasRetention(job) {
		return nil
	}
	args := []string{"forget", "--tag", jobTag(job.Name), "--prune", "--retry-lock", "5m"}
	if job.KeepLast > 0 {
		args = append(args, "--keep-last", strconv.Itoa(job.KeepLast))
	}
	if job.KeepHourly > 0 {
		args = append(args, "--keep-hourly", strconv.Itoa(job.KeepHourly))
	}
	if job.KeepDaily > 0 {
		args = append(args, "--keep-daily", strconv.Itoa(job.KeepDaily))
	}
	if job.KeepWeekly > 0 {
		args = append(args, "--keep-weekly", strconv.Itoa(job.KeepWeekly))
	}
	if job.KeepMonthly > 0 {
		args = append(args, "--keep-monthly", strconv.Itoa(job.KeepMonthly))
	}
	cmd := exec.CommandContext(ctx, r.resticB, args...)
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	if r.Logf != nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				r.Logf("forget: " + line)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HasRetention reports whether the job has any keep-* policy set.
func HasRetention(j config.Job) bool {
	return j.KeepLast > 0 || j.KeepHourly > 0 || j.KeepDaily > 0 || j.KeepWeekly > 0 || j.KeepMonthly > 0
}

func jobTag(name string) string { return "mashed-potato-job:" + name }

// JobTag returns the auto-tag applied to a job's snapshots.
func JobTag(name string) string { return jobTag(name) }

// Snapshot is a restic snapshot, as listed by `restic snapshots --json`.
type Snapshot struct {
	ID    string
	Time  time.Time
	Paths []string
	Tags  []string
	Size  int64 // total_bytes_processed from the snapshot summary (0 if unknown)
	Files int64
}

// Snapshots lists repo snapshots, newest first. If tag is non-empty it filters
// to that tag (e.g. JobTag(name)).
func (r *Runner) Snapshots(ctx context.Context, tag string) ([]Snapshot, error) {
	args := []string{"snapshots", "--json"}
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	cmd := exec.CommandContext(ctx, r.resticB, args...)
	cmd.Env = r.env()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	var raw []struct {
		ID      string    `json:"id"`
		ShortID string    `json:"short_id"`
		Time    time.Time `json:"time"`
		Paths   []string  `json:"paths"`
		Tags    []string  `json:"tags"`
		Summary *struct {
			TotalBytesProcessed int64 `json:"total_bytes_processed"`
			TotalFilesProcessed int64 `json:"total_files_processed"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	snaps := make([]Snapshot, 0, len(raw))
	for i := len(raw) - 1; i >= 0; i-- { // restic lists oldest-first; reverse
		s := raw[i]
		id := s.ShortID
		if id == "" {
			id = s.ID
		}
		sn := Snapshot{ID: id, Time: s.Time, Paths: s.Paths, Tags: s.Tags}
		if s.Summary != nil {
			sn.Size = s.Summary.TotalBytesProcessed
			sn.Files = s.Summary.TotalFilesProcessed
		}
		snaps = append(snaps, sn)
	}
	return snaps, nil
}

// Unlock removes stale repo locks (those whose owning process is dead). It does
// not remove locks held by live restic processes, so it's safe to call before a
// backup to self-heal locks left by a killed process.
func (r *Runner) Unlock(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, r.resticB, "unlock")
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	if r.Logf != nil && len(out) > 0 {
		r.Logf("unlock: " + strings.TrimSpace(string(out)))
	}
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// MountCommand builds (but does not start) a `restic mount` command for the repo.
// Caller manages its lifecycle (it runs until SIGINT/SIGTERM, then unmounts).
func (r *Runner) MountCommand(mountpoint string) *exec.Cmd {
	cmd := exec.Command(r.resticB, "mount", mountpoint)
	cmd.Env = r.env()
	return cmd
}

func (r *Runner) env() []string {
	return append(os.Environ(),
		"RESTIC_REPOSITORY="+r.cfg.Restic.Repository,
		"RESTIC_PASSWORD_FILE="+r.cfg.Restic.PasswordFile,
	)
}

func (r *Runner) parseStdout(stdout io.Reader) (*summaryMsg, []string, error) {
	var summary *summaryMsg
	var errs []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		switch probe.MessageType {
		case "summary":
			var s summaryMsg
			if json.Unmarshal(line, &s) == nil {
				summary = &s
			}
		case "status":
			if r.Progress != nil {
				var st statusMsg
				if json.Unmarshal(line, &st) == nil {
					r.Progress(st.PercentDone, st.CurrentFiles)
				}
			}
		case "error", "exit_error":
			if m := humanizeResticError(string(line)); m != "" {
				errs = append(errs, m)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return summary, errs, err
	}
	return summary, errs, nil
}

// drainStderr humanizes restic's stderr (which is JSON in --json mode), dedups,
// and keeps the last few distinct messages for the error summary.
func (r *Runner) drainStderr(stderr io.Reader) []string {
	seen := map[string]bool{}
	var msgs []string
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		if r.Logf != nil {
			r.Logf(raw)
		}
		m := humanizeResticError(raw)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		msgs = append(msgs, m)
		if len(msgs) > 20 {
			msgs = msgs[1:]
		}
	}
	return msgs
}

// humanizeResticError turns one restic output line into a readable message. JSON
// error/exit_error objects become their human text; status/summary JSON is dropped;
// anything else passes through trimmed.
func humanizeResticError(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if strings.HasPrefix(line, "{") {
		var m struct {
			MessageType string `json:"message_type"`
			Message     string `json:"message"`
			Error       struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &m) == nil {
			switch m.MessageType {
			case "error":
				return m.Error.Message
			case "exit_error":
				return m.Message
			case "status", "summary", "verbose_status":
				return ""
			}
		}
	}
	return line
}

// dedupeStrings preserves order and drops empties/duplicates.
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// EnsureRepo runs `restic cat config` and, if the repo is missing, `restic init`.
func (r *Runner) EnsureRepo(ctx context.Context) error {
	env := r.env()
	check := exec.CommandContext(ctx, r.resticB, "cat", "config")
	check.Env = env
	if check.Run() == nil {
		return nil // repo already exists
	}
	init := exec.CommandContext(ctx, r.resticB, "init")
	init.Env = env
	init.Stdout = os.Stderr
	init.Stderr = os.Stderr
	if err := init.Run(); err != nil {
		return fmt.Errorf("restic init: %w", err)
	}
	return nil
}
