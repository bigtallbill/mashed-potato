// Package server provides the embedded, localhost-only web UI for mashed-potato.
//
// The UI is server-rendered HTML driven by HTMX: the server emits HTML fragments
// and pushes updated fragments to the browser over Server-Sent Events (via the
// htmx SSE extension), so live backup progress needs essentially no bespoke JS.
// It also owns the config — jobs, their directories, excludes and schedule are
// edited through the UI and persisted back to config.toml.
package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bigtallbill/mashed-potato/internal/config"
	"github.com/bigtallbill/mashed-potato/internal/runner"
	"github.com/bigtallbill/mashed-potato/internal/scheduler"
	"github.com/bigtallbill/mashed-potato/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

// Options configures a Server.
type Options struct {
	Config     *config.Config
	ConfigPath string
	Store      *store.Store
	ResticBin  string
	Scheduler  *scheduler.Manager
}

// Server holds the web UI state and live-run tracking.
type Server struct {
	cfgPath   string
	store     *store.Store
	resticBin string
	sched     *scheduler.Manager
	tmpl      *template.Template
	hub       *sseHub

	cfgMu sync.RWMutex
	cfg   *config.Config

	mu         sync.Mutex
	live       map[string]*liveRun // job name -> status; present iff currently running
	lastJob    string
	lastStatus string

	statusMu sync.Mutex
	statusFn func(text, state string) // tray status listener (state: idle|running|error)

	mountMu  sync.Mutex
	mountCmd *exec.Cmd // running `restic mount`, nil if not mounted
	mountDir string
}

type liveRun struct {
	Percent     float64 // restic fraction 0..1
	Current     string
	StartedAt   time.Time
	lastPushPct int
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	tmpl, err := template.ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfgPath:   opts.ConfigPath,
		cfg:       opts.Config,
		store:     opts.Store,
		resticBin: opts.ResticBin,
		sched:     opts.Scheduler,
		tmpl:      tmpl,
		hub:       newSSEHub(),
		live:      map[string]*liveRun{},
	}, nil
}

// Handler returns the HTTP routes for the UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /job", s.handleJobEditor)
	mux.HandleFunc("POST /api/run", s.handleRun)
	mux.HandleFunc("POST /api/job", s.handleJobSave)
	mux.HandleFunc("POST /api/job/delete", s.handleJobDelete)
	mux.HandleFunc("POST /api/job/addpath", s.handleAddPath)
	mux.HandleFunc("POST /api/job/delpath", s.handleDelPath)
	mux.HandleFunc("POST /api/schedule", s.handleSchedule)
	mux.HandleFunc("GET /api/browse", s.handleBrowse)
	mux.HandleFunc("GET /api/snapshots", s.handleSnapshots)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/ingest", s.handleIngest)

	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	return mux
}

// ---- view models ----

type jobView struct {
	Name       string
	Schedule   string
	Paths      int
	Scheduled  bool
	LastStatus string
	LastWhen   string
	Size       string // total file size at last backup (bytes_processed), "" if never run
	Running    bool
	Percent    float64 // 0..100 for display
}

type runView struct {
	Started  string
	Job      string
	Status   string
	Snapshot string
	Added    string
	Duration string
	Error    string
}

type indexData struct {
	Jobs    []jobView
	History []runView
}

func (s *Server) buildJobs() []jobView {
	s.cfgMu.RLock()
	jobs := append([]config.Job(nil), s.cfg.Jobs...)
	s.cfgMu.RUnlock()

	var out []jobView
	for _, j := range jobs {
		jv := jobView{Name: j.Name, Schedule: j.Schedule, Paths: len(j.Paths), Scheduled: s.sched.IsInstalled(j.Name)}
		if last, err := s.store.RecentRuns(j.Name, 1); err == nil && len(last) == 1 {
			jv.LastStatus = last[0].Status
			jv.LastWhen = last[0].StartedAt.Local().Format("2006-01-02 15:04")
			jv.Size = humanBytes(last[0].BytesProcessed)
		}
		s.mu.Lock()
		if lr, ok := s.live[j.Name]; ok {
			jv.Running = true
			jv.Percent = lr.Percent * 100
		}
		s.mu.Unlock()
		out = append(out, jv)
	}
	return out
}

func (s *Server) buildHistory() []runView {
	runs, err := s.store.RecentRuns("", 15)
	if err != nil {
		return nil
	}
	out := make([]runView, 0, len(runs))
	for _, r := range runs {
		out = append(out, runView{
			Started:  r.StartedAt.Local().Format("2006-01-02 15:04"),
			Job:      r.Job,
			Status:   r.Status,
			Snapshot: shortID(r.SnapshotID),
			Added:    humanBytes(r.DataAddedBytes),
			Duration: r.FinishedAt.Sub(r.StartedAt).Round(time.Second).String(),
			Error:    r.ErrMsg,
		})
	}
	return out
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := indexData{Jobs: s.buildJobs(), History: s.buildHistory()}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---- job editing ----

type jobEdit struct {
	IsNew       bool
	Name        string
	Schedule    string
	Excludes    string // newline-joined for the textarea
	Paths       []string
	KeepLast    int
	KeepHourly  int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	Scheduled   bool
	Error       string
}

func (s *Server) handleJobEditor(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	ed := jobEdit{IsNew: true}
	if name != "" {
		s.cfgMu.RLock()
		j, ok := s.cfg.Job(name)
		s.cfgMu.RUnlock()
		if !ok {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		ed = jobEdit{
			Name: j.Name, Schedule: j.Schedule, Excludes: strings.Join(j.Excludes, "\n"),
			Paths: j.Paths, KeepLast: j.KeepLast, KeepHourly: j.KeepHourly,
			KeepDaily: j.KeepDaily, KeepWeekly: j.KeepWeekly, KeepMonthly: j.KeepMonthly,
			Scheduled: s.sched.IsInstalled(name),
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "job.html", ed); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleJobSave(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	schedule := strings.TrimSpace(r.FormValue("schedule"))

	s.cfgMu.Lock()
	existing, found := s.cfg.Job(name)
	j := config.Job{Name: name, Schedule: schedule}
	if found {
		j.Paths = existing.Paths // paths are managed separately
	}
	j.Excludes = splitLines(r.FormValue("excludes"))
	j.KeepLast = atoi(r.FormValue("keep_last"))
	j.KeepHourly = atoi(r.FormValue("keep_hourly"))
	j.KeepDaily = atoi(r.FormValue("keep_daily"))
	j.KeepWeekly = atoi(r.FormValue("keep_weekly"))
	j.KeepMonthly = atoi(r.FormValue("keep_monthly"))
	s.cfg.UpsertJob(j)
	err := config.Save(s.cfgPath, s.cfg)
	s.cfgMu.Unlock()
	if err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Keep an installed timer in sync with the (possibly changed) schedule.
	if s.sched.IsInstalled(name) {
		if schedule == "" {
			_ = s.sched.Disable(name)
		} else {
			_ = s.sched.Enable(name, schedule)
		}
	}

	s.pushJobs()
	w.Header().Set("HX-Redirect", "/job?name="+name) // stay on editor to add directories
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("job")
	_ = s.sched.Disable(name)
	s.cfgMu.Lock()
	s.cfg.RemoveJob(name)
	err := config.Save(s.cfgPath, s.cfg)
	s.cfgMu.Unlock()
	if err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.pushJobs()
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusNoContent)
}

type pathsData struct {
	Name  string
	Paths []string
}

func (s *Server) handleAddPath(w http.ResponseWriter, r *http.Request) {
	s.mutatePaths(w, r, func(paths []string, p string) []string {
		for _, e := range paths {
			if e == p {
				return paths // already present
			}
		}
		return append(paths, p)
	})
}

func (s *Server) handleDelPath(w http.ResponseWriter, r *http.Request) {
	s.mutatePaths(w, r, func(paths []string, p string) []string {
		out := paths[:0]
		for _, e := range paths {
			if e != p {
				out = append(out, e)
			}
		}
		return out
	})
}

func (s *Server) mutatePaths(w http.ResponseWriter, r *http.Request, fn func([]string, string) []string) {
	name := r.FormValue("job")
	p := strings.TrimSpace(r.FormValue("path"))
	if name == "" || p == "" {
		http.Error(w, "job and path are required", http.StatusBadRequest)
		return
	}
	s.cfgMu.Lock()
	j, ok := s.cfg.Job(name)
	if !ok {
		s.cfgMu.Unlock()
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	j.Paths = fn(append([]string(nil), j.Paths...), p)
	s.cfg.UpsertJob(j)
	err := config.Save(s.cfgPath, s.cfg)
	paths := j.Paths
	s.cfgMu.Unlock()
	if err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.pushJobs()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(s.render("pathsList", pathsData{Name: name, Paths: paths}))
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("job")
	enabled := r.FormValue("enabled") == "true"
	s.cfgMu.RLock()
	j, ok := s.cfg.Job(name)
	s.cfgMu.RUnlock()
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	var err error
	if enabled {
		err = s.sched.Enable(name, j.Schedule)
	} else {
		err = s.sched.Disable(name)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.pushJobs()
	w.WriteHeader(http.StatusNoContent)
}

// ---- directory browser ----

type browseEntry struct {
	Name string
	Path string
}

type browseData struct {
	Job         string
	Path        string
	Parent      string
	Dirs        []browseEntry
	Err         string
	ShowHidden  bool
	HiddenParam string // "1" while showing hidden (threaded into nav links), else ""
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	job := r.URL.Query().Get("job")
	path := r.URL.Query().Get("path")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		} else {
			path = "/"
		}
	}
	path = filepath.Clean(path)
	showHidden := r.URL.Query().Get("hidden") == "1"

	data := browseData{Job: job, Path: path, ShowHidden: showHidden}
	if showHidden {
		data.HiddenParam = "1"
	}
	if parent := filepath.Dir(path); parent != path {
		data.Parent = parent
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		data.Err = err.Error()
	} else {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if !showHidden && strings.HasPrefix(e.Name(), ".") {
				continue
			}
			data.Dirs = append(data.Dirs, browseEntry{Name: e.Name(), Path: filepath.Join(path, e.Name())})
		}
		sort.Slice(data.Dirs, func(i, j int) bool { return data.Dirs[i].Name < data.Dirs[j].Name })
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(s.render("browse", data))
}

// ---- snapshots (read-only listing) ----

type snapshotView struct {
	ID    string
	When  string
	Size  string
	Files int64
	Paths string
}

type snapshotsData struct {
	Job   string
	Snaps []snapshotView
	Err   string
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	job := r.URL.Query().Get("job")
	tag := ""
	if job != "" {
		tag = runner.JobTag(job)
	}
	data := snapshotsData{Job: job}
	snaps, err := runner.New(s.cfgSnapshot(), s.resticBin).Snapshots(r.Context(), tag)
	if err != nil {
		data.Err = err.Error()
	}
	for _, sn := range snaps {
		data.Snaps = append(data.Snaps, snapshotView{
			ID:    sn.ID,
			When:  sn.Time.Local().Format("2006-01-02 15:04"),
			Size:  humanBytes(sn.Size),
			Files: sn.Files,
			Paths: strings.Join(sn.Paths, ", "),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(s.render("snapshotsList", data))
}

// ---- repo mount (browse snapshots in a file manager) ----

func mountDirDefault() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "mashed-potato", "mnt")
	}
	return filepath.Join(os.TempDir(), "mashed-potato-mnt")
}

// MountAndOpen mounts the restic repo (once) and opens it in the file manager.
func (s *Server) MountAndOpen() error {
	s.mountMu.Lock()
	if s.mountCmd == nil {
		mp := mountDirDefault()
		if err := os.MkdirAll(mp, 0o755); err != nil {
			s.mountMu.Unlock()
			return err
		}
		cmd := runner.New(s.cfgSnapshot(), s.resticBin).MountCommand(mp)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			s.mountMu.Unlock()
			return err
		}
		s.mountCmd, s.mountDir = cmd, mp
		go func() {
			cmd.Wait()
			s.mountMu.Lock()
			if s.mountCmd == cmd {
				s.mountCmd = nil
			}
			s.mountMu.Unlock()
		}()
	}
	mp := s.mountDir
	s.mountMu.Unlock()

	// Wait briefly for the mount to populate before opening the file manager.
	for i := 0; i < 50; i++ {
		if entries, err := os.ReadDir(mp); err == nil && len(entries) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = exec.Command("xdg-open", mp).Start()
	return nil
}

// Unmount stops the restic mount (SIGINT lets restic unmount cleanly).
func (s *Server) Unmount() {
	s.mountMu.Lock()
	cmd := s.mountCmd
	s.mountMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGINT)
	}
}

// ---- run + SSE ----

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	job := r.FormValue("job")
	if job == "" {
		http.Error(w, "missing job", http.StatusBadRequest)
		return
	}
	switch err := s.startJob(job); err {
	case nil:
		w.WriteHeader(http.StatusNoContent)
	case errNoSuchJob:
		http.Error(w, "no such job", http.StatusNotFound)
	case errAlreadyRunning:
		http.Error(w, "job already running", http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	writeMsg(w, sseMsg{event: "jobs", data: s.render("jobsTable", s.buildJobs())})
	writeMsg(w, sseMsg{event: "history", data: s.render("historyTable", s.buildHistory())})
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			writeMsg(w, m)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

type runError string

func (e runError) Error() string { return string(e) }

const (
	errNoSuchJob      = runError("no such job")
	errAlreadyRunning = runError("job already running")
)

// TriggerJob starts a backup for the named job (used by the tray menu). It
// returns the same errors as the web "Run now" path.
func (s *Server) TriggerJob(name string) error { return s.startJob(name) }

// JobNames returns the configured job names (for the tray's Run submenu).
func (s *Server) JobNames() []string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	names := make([]string, 0, len(s.cfg.Jobs))
	for _, j := range s.cfg.Jobs {
		names = append(names, j.Name)
	}
	return names
}

func (s *Server) startJob(name string) error {
	s.cfgMu.RLock()
	job, ok := s.cfg.Job(name)
	s.cfgMu.RUnlock()
	if !ok {
		return errNoSuchJob
	}
	if !s.liveStart(name) {
		return errAlreadyRunning
	}

	go func() {
		r := runner.New(s.cfgSnapshot(), s.resticBin)
		r.Progress = func(pct float64, files []string) {
			cur := ""
			if len(files) > 0 {
				cur = files[0]
			}
			s.liveProgress(name, pct, cur)
		}
		result, _ := r.Backup(context.Background(), job)
		if _, err := s.store.RecordRun(result); err != nil {
			result.ErrMsg = "recorded with error: " + err.Error()
		}
		s.liveDone(name, result.Status)
	}()
	return nil
}

// liveStart marks a job running and reports false if it already was. It drives
// both in-process runs and ingested (external) runs.
func (s *Server) liveStart(name string) bool {
	s.mu.Lock()
	if _, running := s.live[name]; running {
		s.mu.Unlock()
		return false
	}
	s.live[name] = &liveRun{StartedAt: time.Now(), lastPushPct: -1}
	s.mu.Unlock()
	s.pushJobs()
	s.publishStatus()
	return true
}

// liveProgress updates a running job's percent (pct is a 0..1 fraction) and
// pushes to clients/tray only when the whole-percent value changes.
func (s *Server) liveProgress(name string, pct float64, current string) {
	s.mu.Lock()
	lr := s.live[name]
	push := false
	if lr != nil {
		lr.Percent = pct
		lr.Current = current
		if p := int(math.Floor(pct * 100)); p != lr.lastPushPct {
			lr.lastPushPct = p
			push = true
		}
	}
	s.mu.Unlock()
	if push {
		s.pushJobs()
		s.publishStatus()
	}
}

// liveDone clears the running marker and records the last result for status.
func (s *Server) liveDone(name, status string) {
	s.mu.Lock()
	delete(s.live, name)
	s.lastJob = name
	s.lastStatus = status
	s.mu.Unlock()
	s.pushJobs()
	s.pushHistory()
	s.publishStatus()
}

// handleIngest receives run lifecycle events from a separate `mashed-potatod run`
// process (e.g. a systemd-timer backup) so scheduled runs also stream live.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var ev struct {
		Type    string  `json:"type"`
		Job     string  `json:"job"`
		Percent float64 `json:"percent"`
		Current string  `json:"current"`
		Status  string  `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil || ev.Job == "" {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	switch ev.Type {
	case "started":
		s.liveStart(ev.Job)
	case "progress":
		s.liveProgress(ev.Job, ev.Percent, ev.Current)
	case "done":
		s.liveDone(ev.Job, ev.Status)
	default:
		http.Error(w, "unknown event type", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetStatusFunc registers a listener (the tray) for status changes and fires it
// once with the current status.
func (s *Server) SetStatusFunc(fn func(text, state string)) {
	s.statusMu.Lock()
	s.statusFn = fn
	s.statusMu.Unlock()
	s.publishStatus()
}

func (s *Server) publishStatus() {
	s.statusMu.Lock()
	fn := s.statusFn
	s.statusMu.Unlock()
	if fn == nil {
		return
	}
	text, state := s.statusText()
	fn(text, state)
}

func (s *Server) statusText() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.live); n > 0 {
		if n == 1 {
			for name, lr := range s.live {
				return fmt.Sprintf("running: %s %.0f%%", name, lr.Percent*100), "running"
			}
		}
		return fmt.Sprintf("running: %d jobs", n), "running"
	}
	if s.lastJob != "" {
		state := "idle"
		if s.lastStatus == "failed" {
			state = "error"
		}
		return fmt.Sprintf("idle — last: %s %s", s.lastJob, s.lastStatus), state
	}
	return "idle", "idle"
}

// cfgSnapshot returns the current config under the read lock (restic settings
// may be referenced by the runner while the config is being edited).
func (s *Server) cfgSnapshot() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) pushJobs() {
	s.hub.broadcast(sseMsg{event: "jobs", data: s.render("jobsTable", s.buildJobs())})
}

func (s *Server) pushHistory() {
	s.hub.broadcast(sseMsg{event: "history", data: s.render("historyTable", s.buildHistory())})
}

func (s *Server) render(name string, data any) []byte {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return []byte("<p>render error: " + template.HTMLEscapeString(err.Error()) + "</p>")
	}
	return buf.Bytes()
}

// ---- helpers ----

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// ---- SSE plumbing ----

type sseMsg struct {
	event string
	data  []byte
}

func writeMsg(w http.ResponseWriter, m sseMsg) {
	if m.event != "" {
		fmt.Fprintf(w, "event: %s\n", m.event)
	}
	for _, line := range bytes.Split(m.data, []byte("\n")) {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

type sseHub struct {
	mu   sync.Mutex
	subs map[chan sseMsg]struct{}
}

func newSSEHub() *sseHub { return &sseHub{subs: map[chan sseMsg]struct{}{}} }

func (h *sseHub) subscribe() chan sseMsg {
	ch := make(chan sseMsg, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan sseMsg) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *sseHub) broadcast(m sseMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- m:
		default:
		}
	}
}

// ---- small formatters ----

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
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
