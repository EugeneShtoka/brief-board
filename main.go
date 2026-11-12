// brief-board serves the daily brief as a task board and runs headless Claude Code
// on a task when a button is pressed (reply, clarify, act). Drop is handled here.
//
// Env: BOARD_ADDR (:8787), BOARD_STATE (~/.local/state/brief-board/state.json),
// BOARD_WORKDIR (where claude runs), BOARD_CLAUDE (claude), BOARD_URL (how claude reaches us),
// BOARD_ISSUE_URL / BOARD_PR_URL (link bases for issue keys and #PR numbers in the page).
// Every history entry is also appended to log/<monday>.jsonl next to the state file, one file per week.
package main

import (
	"bufio"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
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
}

type Job struct {
	Action    string     `json:"action"`
	Status    string     `json:"status"` // running | ok | failed
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

type Task struct {
	ID         string              `json:"id"`
	Title      string              `json:"title"`
	Group      string              `json:"group"`  // urgent | waiting_on_you | ready | waiting_on_others | roadmap | later
	Status     string              `json:"status"` // todo | doing | waiting | done | dropped
	Who        string              `json:"who,omitempty"`
	Since      string              `json:"since,omitempty"`
	Order      int                 `json:"order,omitempty"`
	Summary    string              `json:"summary,omitempty"` // the story: what was asked, by whom, what's known
	Jira       []string            `json:"jira,omitempty"`
	JiraStatus map[string]string   `json:"jiraStatus,omitempty"` // key -> Jira status name, written by the brief
	Links      []map[string]string `json:"links,omitempty"`      // {label, url}
	Thread     map[string]string   `json:"thread,omitempty"`     // where a reply goes: {room, reply_to, thread}
	Messages   []map[string]string `json:"messages,omitempty"`   // the story's messages: {label, room, event}, opened in kith
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
	// JiraSeen is the event ids of the Jira feed's last read; nil until the first read sets a baseline.
	JiraSeen []string `json:"jiraSeen,omitempty"`
	BriefRun *Job     `json:"briefRun,omitempty"`
}

const logCap = 200

type Board struct {
	mu      sync.Mutex
	path    string
	logDir  string
	st      State
	workdir string
	claude  string
	selfURL string
	triage  chan struct{} // one Jira triage at a time
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (b *Board) load() error {
	b.st = State{Brief: map[string]any{}, Tasks: map[string]*Task{}}
	data, err := os.ReadFile(b.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &b.st)
}

// save writes atomically; callers hold mu.
func (b *Board) save() error {
	data, err := json.MarshalIndent(b.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

func (b *Board) note(t *Task, actor, kind, text string) {
	e := Entry{At: time.Now(), Actor: actor, Kind: kind, Text: text}
	t.History = append(t.History, e)
	t.UpdatedAt = time.Now()
	b.archive(LogEntry{Entry: e, Task: t.ID, Title: t.Title})
}

// weekOf is the local Monday that starts t's week, as YYYY-MM-DD; it names the week's log file.
func weekOf(t time.Time) string {
	return t.AddDate(0, 0, -(int(t.Weekday())+6)%7).Format(time.DateOnly)
}

// archive appends e to its week's file; callers hold mu.
func (b *Board) archive(e LogEntry) {
	f, err := os.OpenFile(filepath.Join(b.logDir, weekOf(e.At)+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("archive: %v", err)
		return
	}
	defer f.Close()
	line, _ := json.Marshal(e)
	f.Write(append(line, '\n'))
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
	if f, err := os.Open(filepath.Join(b.logDir, week+".jsonl")); err == nil {
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
		f.Close()
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
	json.NewEncoder(w).Encode(v)
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
		"config": map[string]string{"issue": os.Getenv("BOARD_ISSUE_URL"), "pr": os.Getenv("BOARD_PR_URL")}})
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

// handleUpsert merges the body's fields into the task; history and job are server-owned.
func (b *Board) handleUpsert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	delete(patch, "history")
	delete(patch, "job")
	delete(patch, "id")
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[id]
	if !ok {
		t = &Task{ID: id, Status: "todo", History: []Entry{}}
	}
	prevStatus := t.Status
	// jiraStatus merges per key, so updating one ticket keeps the others.
	if raw, ok := patch["jiraStatus"]; ok {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			writeJSON(w, 400, map[string]string{"error": "jiraStatus: " + err.Error()})
			return
		}
		merged := map[string]string{}
		for k, v := range t.JiraStatus {
			merged[k] = v
		}
		for k, v := range m {
			merged[k] = v
		}
		patch["jiraStatus"], _ = json.Marshal(merged)
	}
	cur, _ := json.Marshal(t)
	var merged map[string]json.RawMessage
	json.Unmarshal(cur, &merged)
	for k, v := range patch {
		merged[k] = v
	}
	raw, _ := json.Marshal(merged)
	var next Task
	if err := json.Unmarshal(raw, &next); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if _, set := patch["status"]; set && next.Status != prevStatus {
		setStatus(&next, next.Status)
		if ok {
			// ?actor=eugene marks a drag on the board; the brief's writes leave it unset.
			b.note(&next, cmp.Or(r.URL.Query().Get("actor"), "claude"), "status", prevStatus+" → "+next.Status)
		}
	}
	next.UpdatedAt = time.Now()
	b.st.Tasks[id] = &next
	if err := b.save(); err != nil {
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
	b.save()
	writeJSON(w, 200, t)
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
		if t.Job != nil && t.Job.Status == "running" {
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
	b.save()
	writeJSON(w, 200, t)
}

const actionRules = `What each action means:
- reply: write the reply to the person in the task and SEND it now, in the room and thread where they raised it
  (task.thread names the room, reply_to and thread when known; otherwise find the message with kith-mcp).
  Write in that room's language and in Eugene's voice. Send once: never retry inside the room's cooldown,
  and report exactly what kith answered, "sent" or "drafted". A Jira question gets a Jira comment instead.
- clarify: get the missing information Eugene's guidance names. If only a person can answer it, ask them
  in the thread where the item came from and send it. Otherwise investigate (Jira, chat, code, public API) and answer it yourself.
- act: do the task itself, as far as it can be done safely now. Follow the repo's CLAUDE.md, skills and memory
  (branch from development, pre-PR checklist, PR review post). Never merge, push to main, deploy or touch prod data.
- qa: the task is done but its Jira ticket is still before QA. Transition that one key to "QA - Staging"
  (read its transitions first). A sub-task that has no QA state goes straight to Done. Change nothing else, then
  record the new status: PUT {"jiraStatus": {"<KEY>": "<status now>"}}.
- update: a Jira update arrived (from the Jira feed, not from Eugene). Read the ticket in Jira (status, newest
  comments) and refresh this task: summary (what changed, who said what), jiraStatus, messages. If the update asks
  something of Eugene (a mention, a question, an assignment), set group "waiting_on_you" and status "todo".
  Never send messages and never change Jira on an update.`

func (b *Board) prompt(t *Task, req actRequest) string {
	view := *t
	view.Job = nil
	taskJSON, _ := json.MarshalIndent(view, "", "  ")
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
	fmt.Fprintf(&sb, "\n%s\n\nKeep the board current while you work (JSON body, fields merge):\n"+
		"  curl -s -X PUT %[2]s/api/tasks/%[3]s -H 'Content-Type: application/json' -d '{\"status\":\"waiting\"}'   # todo|doing|waiting|done|dropped\n"+
		"  curl -s -X PUT %[2]s/api/tasks/%[3]s -H 'Content-Type: application/json' -d '{\"summary\":\"...\"}'    # when the story changed\n"+
		"Your final message goes into the task's history as is: 2-4 plain lines saying what you did, what was sent or drafted, and links.\n",
		actionRules, b.selfURL, t.ID)
	return sb.String()
}

// start runs Claude on t in the background; callers hold mu.
func (b *Board) start(t *Task, req actRequest) {
	t.Job = &Job{Action: req.Action, Status: "running", StartedAt: time.Now()}
	go b.run(t.ID, req.Action, b.prompt(t, req))
}

func (b *Board) logf(kind, format string, args ...any) {
	e := Entry{At: time.Now(), Actor: "claude", Kind: kind, Text: fmt.Sprintf(format, args...)}
	b.st.Log = append(b.st.Log, e)
	b.archive(LogEntry{Entry: e})
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
	cmd := exec.CommandContext(ctx, claude, "-p", prompt, "--permission-mode", "auto", "--output-format", "stream-json", "--verbose")
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

func main() {
	home, _ := os.UserHomeDir()
	addr := env("BOARD_ADDR", ":8787")
	b := &Board{
		path:    env("BOARD_STATE", filepath.Join(home, ".local/state/brief-board/state.json")),
		workdir: env("BOARD_WORKDIR", home),
		claude:  env("BOARD_CLAUDE", "claude"),
		selfURL: env("BOARD_URL", "http://localhost"+addr),
		triage:  make(chan struct{}, 1),
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		log.Fatal(err)
	}
	if err := b.load(); err != nil {
		log.Fatalf("load %s: %v", b.path, err)
	}
	b.logDir = filepath.Join(filepath.Dir(b.path), "log")
	// First start with week files: seed them from the history state.json already holds.
	if _, err := os.Stat(b.logDir); os.IsNotExist(err) {
		if err := os.MkdirAll(b.logDir, 0o700); err != nil {
			log.Fatal(err)
		}
		var all []LogEntry
		for _, t := range b.st.Tasks {
			for _, e := range t.History {
				all = append(all, LogEntry{Entry: e, Task: t.ID, Title: t.Title})
			}
		}
		for _, e := range b.st.Log {
			all = append(all, LogEntry{Entry: e})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })
		for _, e := range all {
			b.archive(e)
		}
	}
	// A restart kills running jobs; don't leave them looking alive.
	for _, t := range b.st.Tasks {
		if t.Job != nil && t.Job.Status == "running" {
			t.Job.Status = "failed"
			b.note(t, "claude", t.Job.Action, "Interrupted: the board restarted while this was running.")
		}
	}
	if b.st.BriefRun != nil && b.st.BriefRun.Status == "running" {
		b.st.BriefRun.Status = "failed"
	}
	b.save()

	// BOARD_JIRA_ROOM: the kith room the Jira bot posts in; BOARD_ISSUE_KEY: the key pattern in it.
	if room := os.Getenv("BOARD_JIRA_ROOM"); room != "" {
		every := cmp.Or(envDuration("BOARD_JIRA_POLL"), 2*time.Minute)
		go b.watchJira(env("BOARD_KITH_MCP", "kith-mcp"), room, regexp.MustCompile(env("BOARD_ISSUE_KEY", `\b[A-Z][A-Z0-9]+-\d+\b`)), every)
		log.Printf("watching %s for Jira updates every %s", room, every)
	}
	if every := envDuration("BOARD_BRIEF_EVERY"); every > 0 {
		go b.scheduleBrief(every)
		log.Printf("brief every %s", every)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", b.handleState)
	mux.HandleFunc("GET /api/log", b.handleLog)
	mux.HandleFunc("PUT /api/brief", b.handleBrief)
	mux.HandleFunc("PUT /api/tasks/{id}", b.handleUpsert)
	mux.HandleFunc("POST /api/tasks/{id}/notes", b.handleNote)
	mux.HandleFunc("POST /api/tasks/{id}/act", b.handleAct)
	log.Printf("brief-board on %s, state %s, claude runs in %s", addr, b.path, b.workdir)
	log.Fatal(http.ListenAndServe(addr, mux))
}
