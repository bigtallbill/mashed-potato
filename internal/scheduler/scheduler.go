// Package scheduler manages per-job systemd *user* timers.
//
// On NixOS, system units under /etc are read-only and owned by Nix, but
// ~/.config/systemd/user is a normal writable directory, so mashed-potato manages
// its own user timers there imperatively (the web UI / CLI enable and disable them).
// Timers use Persistent=true, so a run missed while logged out is caught up at the
// next login — which is why session-only scheduling (no linger) is still usable.
package scheduler

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const unitPrefix = "mashed-potato-"

// Manager renders and (un)installs user units. Fields can be overridden via the
// MASHED_POTATO_UNIT_DIR / MASHED_POTATO_SYSTEMCTL env vars (used by tests).
type Manager struct {
	UnitDir    string // default ~/.config/systemd/user
	Systemctl  string // default "systemctl"
	Binary     string // absolute path to mashed-potatod for ExecStart
	ConfigPath string // --config value embedded in units (may be "")
	ResticBin  string // --restic value embedded in units (may be "")
}

// New builds a Manager with sensible defaults and env overrides.
func New(binary, configPath, resticBin string) *Manager {
	unitDir := os.Getenv("MASHED_POTATO_UNIT_DIR")
	if unitDir == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			unitDir = filepath.Join(dir, "systemd", "user")
		}
	}
	sc := os.Getenv("MASHED_POTATO_SYSTEMCTL")
	if sc == "" {
		sc = "systemctl"
	}
	return &Manager{UnitDir: unitDir, Systemctl: sc, Binary: binary, ConfigPath: configPath, ResticBin: resticBin}
}

func (m *Manager) serviceName(job string) string { return unitPrefix + job + ".service" }
func (m *Manager) timerName(job string) string   { return unitPrefix + job + ".timer" }

// ServiceUnit returns the rendered .service file contents.
func (m *Manager) ServiceUnit(job string) string {
	exec := m.Binary + " run " + job
	if m.ConfigPath != "" {
		exec += " --config " + m.ConfigPath
	}
	if m.ResticBin != "" {
		exec += " --restic " + m.ResticBin
	}
	return strings.Join([]string{
		"[Unit]",
		"Description=mashed-potato backup: " + job,
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=oneshot",
		"ExecStart=" + exec,
		"",
	}, "\n")
}

// TimerUnit returns the rendered .timer file contents.
func (m *Manager) TimerUnit(job, schedule string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Schedule for mashed-potato backup: " + job,
		"",
		"[Timer]",
		"OnCalendar=" + schedule,
		"Persistent=true",
		"",
		"[Install]",
		"WantedBy=timers.target",
		"",
	}, "\n")
}

// Enable writes the units for job and enables+starts its timer.
func (m *Manager) Enable(job, schedule string) error {
	if strings.TrimSpace(schedule) == "" {
		return fmt.Errorf("job %q has no schedule to enable", job)
	}
	if err := os.MkdirAll(m.UnitDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.UnitDir, m.serviceName(job)), []byte(m.ServiceUnit(job)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.UnitDir, m.timerName(job)), []byte(m.TimerUnit(job, schedule)), 0o644); err != nil {
		return err
	}
	if err := m.run("daemon-reload"); err != nil {
		return err
	}
	return m.run("enable", "--now", m.timerName(job))
}

// Disable stops/disables the timer and removes the unit files.
func (m *Manager) Disable(job string) error {
	_ = m.run("disable", "--now", m.timerName(job)) // best-effort; may not be loaded
	var firstErr error
	for _, f := range []string{m.serviceName(job), m.timerName(job)} {
		if err := os.Remove(filepath.Join(m.UnitDir, f)); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	if err := m.run("daemon-reload"); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Installed returns the set of job names that currently have a timer unit.
func (m *Manager) Installed() (map[string]bool, error) {
	set := map[string]bool{}
	entries, err := os.ReadDir(m.UnitDir)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return set, err
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, unitPrefix) && strings.HasSuffix(n, ".timer") {
			set[strings.TrimSuffix(strings.TrimPrefix(n, unitPrefix), ".timer")] = true
		}
	}
	return set, nil
}

// IsInstalled reports whether the job has a timer unit (best-effort; false on error).
func (m *Manager) IsInstalled(job string) bool {
	set, _ := m.Installed()
	return set[job]
}

// ListTimers returns `systemctl --user list-timers` output for mashed-potato timers.
func (m *Manager) ListTimers() (string, error) {
	set, err := m.Installed()
	if err != nil {
		return "", err
	}
	if len(set) == 0 {
		return "(no mashed-potato timers installed)\n", nil
	}
	names := make([]string, 0, len(set))
	for job := range set {
		names = append(names, m.timerName(job))
	}
	sort.Strings(names)
	args := append([]string{"--user", "list-timers", "--all", "--no-pager"}, names...)
	out, err := exec.Command(m.Systemctl, args...).CombinedOutput()
	return string(out), err
}

func (m *Manager) run(args ...string) error {
	full := append([]string{"--user"}, args...)
	out, err := exec.Command(m.Systemctl, full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", m.Systemctl, strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
