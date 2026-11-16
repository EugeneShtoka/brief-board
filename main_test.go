package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWeekOf(t *testing.T) {
	for day, want := range map[string]string{
		"2026-11-09": "2026-11-09", // Monday
		"2026-11-12": "2026-11-09",
		"2026-11-15": "2026-11-09", // Sunday stays in the week it ends
		"2026-11-16": "2026-11-16",
		"2026-01-01": "2025-12-29", // across a year
	} {
		d, _ := time.ParseInLocation(time.DateOnly, day, time.Local)
		if got := weekOf(d.Add(23 * time.Hour)); got != want {
			t.Errorf("weekOf(%s) = %s, want %s", day, got, want)
		}
	}
}

func TestRoadmap(t *testing.T) {
	kith, empty := t.TempDir(), t.TempDir()
	for _, f := range []string{"ROADMAP.md", "planning/roadmap.md", "notes.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(kith, f)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(kith, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := &Board{home: "/home", selfURL: "http://board"}
	var err error
	b.areas, err = b.parseAreas(`[{"id": "work", "dir": "` + empty + `", "roadmap": []}, {"id": "kith", "dir": "` + kith + `"},
		{"id": "plans", "dir": "` + kith + `", "roadmap": ["*/roadmap.md", "ROADMAP.md", "planning/*.md", "["]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.areas[1].roadmapFiles(), []string{filepath.Join(kith, "ROADMAP.md")}; !slices.Equal(got, want) {
		t.Errorf("default roadmapFiles = %v, want %v", got, want)
	}
	// Each file once, in glob order; a bad glob matches nothing.
	plan := filepath.Join(kith, "planning", "roadmap.md")
	if got, want := b.areas[2].roadmapFiles(), []string{plan, filepath.Join(kith, "ROADMAP.md")}; !slices.Equal(got, want) {
		t.Errorf("roadmapFiles = %v, want %v", got, want)
	}
	if p := b.roadmapPrompt(); !strings.Contains(p, `area "plans": `+plan) || strings.Contains(p, `area "work"`) {
		t.Errorf("roadmapPrompt lists the wrong files:\n%s", p)
	}
	b.areas = b.areas[:1]
	if p := b.roadmapPrompt(); p != "" {
		t.Errorf("roadmapPrompt with no files = %q, want none", p)
	}
}

func TestAreas(t *testing.T) {
	work, kith := t.TempDir(), t.TempDir()
	b := &Board{home: "/home"}
	areas, err := b.parseAreas(`[{"id": "work", "name": "Work", "dir": "` + work + `"}, {"id": "kith", "name": "Kith", "dir": "` + kith + `"},
		{"id": "nixos", "dir": "~/dev/nixos"}, {"id": "personal", "name": "Personal", "dir": "~"}]`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Area{{"work", "Work", work, defaultRoadmap, ""}, {"kith", "Kith", kith, defaultRoadmap, ""},
		{"nixos", "nixos", "/home/dev/nixos", defaultRoadmap, ""}, {"personal", "Personal", "/home", defaultRoadmap, ""}}
	if !reflect.DeepEqual(areas, want) {
		t.Errorf("parseAreas = %v, want %v", areas, want)
	}
	for _, bad := range []string{``, `{}`, `[{"id": "Kith", "dir": "/"}]`, `[{"id": "work", "dir": "/"}, {"id": "work", "dir": "/"}]`,
		`[{"name": "no id", "dir": "/"}]`, `[{"id": "work"}]`, `[{"id": "kith", "dir": "/"}]`} {
		if _, err := b.parseAreas(bad); err == nil {
			t.Errorf("parseAreas(%s) took it", bad)
		}
	}
	b.areas = areas
	for area, want := range map[string]string{"work": work, "kith": kith} {
		if got, err := b.dirFor(&Task{Area: area}); got != want || err != nil {
			t.Errorf("dirFor(%q) = %s, %v; want %s", area, got, err, want)
		}
	}
	// No fallback: an unknown area or a missing folder is an error, not home.
	for _, area := range []string{"nixos", "gone", ""} {
		if got, err := b.dirFor(&Task{Area: area}); err == nil {
			t.Errorf("dirFor(%q) = %s, want an error", area, got)
		}
	}
	if b.badField(&Task{Area: "kith", Urgency: "medium", Importance: "medium"}) != "" || b.badField(&Task{Area: "gone"}) == "" || b.badField(&Task{}) == "" {
		t.Error("badField: want a configured area, required")
	}
	// A task saved before areas and levels were required loads as work, medium; the old "urgent" group as high urgency.
	b.path = filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(b.path, []byte(`{"tasks": {"old": {"id": "old"}, "k": {"id": "k", "area": "kith"},
		"u": {"id": "u", "group": "urgent"}, "ul": {"id": "ul", "group": "urgent", "urgency": "low"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	if b.st.Tasks["old"].Area != "work" || b.st.Tasks["k"].Area != "kith" {
		t.Errorf("load: areas %q, %q; want work, kith", b.st.Tasks["old"].Area, b.st.Tasks["k"].Area)
	}
	for id, want := range map[string]string{"old": "medium", "u": "high", "ul": "low"} {
		if got := b.st.Tasks[id]; got.Urgency != want || got.Importance != "medium" || got.Group != "" {
			t.Errorf("load %s: urgency %q, importance %q, group %q; want %s, medium, none", id, got.Urgency, got.Importance, got.Group, want)
		}
	}
}

func TestKithBack(t *testing.T) {
	dir := t.TempDir()
	lastOK := time.Now()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, wake: make(chan struct{}, 1)}
	b.st.BriefRun = &Job{StartedAt: lastOK.Add(-time.Hour)}
	if b.kithBack(lastOK) || b.rerun {
		t.Error("a brief from before the outage was re-run")
	}
	b.st.BriefRun.StartedAt = lastOK.Add(time.Minute) // ran while kith was down
	if !b.kithBack(lastOK) || !b.rerun || len(b.wake) != 1 {
		t.Error("a brief from during the outage was not re-run")
	}
	b.kithBack(lastOK) // a second nudge must not block on the full channel
}

func TestHandleBriefRun(t *testing.T) {
	t.Setenv("BOARD_BRIEF_EVERY", "")
	b := &Board{wake: make(chan struct{}, 1)}
	run := func() int {
		rec := httptest.NewRecorder()
		b.handleBriefRun(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/api/brief/run", nil))
		return rec.Code
	}
	if code := run(); code != 409 || b.rerun {
		t.Errorf("unscheduled: %d, rerun %v; want 409 and no run", code, b.rerun)
	}
	t.Setenv("BOARD_BRIEF_EVERY", "6h")
	b.st.BriefRun = &Job{Status: "running"}
	if code := run(); code != 409 || b.rerun {
		t.Errorf("while running: %d, rerun %v; want 409 and no run", code, b.rerun)
	}
	b.st.BriefRun.Status = "ok"
	if code := run(); code != 202 || !b.rerun || len(b.wake) != 1 {
		t.Errorf("run: %d, rerun %v, wake %d; want 202 and a nudge", code, b.rerun, len(b.wake))
	}
}

func TestMergePatch(t *testing.T) {
	cur := &Task{ID: "a", Title: "old", Status: "todo", JiraStatus: map[string]string{"X-1": "Open", "X-2": "Open"}}
	next, err := mergePatch(cur, map[string]json.RawMessage{
		"title":      json.RawMessage(`"new"`),
		"jiraStatus": json.RawMessage(`{"X-2": "Done"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Title != "new" || next.Status != "todo" {
		t.Errorf("fields: got title %q status %q", next.Title, next.Status)
	}
	if want := map[string]string{"X-1": "Open", "X-2": "Done"}; !maps.Equal(next.JiraStatus, want) {
		t.Errorf("jiraStatus = %v, want %v (merged per key)", next.JiraStatus, want)
	}
	if _, err := mergePatch(&Task{}, map[string]json.RawMessage{"jiraStatus": json.RawMessage(`{"X-1": "Done"}`)}); err != nil {
		t.Errorf("a task with no jiraStatus yet: %v", err)
	}
	if _, err := mergePatch(cur, map[string]json.RawMessage{"jiraStatus": json.RawMessage(`"Done"`)}); err == nil {
		t.Error("a jiraStatus that is not a map was accepted")
	}
}

func TestHandleUpsertLevels(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, areas: []Area{{ID: "work", Name: "Work", Dir: dir}}}
	b.st.Tasks = map[string]*Task{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/tasks/{id}", b.handleUpsert)
	put := func(body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "PUT", "/api/tasks/t1", strings.NewReader(body)))
		return rec.Code
	}
	if code := put(`{"urgency": "high"}`); code != 400 {
		t.Fatalf("a new task with no area: %d, want 400", code)
	}
	if code := put(`{"area": "work", "urgency": "high", "importance": "low"}`); code != 200 {
		t.Fatalf("valid levels: %d", code)
	}
	if code := put(`{"urgency": "asap"}`); code != 400 || b.st.Tasks["t1"].Urgency != "high" {
		t.Errorf("unknown urgency: %d, urgency %q; want 400 and unchanged", code, b.st.Tasks["t1"].Urgency)
	}
	if code := put(`{"importance": ""}`); code != 200 || b.st.Tasks["t1"].Importance != "medium" {
		t.Errorf("clearing importance: %d, %q; want medium", code, b.st.Tasks["t1"].Importance)
	}
	if code := put(`{"group": "urgent"}`); code != 400 {
		t.Errorf("group urgent: %d, want 400", code)
	}
}

func TestSpawnSessionTerminal(t *testing.T) {
	dir := t.TempDir()
	// The terminal gets the session as its argv, run in the task's folder.
	b := &Board{claude: "claude", terminal: []string{"sh", "-c", `printf '%s\n' "$PWD" "$@" > args.tmp && mv args.tmp args`, "sh"}}
	pane, err := b.spawnSession(dir, "s1", "do it")
	if err != nil || pane != "" {
		t.Fatalf("spawnSession = %q, %v; want no pane, no error", pane, err)
	}
	var out []byte
	for range 100 {
		if out, err = os.ReadFile(filepath.Join(dir, "args")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	want := dir + "\nzsh\n-lic\nexec \"$0\" \"$@\"\nclaude\n--permission-mode\nauto\n--remote-control\ns1\n-n\ns1\ndo it\n"
	if string(out) != want {
		t.Errorf("terminal got\n%s\nwant\n%s", out, want)
	}
}

func TestHandleAttach(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{"t1": {ID: "t1"}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks/{id}/files", b.handleAttach)
	post := func(id string, files map[string]string) int {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for name, content := range files {
			fw, err := mw.CreateFormFile("file", name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write([]byte(content))
		}
		_ = mw.Close()
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/tasks/"+id+"/files", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("t1", map[string]string{"a.txt": "one"}); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if code := post("t1", map[string]string{"a.txt": "two", "../b.txt": "b"}); code != 200 {
		t.Fatalf("second upload: %d", code)
	}
	want := []string{filepath.Join(dir, "files", "t1", "a.txt"), filepath.Join(dir, "files", "t1", "b.txt")}
	if got := b.st.Tasks["t1"].Files; !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("Files = %v, want %v", got, want)
	}
	if data, _ := os.ReadFile(want[0]); string(data) != "two" {
		t.Errorf("a.txt = %q, want the replacement", data)
	}
	if code := post("nope", map[string]string{"a.txt": "x"}); code != 404 {
		t.Errorf("unknown task: %d, want 404", code)
	}
	if code := post("t1", map[string]string{"..": "x"}); code != 400 {
		t.Errorf("name ..: %d, want 400", code)
	}
}

func TestReapSessions(t *testing.T) {
	// The mux lists pane 7 only.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "wezterm"), []byte("#!/bin/sh\necho '[{\"pane_id\":7}]'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{
		"live": {ID: "live", Job: &Job{Action: "act", Status: "running", Session: "s1", Pane: "7"}},
		"dead": {ID: "dead", Job: &Job{Action: "act", Status: "running", Session: "s2", Pane: "17"}},
	}
	b.reapSessions()
	if got := b.st.Tasks["live"].Job.Status; got != "running" {
		t.Errorf("live pane's job = %q, want running", got)
	}
	if got := b.st.Tasks["dead"].Job.Status; got != "failed" {
		t.Errorf("gone pane's job = %q, want failed", got)
	}
	if _, err := os.Stat(b.path); err != nil {
		t.Errorf("reaping did not save: %v", err)
	}
}

func TestGhTasks(t *testing.T) {
	b := &Board{st: State{Tasks: map[string]*Task{
		"pr":     {ID: "pr", Links: []map[string]string{{"url": "https://github.com/o/r/pull/7#discussion_r1"}}},
		"jira":   {ID: "jira", Jira: []string{"TM-2"}},
		"pr70":   {ID: "pr70", Links: []map[string]string{{"url": "https://github.com/o/r/pull/70"}}},
		"other":  {ID: "other", Jira: []string{"TM-20"}},
		"issue7": {ID: "issue7", Links: []map[string]string{{"url": "https://github.com/o/r/issues/7"}}},
	}}}
	n := &ghNotification{}
	n.Subject.URL = "https://api.github.com/repos/o/r/pulls/7"
	n.Subject.Title = "TM-2: fix it"
	var ids []string
	for _, t := range b.ghTasks(n, regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)) {
		ids = append(ids, t.ID)
	}
	slices.Sort(ids)
	if want := []string{"jira", "pr"}; !slices.Equal(ids, want) {
		t.Errorf("ghTasks = %v, want %v", ids, want)
	}
}

func TestGhStatus(t *testing.T) {
	status, etag, err := ghStatus([]byte("HTTP/2.0 304 Not Modified\r\nEtag: W/\"abc\"\r\nX-Poll-Interval: 60\r\n\r\n"))
	if err != nil || status != http.StatusNotModified || etag != `W/"abc"` {
		t.Errorf("ghStatus = %d %q %v, want 304 W/\"abc\"", status, etag, err)
	}
}

func TestTaskPulls(t *testing.T) {
	task := &Task{Links: []map[string]string{
		{"url": "https://github.com/o/r/pull/7#discussion_r1"},
		{"url": "https://github.com/o/r/pull/7/files"},
		{"url": "https://github.com/o/r/issues/8"},
		{"url": "https://github.com/o/s/pull/9"},
	}}
	if got, want := taskPulls(task), []string{"https://github.com/o/r/pull/7", "https://github.com/o/s/pull/9"}; !slices.Equal(got, want) {
		t.Errorf("taskPulls = %v, want %v", got, want)
	}
}

func TestPullsDone(t *testing.T) {
	merged, closed, open := ghPull{State: "closed", Merged: true}, ghPull{State: "closed"}, ghPull{State: "open"}
	for _, c := range []struct {
		pulls []ghPull
		want  bool
	}{
		{[]ghPull{merged}, true},
		{[]ghPull{merged, closed}, true},
		{[]ghPull{merged, open}, false},
		{[]ghPull{closed}, false},
		{nil, false},
	} {
		if got := pullsDone(c.pulls); got != c.want {
			t.Errorf("pullsDone(%v) = %v, want %v", c.pulls, got, c.want)
		}
	}
}

// gitRepo is a fresh repo with one commit, and tree, which makes .worktrees/name on a new branch name
// with one commit and returns its path and tip (the merged head).
func gitRepo(t *testing.T) (repo string, run func(dir string, args ...string) string, tree func(name string) (string, string)) {
	t.Helper()
	// Run as a git hook (pre-push), GIT_DIR and friends would point every git call at the real repo.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			t.Setenv(k, "") // restores it after the test
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@t")
	}
	run = func(dir string, args ...string) string {
		t.Helper()
		out, err := git(dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	repo = t.TempDir()
	run(repo, "init", "-q", "-b", "main")
	run(repo, "commit", "-q", "--allow-empty", "-m", "init")
	tree = func(name string) (string, string) {
		path := filepath.Join(repo, ".worktrees", name)
		run(repo, "worktree", "add", "-q", path, "-b", name)
		run(path, "commit", "-q", "--allow-empty", "-m", name)
		return path, run(path, "rev-parse", "HEAD")
	}
	return repo, run, tree
}

func TestPruneWorktree(t *testing.T) {
	repo, run, tree := gitRepo(t)

	path, sha := tree("merged")
	if got, err := pruneWorktree(repo, "merged", sha); err != nil || got != "removed .worktrees/merged and branch merged, merged" {
		t.Errorf("merged: %q, %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("merged worktree still there: %v", err)
	}
	if _, err := git(repo, "rev-parse", "--verify", "refs/heads/merged"); err == nil {
		t.Error("merged branch still there")
	}

	path, sha = tree("dirty")
	if err := os.WriteFile(filepath.Join(path, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ahead, aheadSHA := tree("ahead")
	run(ahead, "commit", "-q", "--allow-empty", "-m", "after the merge")
	locked, lockedSHA := tree("locked")
	run(repo, "worktree", "lock", locked)
	busy, busySHA := tree("busy")
	sleep := exec.CommandContext(t.Context(), "sleep", "30")
	sleep.Dir = busy
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleep.Process.Kill(); _ = sleep.Wait() }()

	for _, c := range []struct{ branch, sha, want string }{
		{"dirty", sha, "kept .worktrees/dirty: it has uncommitted changes"},
		{"ahead", aheadSHA, "kept .worktrees/ahead: ahead has moved on from the merged head"},
		{"locked", lockedSHA, "kept .worktrees/locked: it is locked"},
		{"busy", busySHA, "kept .worktrees/busy: a process (a session?) is running in it"},
		{"main", "", ""}, // the main checkout is never one
		{"nowhere", "", ""},
	} {
		if got, err := pruneWorktree(repo, c.branch, c.sha); err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.branch, got, err, c.want)
		}
	}
	for _, p := range []string{path, ahead, locked, busy} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("kept worktree gone: %v", err)
		}
	}
}

func TestCleanMerged(t *testing.T) {
	repo, _, tree := gitRepo(t)
	path, sha := tree("feature")
	// The session's shell sits in the worktree until its pane is killed.
	shell := exec.CommandContext(t.Context(), "sleep", "30")
	shell.Dir = path
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = shell.Wait() }()
	defer func() { _ = shell.Process.Kill() }()

	bin, dir := t.TempDir(), t.TempDir()
	killed := filepath.Join(dir, "killed")
	for name, script := range map[string]string{
		"gh":      `echo '{"state":"closed","merged":true,"head":{"ref":"feature","sha":"` + sha + `"}}'`,
		"wezterm": `case "$2" in list) echo '[{"pane_id":7}]';; kill-pane) echo "$@" >` + killed + `; kill ` + strconv.Itoa(shell.Process.Pid) + `;; esac`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{"t": {ID: "t", Job: &Job{Status: "ok", Pane: "7"}}}

	b.cleanMerged("t", repo, "7", []string{"https://github.com/o/r/pull/1"})
	if got, _ := os.ReadFile(killed); string(got) != "cli kill-pane --pane-id 7\n" {
		t.Errorf("wezterm got %q, want the pane killed", got)
	}
	var notes []string
	for _, e := range b.st.Tasks["t"].History {
		notes = append(notes, e.Text)
	}
	if want := []string{
		"closed its session (wezterm pane 7): its pull request is merged",
		"removed .worktrees/feature and branch feature, merged",
	}; !slices.Equal(notes, want) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
}
