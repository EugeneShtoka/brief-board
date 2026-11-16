// brief-board serves the daily brief as a task board and opens an interactive Claude Code session
// (wezterm mux, remote control) on a task when a button is pressed (reply, clarify, act). Drop is handled here.
//
// Env: BOARD_ADDR (:8787), BOARD_STATE (~/.local/state/brief-board/state.json),
// BOARD_AREAS (required: a JSON list of {"id", "name", "dir", "roadmap", "jiraRoom"}, the board's areas and the folders
// their sessions run in; work's is also where the brief runs; roadmap: the files the roadmap run reads, see defaultRoadmap;
// jiraRoom: the kith room the area's Jira bot posts in, see watchJira),
// BOARD_CLAUDE (claude), BOARD_URL (how claude reaches us),
// BOARD_TERMINAL (where sessions open: unset is the wezterm mux, else a command the session is appended to,
// run in the task's folder, e.g. "kitty", "alacritty -e" or "kitty @ launch --type=tab"),
// BOARD_SESSION_IN (tab | window: a wezterm session's new tab in the window last used, or its own window),
// BOARD_ISSUE_URL / BOARD_PR_URL (link bases for issue keys and #PR numbers in the page),
// BOARD_CLEAN_MERGED (1: a task moving to done with a merged pull request closes its session and removes the
// pull request's worktree, see cleanMerged).
// Every history entry is also appended to log/<monday>.jsonl next to the state file, one file per week.
package main

import (
	"bufio"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

type Entry struct {
	At    time.Time `json:"at"`
	Actor string    `json:"actor"` // eugene | claude
	Kind  string    `json:"kind"`  // note | reply | clarify | act | drop | status
	Text  string    `json:"text"`
}

// LogEntry is one line of a week file: an entry and the task it belongs to (none for board-level).
type LogEntry struct {
	Entry
	Task  string `json:"task,omitempty"`
	Title string `json:"title,omitempty"`
	Area  string `json:"area,omitempty"` // the task's area when it was written
}

type Job struct {
	Action    string     `json:"action"`
	Status    string     `json:"status"` // running | ok | failed
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Session   string     `json:"session,omitempty"` // button jobs: the Claude session's name, for remote control
	Pane      string     `json:"pane,omitempty"`    // button jobs: its wezterm pane id
}

type Task struct {
	ID         string              `json:"id"`
	Title      string              `json:"title"`
	Group      string              `json:"group"`            // waiting_on_you | ready | waiting_on_others | roadmap | later; urgency says what is urgent
	Status     string              `json:"status"`           // todo | doing | waiting | done | dropped
	Area       string              `json:"area,omitempty"`   // an id from BOARD_AREAS, required
	Source     string              `json:"source,omitempty"` // who maintains it: "" the brief | eugene | sweep (mail, calendar, personal chats) | roadmap (an area's roadmap files)
	Who        string              `json:"who,omitempty"`
	WaitingFor string              `json:"waitingFor,omitempty"` // what is owed between Eugene and who: a review, an email reply, a Jira answer, info
	Since      string              `json:"since,omitempty"`
	Order      int                 `json:"order,omitempty"`
	Summary    string              `json:"summary,omitempty"`    // the story: what was asked, by whom, what's known
	Tags       []string            `json:"tags,omitempty"`       // Eugene's own labels, set on the board
	Urgency    string              `json:"urgency,omitempty"`    // high | medium | low, medium unless set
	Importance string              `json:"importance,omitempty"` // high | medium | low, medium unless set
	Jira       []string            `json:"jira,omitempty"`
	JiraStatus map[string]string   `json:"jiraStatus,omitempty"` // key -> Jira status name, written by the brief
	Links      []map[string]string `json:"links,omitempty"`      // {label, url}
	Thread     map[string]string   `json:"thread,omitempty"`     // where a reply goes: {room, reply_to, thread}
	Messages   []map[string]string `json:"messages,omitempty"`   // the story's messages: {label, room, event, url}, opened at url, else in kith
	Files      []string            `json:"files,omitempty"`      // Eugene's attachments: absolute paths under files/<id> next to the state file
	DoneAt     *time.Time          `json:"doneAt,omitempty"`
	UpdatedAt  time.Time           `json:"updatedAt"`
	History    []Entry             `json:"history"`
	Job        *Job                `json:"job,omitempty"`
}

type State struct {
	Brief map[string]any   `json:"brief"`
	Tasks map[string]*Task `json:"tasks"`
	// Log is board-level activity (Jira triage, scheduled briefs), newest last, capped at logCap.
	Log []Entry `json:"log,omitempty"`
	// JiraSeen is, per Jira feed room, the event ids of its last read; a room with none yet sets a baseline.
	JiraSeen map[string][]string `json:"jiraSeenIn,omitempty"`
	// GitHubSince is the newest GitHub notification's updated_at seen; empty until the first poll sets a baseline.
	GitHubSince string `json:"githubSince,omitempty"`
	BriefRun    *Job   `json:"briefRun,omitempty"`
}

const logCap = 200

type Board struct {
	mu       sync.Mutex
	path     string
	logDir   string
	st       State
	workdir  string
	home     string
	claude   string
	terminal []string // BOARD_TERMINAL; empty is the wezterm mux
	window   bool     // wezterm: a session gets its own window, not a tab
	selfURL  string
	triage   chan struct{} // one Jira triage at a time
	wake     chan struct{} // nudges scheduleBrief out of its sleep
	rerun    bool          // a brief ran while kith was down: run it again now
	ghETag   string        // pollGitHub's own: the ETag of its last notifications answer
	clean    bool          // BOARD_CLEAN_MERGED
	areas    []Area        // BOARD_AREAS
}

// areaRe keeps an area id a plain lowercase name.
var areaRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Area is one area of the board: its id (what a task stores), the name the page shows, the folder its sessions run in,
// the globs, relative to that folder, of the files the roadmap run reads for its next items, and the kith room its Jira
// bot posts in, if it has a Jira.
type Area struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Dir      string   `json:"dir"`
	Roadmap  []string `json:"roadmap"`
	JiraRoom string   `json:"jiraRoom,omitempty"`
}

// defaultRoadmap is an area's roadmap when BOARD_AREAS gives it none; "roadmap": [] reads no files.
var defaultRoadmap = []string{"roadmap.md", "ROADMAP.md", "roadmap.txt", "plan.md", "PLAN.md", "plan.txt", "todo.md", "TODO.md", "todo.txt"}

// roadmapFiles is the files a's roadmap globs match, each once.
func (a *Area) roadmapFiles() []string {
	var files []string
	for _, g := range a.Roadmap {
		matches, _ := filepath.Glob(filepath.Join(a.Dir, g)) // the only error is a bad pattern, which matches nothing
		for _, f := range matches {
			if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() && !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	}
	return files
}

// parseAreas reads BOARD_AREAS, a JSON list of areas, each with its folder; work is required, since tasks from
// before areas were required are work, and the brief runs there. A missing name is the id; a dir of "~" or "~/..." is at home.
func (b *Board) parseAreas(conf string) ([]Area, error) {
	var areas []Area
	if conf == "" {
		return nil, errors.New("parseAreas: BOARD_AREAS is not set")
	}
	if err := json.Unmarshal([]byte(conf), &areas); err != nil {
		return nil, fmt.Errorf("parseAreas: %w", err)
	}
	seen := map[string]bool{}
	for i := range areas {
		a := &areas[i]
		if !areaRe.MatchString(a.ID) || seen[a.ID] {
			return nil, fmt.Errorf("parseAreas: id %q: want a unique lowercase folder-like name", a.ID)
		}
		if a.Dir == "" {
			return nil, fmt.Errorf("parseAreas: %s: no dir", a.ID)
		}
		seen[a.ID] = true
		a.Name = cmp.Or(a.Name, a.ID)
		if a.Roadmap == nil {
			a.Roadmap = defaultRoadmap
		}
		if a.Dir == "~" || strings.HasPrefix(a.Dir, "~/") {
			a.Dir = filepath.Join(b.home, a.Dir[1:])
		}
	}
	if !seen["work"] {
		return nil, errors.New("parseAreas: no work area")
	}
	return areas, nil
}

// dirFor is the folder a session on t runs in: its area's; an unknown area or a missing folder is an error.
func (b *Board) dirFor(t *Task) (string, error) {
	i := slices.IndexFunc(b.areas, func(a Area) bool { return a.ID == t.Area })
	if i < 0 {
		return "", fmt.Errorf("dirFor: no area %q in BOARD_AREAS", t.Area)
	}
	if fi, err := os.Stat(b.areas[i].Dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("dirFor: area %s: %s is not a folder", t.Area, b.areas[i].Dir)
	}
	return b.areas[i].Dir, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (b *Board) load() error {
	b.st = State{Brief: map[string]any{}, Tasks: map[string]*Task{}, JiraSeen: map[string][]string{}}
	data, err := os.ReadFile(b.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	if err := json.Unmarshal(data, &b.st); err != nil {
		return fmt.Errorf("load: %w", err)
	}
	// A task's area is required; ones from before that are work. The old "urgent" group is high urgency.
	for _, t := range b.st.Tasks {
		t.Area = cmp.Or(t.Area, "work")
		if t.Group == "urgent" {
			t.Group, t.Urgency = "", cmp.Or(t.Urgency, "high")
		}
		defaultLevels(t)
	}
	return nil
}

// save writes atomically; callers hold mu.
func (b *Board) save() error {
	data, err := json.MarshalIndent(b.st, "", "  ")
	if err != nil {
		return fmt.Errorf("save: %w", err)
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	return nil
}

// saveOrLog is save for the background loops, which have nobody to answer; callers hold mu.
func (b *Board) saveOrLog() {
	if err := b.save(); err != nil {
		log.Print(err)
	}
}

func (b *Board) note(t *Task, actor, kind, text string) {
	e := Entry{At: time.Now(), Actor: actor, Kind: kind, Text: text}
	t.History = append(t.History, e)
	t.UpdatedAt = time.Now()
	b.archive(&LogEntry{Entry: e, Task: t.ID, Title: t.Title, Area: t.Area})
}

// weekOf is the local Monday that starts t's week, as YYYY-MM-DD; it names the week's log file.
func weekOf(t time.Time) string {
	return t.AddDate(0, 0, -(int(t.Weekday())+6)%7).Format(time.DateOnly)
}

// archive appends e to its week's file; callers hold mu.
func (b *Board) archive(e *LogEntry) {
	f, err := os.OpenFile(filepath.Join(b.logDir, weekOf(e.At)+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("archive: %v", err)
		return
	}
	line, err := json.Marshal(e)
	if err == nil {
		_, err = f.Write(append(line, '\n'))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		log.Printf("archive: %v", err)
	}
}

// handleLog returns one week's entries: ?week=YYYY-MM-DD (any day of it), this week by default.
func (b *Board) handleLog(w http.ResponseWriter, r *http.Request) {
	week := weekOf(time.Now())
	if q := r.URL.Query().Get("week"); q != "" {
		d, err := time.Parse(time.DateOnly, q)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "week: want YYYY-MM-DD"})
			return
		}
		week = weekOf(d)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entries := []LogEntry{}
	// week is a formatted date, so the name stays inside logDir.
	if f, err := os.Open(filepath.Join(b.logDir, week+".jsonl")); err == nil { //nolint:gosec // see above
		rd := bufio.NewReader(f)
		for {
			line, err := rd.ReadBytes('\n')
			var e LogEntry
			if json.Unmarshal(line, &e) == nil {
				entries = append(entries, e)
			}
			if err != nil {
				break
			}
		}
		_ = f.Close() // read only: nothing to lose
	}
	older := false
	files, _ := os.ReadDir(b.logDir)
	for _, f := range files {
		older = older || strings.HasSuffix(f.Name(), ".jsonl") && f.Name() < week+".jsonl"
	}
	writeJSON(w, 200, map[string]any{"week": week, "entries": entries, "older": older})
}

func setStatus(t *Task, status string) {
	t.Status = status
	if status == "done" || status == "dropped" {
		if t.DoneAt == nil {
			now := time.Now()
			t.DoneAt = &now
		}
	} else {
		t.DoneAt = nil
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func (b *Board) handleState(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tasks := make([]*Task, 0, len(b.st.Tasks))
	for _, t := range b.st.Tasks {
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].Order < tasks[j].Order || tasks[i].Order == tasks[j].Order && tasks[i].ID < tasks[j].ID
	})
	writeJSON(w, 200, map[string]any{"brief": b.st.Brief, "tasks": tasks, "log": b.st.Log, "briefRun": b.st.BriefRun,
		"config": map[string]any{"issue": os.Getenv("BOARD_ISSUE_URL"), "pr": os.Getenv("BOARD_PR_URL"), "areas": b.areas}})
}

func (b *Board) handleBrief(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.Brief = body
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, body)
}

// handleBriefRun runs the scheduled brief now, from the board's button.
func (b *Board) handleBriefRun(w http.ResponseWriter, r *http.Request) {
	if envDuration("BOARD_BRIEF_EVERY") <= 0 {
		writeJSON(w, 409, map[string]string{"error": "the brief is not scheduled here (BOARD_BRIEF_EVERY)"})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.BriefRun != nil && b.st.BriefRun.Status == "running" {
		writeJSON(w, 409, map[string]string{"error": "a brief is already running"})
		return
	}
	b.briefNow()
	writeJSON(w, 202, map[string]string{"status": "queued"})
}

// mergePatch returns t with patch's fields over it. jiraStatus merges per key, so updating one
// ticket keeps the others.
func mergePatch(t *Task, patch map[string]json.RawMessage) (Task, error) {
	if raw, ok := patch["jiraStatus"]; ok {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return Task{}, fmt.Errorf("jiraStatus: %w", err)
		}
		merged := map[string]string{}
		maps.Copy(merged, t.JiraStatus)
		maps.Copy(merged, m)
		data, err := json.Marshal(merged)
		if err != nil {
			return Task{}, fmt.Errorf("jiraStatus: %w", err)
		}
		patch["jiraStatus"] = data
	}
	var fields map[string]json.RawMessage
	if err := remarshal(t, &fields); err != nil {
		return Task{}, err
	}
	maps.Copy(fields, patch)
	var next Task
	if err := remarshal(fields, &next); err != nil {
		return Task{}, err
	}
	return next, nil
}

// remarshal moves from into to through JSON.
func remarshal(from, to any) error {
	data, err := json.Marshal(from)
	if err == nil {
		err = json.Unmarshal(data, to)
	}
	if err != nil {
		return fmt.Errorf("task: %w", err)
	}
	return nil
}

// defaultLevels sets an unset urgency or importance to medium: every task has both.
func defaultLevels(t *Task) {
	t.Urgency, t.Importance = cmp.Or(t.Urgency, "medium"), cmp.Or(t.Importance, "medium")
}

// badField says what is wrong with a merged task's area, group or levels, or "".
func (b *Board) badField(t *Task) string {
	levels := []string{"high", "medium", "low"}
	switch {
	case !slices.ContainsFunc(b.areas, func(a Area) bool { return a.ID == t.Area }):
		return "area: required, an id from BOARD_AREAS"
	case t.Group == "urgent":
		return `group: "urgent" is gone, set urgency "high"`
	case !slices.Contains(levels, t.Urgency) || !slices.Contains(levels, t.Importance):
		return "urgency, importance: want high, medium or low"
	}
	return ""
}

// handleUpsert merges the body's fields into the task; history, job and files are server-owned.
func (b *Board) handleUpsert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	delete(patch, "history")
	delete(patch, "job")
	delete(patch, "files")
	delete(patch, "id")
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[id]
	if !ok {
		t = &Task{ID: id, Status: "todo", History: []Entry{}}
	}
	prevStatus := t.Status
	next, err := mergePatch(t, patch)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	defaultLevels(&next)
	if bad := b.badField(&next); bad != "" {
		writeJSON(w, 400, map[string]string{"error": bad})
		return
	}
	if !ok && r.URL.Query().Get("actor") == "eugene" {
		b.note(&next, "eugene", "created", "Added on the board.")
	}
	if ok && next.Area != t.Area {
		b.note(&next, cmp.Or(r.URL.Query().Get("actor"), "claude"), "area", t.Area+" → "+next.Area)
	}
	if _, set := patch["status"]; set && next.Status != prevStatus {
		setStatus(&next, next.Status)
		if ok {
			// ?actor=eugene marks a drag on the board; the brief's writes leave it unset.
			b.note(&next, cmp.Or(r.URL.Query().Get("actor"), "claude"), "status", prevStatus+" → "+next.Status)
		}
		if next.Status == "done" {
			b.doneNow(&next)
		}
	}
	next.UpdatedAt = time.Now()
	b.st.Tasks[id] = &next
	if err = b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, next)
}

func (b *Board) handleNote(w http.ResponseWriter, r *http.Request) {
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil || e.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "need {actor, kind, text}"})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[r.PathValue("id")]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such task"})
		return
	}
	b.note(t, e.Actor, e.Kind, e.Text)
	// A session's report on its action ends the job; the session itself stays open.
	if j := t.Job; j != nil && j.Status == "running" && j.Session != "" && e.Actor == "claude" && e.Kind == j.Action {
		now := time.Now()
		j.Status, j.EndedAt = "ok", &now
	}
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

const maxAttach = 32 << 20

// handleAttach stores the multipart "file" fields in files/<id>/ next to the state file and lists
// them on the task, so the next session on it reads them. A file with the same name is replaced.
func (b *Board) handleAttach(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAttach)
	if err := r.ParseMultipartForm(maxAttach); err != nil { //nolint:gosec // the body is capped by MaxBytesReader above
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	uploads := r.MultipartForm.File["file"]
	id := r.PathValue("id")
	// The id and the file names become paths: keep each a single plain name.
	if len(uploads) == 0 || !filepath.IsLocal(id) || slices.ContainsFunc(uploads, func(fh *multipart.FileHeader) bool {
		name := filepath.Base(fh.Filename)
		return name == "." || !filepath.IsLocal(name)
	}) {
		writeJSON(w, 400, map[string]string{"error": "need one or more files, each with a plain name"})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[id]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such task"})
		return
	}
	dir := filepath.Join(filepath.Dir(b.path), "files", id)
	var names []string
	for _, fh := range uploads {
		name := filepath.Base(fh.Filename)
		path := filepath.Join(dir, name)
		if err := saveUpload(fh, path); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if !slices.Contains(t.Files, path) {
			t.Files = append(t.Files, path)
		}
		names = append(names, name)
	}
	b.note(t, "eugene", "attach", strings.Join(names, ", "))
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

func saveUpload(fh *multipart.FileHeader, path string) error {
	src, err := fh.Open()
	if err != nil {
		return fmt.Errorf("saveUpload: %w", err)
	}
	defer func() { _ = src.Close() }()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // path is checked by handleAttach
		return fmt.Errorf("saveUpload: %w", err)
	}
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // path is checked by handleAttach
	if err != nil {
		return fmt.Errorf("saveUpload: %w", err)
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("saveUpload: %w", err)
	}
	return nil
}

type actRequest struct {
	Action  string `json:"action"` // reply | clarify | act | qa | drop; update comes from the Jira watcher
	Comment string `json:"comment"`
	Key     string `json:"key"` // qa only: the Jira key to move
}

func (b *Board) handleAct(w http.ResponseWriter, r *http.Request) {
	var req actRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[r.PathValue("id")]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such task"})
		return
	}
	switch req.Action {
	case "drop":
		setStatus(t, "dropped")
		b.note(t, "eugene", "drop", strings.TrimSpace("Dropped. "+req.Comment))
	case "reply", "clarify", "act", "qa":
		if req.Action == "qa" && !slices.Contains(t.Jira, req.Key) {
			writeJSON(w, 400, map[string]string{"error": "qa needs one of this task's Jira keys"})
			return
		}
		if b.busy(t) {
			writeJSON(w, 409, map[string]string{"error": "Claude is already working on this task"})
			return
		}
		text := req.Comment
		if req.Action == "qa" {
			text = strings.TrimSpace("Move " + req.Key + " to QA - Staging. " + req.Comment)
		}
		if text == "" {
			text = "(no guidance)"
		}
		b.note(t, "eugene", req.Action, text)
		b.start(t, req)
	default:
		writeJSON(w, 400, map[string]string{"error": "action must be reply, clarify, act or drop"})
		return
	}
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

const actionRules = `What each action means:
- reply: write the reply to the person in the task and SEND it now, in the room and thread where they raised it
  (task.thread names the room, reply_to and thread when known; otherwise find the message with kith-mcp).
  Write in that room's language and in Eugene's voice. Send once: never retry inside the room's cooldown,
  and report exactly what kith answered, "sent" or "drafted". A Jira question gets a Jira comment instead.
  An email (task.thread.email is its Gmail thread id) gets a Gmail draft reply in that thread, never a sent one.
- clarify: get the missing information Eugene's guidance names. If only a person can answer it, ask them
  in the thread where the item came from and send it. Otherwise investigate (Jira, chat, code, public API) and answer it yourself.
- act: do the task itself, as far as it can be done safely now. Follow the CLAUDE.md, skills and memory of the
  folder you run in, which the task's area picks (in the be repo: branch from development, pre-PR checklist, PR
  review post). Never merge, push to main, deploy or touch prod data. A task with source "roadmap" came from the
  roadmap file its summary names: once the work is done, mark its item done there, the way that file marks done items,
  in the work's own commit when git tracks the file, or just edited when it doesn't.
- qa: the task is done but its Jira ticket is still before QA. Transition that one key to "QA - Staging"
  (read its transitions first). A sub-task that has no QA state goes straight to Done. Change nothing else, then
  record the new status: PUT {"jiraStatus": {"<KEY>": "<status now>"}}.
- update: a Jira update arrived (from the Jira feed, not from Eugene). Read the ticket in Jira (status, newest
  comments) and refresh this task: summary (what changed, who said what), jiraStatus, messages. If the update asks
  something of Eugene (a mention, a question, an assignment), set group "waiting_on_you" and status "todo".
  Never send messages and never change Jira on an update.`

func (b *Board) prompt(t *Task, req actRequest) (string, error) {
	view := *t
	view.Job = nil
	taskJSON, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return "", fmt.Errorf("prompt: %w", err)
	}
	guidance := req.Comment
	if guidance == "" {
		guidance = "none"
	}
	var sb strings.Builder
	if req.Action == "update" {
		fmt.Fprintf(&sb, "A Jira update arrived for an item of Eugene's Brief Board (%s). Nobody is watching this run.\n\nTask:\n%s\n\nThe update: %s\n", b.selfURL, taskJSON, guidance)
	} else {
		fmt.Fprintf(&sb, "Eugene pressed %q on an item of his Brief Board (%s).\n\nTask:\n%s\n\nEugene's guidance: %s\n", req.Action, b.selfURL, taskJSON, guidance)
	}
	if req.Action == "qa" {
		fmt.Fprintf(&sb, "Jira key to move: %s\n", req.Key)
	}
	if len(t.Files) > 0 {
		sb.WriteString("Eugene attached files to this task (task.files): read them, they are part of it.\n")
	}
	fmt.Fprintf(&sb, "\n%s\n\nKeep the board current while you work (JSON body, fields merge):\n"+
		"  curl -s -X PUT %[2]s/api/tasks/%[3]s -H 'Content-Type: application/json' -d '{\"status\":\"waiting\",\"waitingFor\":\"PR review\"}'   # todo|doing|waiting|done|dropped; waitingFor: what who owes\n"+
		"  curl -s -X PUT %[2]s/api/tasks/%[3]s -H 'Content-Type: application/json' -d '{\"summary\":\"...\"}'    # when the story changed\n",
		actionRules, b.selfURL, t.ID)
	if req.Action == "update" {
		sb.WriteString("Your final message goes into the task's history as is: 2-4 plain lines saying what you did, what was sent or drafted, and links.\n")
	} else {
		fmt.Fprintf(&sb, "This is an interactive session Eugene may attach to and steer; the board does not see your final message. "+
			"When the action is done, post your report once, 2-4 plain lines saying what you did, what was sent or drafted, and links; "+
			"it goes into the task's history and marks the job finished:\n"+
			"  jq -n --arg text '...' '{actor: \"claude\", kind: \"%[3]s\", text: $text}' | curl -s -X POST %[1]s/api/tasks/%[2]s/notes -H 'Content-Type: application/json' -d @-\n"+
			"Then stay open for Eugene.\n", b.selfURL, t.ID, req.Action)
	}
	return sb.String(), nil
}

// start runs Claude on t; callers hold mu. A Jira update runs headless; a button press gets its own
// interactive session in the wezterm mux server, which outlives a board restart and can be attached to.
func (b *Board) start(t *Task, req actRequest) {
	t.Job = &Job{Action: req.Action, Status: "running", StartedAt: time.Now()}
	prompt, err := b.prompt(t, req)
	if err == nil && req.Action == "update" {
		go b.run(t.ID, req.Action, prompt)
		return
	}
	if err == nil {
		t.Job.Session = t.ID + "-" + req.Action
		var dir string
		if dir, err = b.dirFor(t); err == nil {
			t.Job.Pane, err = b.spawnSession(dir, t.Job.Session, prompt)
		}
	}
	if err != nil {
		now := time.Now()
		t.Job.Status, t.Job.EndedAt = "failed", &now
		b.note(t, "claude", req.Action, "Could not start a session: "+err.Error())
		return
	}
	where := "your terminal"
	if t.Job.Pane != "" {
		where = "wezterm pane " + t.Job.Pane
	}
	b.note(t, "claude", "session", fmt.Sprintf("Started session %q in %s: attach there, or on claude.ai/code (remote control).", t.Job.Session, where))
}

// spawnSession opens an interactive Claude Code session and returns its wezterm pane id ("" in another terminal).
// In wezterm the pane closes when Claude exits, so a live pane means a live session.
func (b *Board) spawnSession(dir, name, prompt string) (string, error) {
	// zsh -lic gives the session the user's login env; the service's own PATH is bare.
	// claude and the prompt reach zsh as "$0" and "$@", never as shell text.
	session := []string{"zsh", "-lic", `exec "$0" "$@"`, b.claude, "--permission-mode", "auto", "--remote-control", name, "-n", name, prompt}
	if len(b.terminal) > 0 {
		// shortcut: no pane to watch, so a session that dies without reporting keeps its task busy,
		// and a terminal that stays our child dies with a board restart; track its process if another terminal gets real use.
		cmd := exec.CommandContext(context.Background(), b.terminal[0], slices.Concat(b.terminal[1:], session)...) //nolint:gosec // BOARD_TERMINAL is the service's own setting
		cmd.Dir = dir
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("spawn: %w", err)
		}
		go func() { _ = cmd.Wait() }() // reap it; its exit says nothing about the session
		return "", nil
	}
	spawn := []string{"cli", "spawn", "--cwd", dir}
	if b.window {
		spawn = append(spawn, "--new-window")
	}
	pane, err := wezterm(slices.Concat(spawn, []string{"--"}, session))
	if err != nil && !b.window {
		// No GUI attached to take the tab (wezterm cli picks the last-focused client's window): open a mux window.
		pane, err = wezterm(slices.Concat(spawn, []string{"--new-window", "--"}, session))
	}
	return pane, err
}

// wezterm runs wezterm with args and returns its trimmed output.
func wezterm(args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wezterm", args...).Output() //nolint:gosec // args are the session's, passed as argv
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("spawn: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("spawn: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// paneAlive reports whether the wezterm mux still has pane; if the mux can't be asked, it assumes yes.
func paneAlive(pane string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wezterm", "cli", "list", "--format", "json").Output()
	var panes []struct {
		PaneID int `json:"pane_id"`
	}
	if err != nil || json.Unmarshal(out, &panes) != nil {
		return true
	}
	for _, p := range panes {
		if fmt.Sprint(p.PaneID) == pane {
			return true
		}
	}
	return false
}

// busy reports whether t has a job still running; a session whose pane is gone ended without reporting,
// and is marked failed so the buttons work again. Callers hold mu.
func (b *Board) busy(t *Task) bool {
	if t.Job == nil || t.Job.Status != "running" {
		return false
	}
	if t.Job.Pane == "" || paneAlive(t.Job.Pane) {
		return true
	}
	now := time.Now()
	t.Job.Status, t.Job.EndedAt = "failed", &now
	b.note(t, "claude", t.Job.Action, fmt.Sprintf("Session %q ended without reporting back.", t.Job.Session))
	return false
}

// reapSessions fails every session job whose pane is gone, so the board corrects itself without waiting
// for a button press on that task.
func (b *Board) reapSessions() {
	b.mu.Lock()
	defer b.mu.Unlock()
	reaped := false
	for _, t := range b.st.Tasks {
		if t.Job != nil && t.Job.Status == "running" && t.Job.Pane != "" && !b.busy(t) {
			reaped = true
		}
	}
	if reaped {
		b.saveOrLog()
	}
}

func (b *Board) logf(kind, format string, args ...any) {
	e := Entry{At: time.Now(), Actor: "claude", Kind: kind, Text: fmt.Sprintf(format, args...)}
	b.st.Log = append(b.st.Log, e)
	b.archive(&LogEntry{Entry: e})
	if len(b.st.Log) > logCap {
		b.st.Log = b.st.Log[len(b.st.Log)-logCap:]
	}
}

func (b *Board) run(id, action, prompt string) {
	status, text := runClaude(b.claude, b.workdir, prompt)

	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[id]
	if !ok {
		return
	}
	now := time.Now()
	t.Job = &Job{Action: action, Status: status, StartedAt: t.Job.StartedAt, EndedAt: &now}
	b.note(t, "claude", action, text)
	if err := b.save(); err != nil {
		log.Printf("save after %s on %s: %v", action, id, err)
	}
}

type streamEvent struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// runClaude runs one headless Claude Code session and returns its final message.
// It reads stream-json because `--output-format json` only prints at exit, and the
// process can linger after its result (async hooks); it's killed 30s after the result.
func runClaude(claude, dir, prompt string) (status, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "-p", prompt, "--permission-mode", "auto", "--output-format", "stream-json", "--verbose") //nolint:gosec // claude is BOARD_CLAUDE, the service's own setting
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BRIEF_BOARD=1")
	// Own process group, so a timeout also kills its hooks and MCP servers.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return "failed", err.Error()
	}

	var res *streamEvent
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var ev streamEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "result" {
			res = &ev
			time.AfterFunc(30*time.Second, cancel)
			break
		}
	}
	werr := cmd.Wait()

	if res != nil {
		if res.IsError {
			return "failed", res.Result
		}
		return "ok", res.Result
	}
	msg := "no result from claude"
	if werr != nil {
		msg += ": " + werr.Error()
	}
	if e := strings.TrimSpace(stderr.String()); e != "" {
		msg += "\n" + e
	}
	return "failed", msg
}

// failInterrupted marks the headless jobs a restart killed as failed; don't leave them looking alive.
// Sessions live on in their terminal.
func (b *Board) failInterrupted() {
	for _, t := range b.st.Tasks {
		if t.Job != nil && t.Job.Status == "running" && t.Job.Session == "" {
			t.Job.Status = "failed"
			b.note(t, "claude", t.Job.Action, "Interrupted: the board restarted while this was running.")
		}
	}
	if b.st.BriefRun != nil && b.st.BriefRun.Status == "running" {
		b.st.BriefRun.Status = "failed"
	}
}

func (b *Board) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write(indexHTML); err != nil {
			log.Printf("index: %v", err)
		}
	})
	mux.HandleFunc("GET /api/state", b.handleState)
	mux.HandleFunc("GET /api/log", b.handleLog)
	mux.HandleFunc("PUT /api/brief", b.handleBrief)
	mux.HandleFunc("POST /api/brief/run", b.handleBriefRun)
	mux.HandleFunc("PUT /api/tasks/{id}", b.handleUpsert)
	mux.HandleFunc("POST /api/tasks/{id}/notes", b.handleNote)
	mux.HandleFunc("POST /api/tasks/{id}/files", b.handleAttach)
	mux.HandleFunc("POST /api/tasks/{id}/act", b.handleAct)
	return mux
}

func main() {
	home, _ := os.UserHomeDir()
	addr := env("BOARD_ADDR", ":8787")
	b := &Board{
		path:     env("BOARD_STATE", filepath.Join(home, ".local", "state", "brief-board", "state.json")),
		home:     home,
		claude:   env("BOARD_CLAUDE", "claude"),
		terminal: strings.Fields(os.Getenv("BOARD_TERMINAL")),
		window:   os.Getenv("BOARD_SESSION_IN") == "window",
		clean:    os.Getenv("BOARD_CLEAN_MERGED") == "1",
		selfURL:  env("BOARD_URL", "http://localhost"+addr),
		triage:   make(chan struct{}, 1),
		wake:     make(chan struct{}, 1),
	}
	areas, err := b.parseAreas(os.Getenv("BOARD_AREAS"))
	if err != nil {
		log.Fatal(err)
	}
	b.areas = areas
	if b.workdir, err = b.dirFor(&Task{Area: "work"}); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		log.Fatal(err)
	}
	if err := b.load(); err != nil {
		log.Fatalf("load %s: %v", b.path, err)
	}
	b.logDir = filepath.Join(filepath.Dir(b.path), "log")
	if err := b.seedLog(); err != nil {
		log.Fatal(err)
	}
	b.failInterrupted()
	b.saveOrLog()
	go func() {
		for ; ; time.Sleep(time.Minute) {
			b.reapSessions()
		}
	}()

	// BOARD_ISSUE_KEY: the key pattern in the Jira feeds (each area's jiraRoom) and GitHub notifications.
	keyRe := regexp.MustCompile(env("BOARD_ISSUE_KEY", `\b[A-Z][A-Z0-9]+-\d+\b`))
	for _, a := range b.areas {
		if a.JiraRoom != "" {
			every := cmp.Or(envDuration("BOARD_JIRA_POLL"), 2*time.Minute)
			go b.watchJira(env("BOARD_KITH_MCP", "kith-mcp"), &a, keyRe, every)
			log.Printf("watching %q for %s's Jira updates every %s", a.JiraRoom, a.ID, every)
		}
	}
	// BOARD_GITHUB_POLL: how often to read GitHub notifications (via gh) onto the tasks they are about; unset is off.
	go b.watchGitHub(keyRe, envDuration("BOARD_GITHUB_POLL"))
	if every := envDuration("BOARD_BRIEF_EVERY"); every > 0 {
		go b.scheduleBrief(every)
		log.Printf("brief every %s", every)
	}

	log.Printf("brief-board on %s, state %s, claude runs in %s", addr, b.path, b.workdir)
	srv := &http.Server{Addr: addr, Handler: b.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// seedLog creates the week files on their first start, from the history state.json already holds.
func (b *Board) seedLog() error {
	if _, err := os.Stat(b.logDir); !os.IsNotExist(err) {
		return nil
	}
	if err := os.MkdirAll(b.logDir, 0o700); err != nil {
		return fmt.Errorf("seedLog: %w", err)
	}
	var all []LogEntry
	for _, t := range b.st.Tasks {
		for _, e := range t.History {
			all = append(all, LogEntry{Entry: e, Task: t.ID, Title: t.Title, Area: t.Area})
		}
	}
	for _, e := range b.st.Log {
		all = append(all, LogEntry{Entry: e})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })
	for i := range all {
		b.archive(&all[i])
	}
	return nil
}
