package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

func TestDirFor(t *testing.T) {
	projects := t.TempDir()
	if err := os.Mkdir(filepath.Join(projects, "kith"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := &Board{workdir: "/work", home: "/home", projects: projects}
	for area, want := range map[string]string{
		"":         "/work",
		"work":     "/work",
		"personal": "/home",
		"kith":     filepath.Join(projects, "kith"),
		"gone":     "/home", // a project with no folder runs at home
	} {
		if got := b.dirFor(&Task{Area: area}); got != want {
			t.Errorf("dirFor(%q) = %s, want %s", area, got, want)
		}
	}
	for _, bad := range []string{"..", "../etc", "a/b", "Kith", ""} {
		if areaRe.MatchString(bad) {
			t.Errorf("areaRe accepts %q", bad)
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
