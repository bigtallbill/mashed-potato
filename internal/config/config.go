// Package config loads and validates the mashed-potato job configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration, normally at
// ~/.config/mashed-potato/config.toml.
type Config struct {
	Restic Restic `toml:"restic"`
	Jobs   []Job  `toml:"job"`
}

// Restic holds the repository connection settings shared by every job.
type Restic struct {
	// Repository is a restic repo string, e.g.
	// "sftp:nas-backup:/srv/.../backups/restic-repo".
	Repository string `toml:"repository"`
	// PasswordFile points at a 0600 file containing the repo passphrase.
	PasswordFile string `toml:"password_file"`
	// ExtraArgs are passed to every restic invocation (rare; e.g. --limit-upload).
	ExtraArgs []string `toml:"extra_args"`
	// ResticBin optionally pins the restic binary (absolute path). Useful for
	// systemd units on non-Nix builds where restic may not be on the unit's PATH.
	// Empty => use "restic" from PATH (the Nix-wrapped binary bundles it).
	ResticBin string `toml:"restic_bin"`
}

// Job is a single backup definition.
type Job struct {
	Name     string   `toml:"name"`
	Paths    []string `toml:"paths"`
	Excludes []string `toml:"excludes"`
	Tags     []string `toml:"tags"`

	// Schedule is an OnCalendar-style string used to generate the systemd timer
	// (e.g. "daily", "*-*-* 02:00:00"). Unused until the systemd milestone.
	Schedule string `toml:"schedule"`

	// Retention policy, applied by `forget --prune`. Zero = that rule is unset.
	KeepLast    int `toml:"keep_last"`   // keep the N most recent snapshots
	KeepHourly  int `toml:"keep_hourly"` // keep the last snapshot of the last N hours
	KeepDaily   int `toml:"keep_daily"`
	KeepWeekly  int `toml:"keep_weekly"`
	KeepMonthly int `toml:"keep_monthly"`
}

// Load reads, expands, and validates the config at path.
func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Restic.PasswordFile = expandHome(c.Restic.PasswordFile)
	for i := range c.Jobs {
		for j := range c.Jobs[i].Paths {
			c.Jobs[i].Paths[j] = expandHome(c.Jobs[i].Paths[j])
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Restic.Repository) == "" {
		return fmt.Errorf("restic.repository is required")
	}
	if strings.TrimSpace(c.Restic.PasswordFile) == "" {
		return fmt.Errorf("restic.password_file is required")
	}
	seen := map[string]bool{}
	for _, j := range c.Jobs {
		if strings.TrimSpace(j.Name) == "" {
			return fmt.Errorf("a job is missing a name")
		}
		if seen[j.Name] {
			return fmt.Errorf("duplicate job name %q", j.Name)
		}
		seen[j.Name] = true
		// Note: a job with no paths is allowed here — the UI creates a job and
		// then adds directories. "no directories" is enforced at run time instead.
	}
	return nil
}

// Job returns the job with the given name, or false if none matches.
func (c *Config) Job(name string) (Job, bool) {
	for _, j := range c.Jobs {
		if j.Name == name {
			return j, true
		}
	}
	return Job{}, false
}

// UpsertJob replaces the job with the same name, or appends it if new.
func (c *Config) UpsertJob(j Job) {
	for i := range c.Jobs {
		if c.Jobs[i].Name == j.Name {
			c.Jobs[i] = j
			return
		}
	}
	c.Jobs = append(c.Jobs, j)
}

// RemoveJob deletes the named job, reporting whether it existed.
func (c *Config) RemoveJob(name string) bool {
	for i := range c.Jobs {
		if c.Jobs[i].Name == name {
			c.Jobs = append(c.Jobs[:i], c.Jobs[i+1:]...)
			return true
		}
	}
	return false
}

const savedHeader = "# Managed by mashed-potato. The web UI and CLI rewrite this file,\n" +
	"# so hand-added comments may be lost on the next save.\n\n"

// Save validates and atomically writes the config to path as TOML.
func Save(path string, c *Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := tmp.WriteString(savedHeader); err != nil {
		tmp.Close()
		return err
	}
	if err := toml.NewEncoder(tmp).Encode(c); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
