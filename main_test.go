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
	if p, ids := b.roadmapPrompt([]string{"work", "plans"}); !strings.Contains(p, `area "plans": `+plan) || strings.Contains(p, `area "work"`) || !slices.Equal(ids, []string{"plans"}) {
		t.Errorf("roadmapPrompt lists the wrong files, for %v:\n%s", ids, p)
	}
	// Only the areas asked for.
	if p, ids := b.roadmapPrompt([]string{"kith"}); strings.Contains(p, `area "plans"`) || !slices.Equal(ids, []string{"kith"}) {
		t.Errorf("roadmapPrompt(kith) for %v:\n%s", ids, p)
	}
	if p, ids := b.roadmapPrompt([]string{"work"}); p != "" || ids != nil {
		t.Errorf("roadmapPrompt with no files = %q, %v; want none", p, ids)
	}
}

func TestAreas(t *testing.T) {
	work, kith := t.TempDir(), t.TempDir()
	b := &Board{home: "/home"}
	t.Setenv("BOARD_BRIEF_EVERY", "6h") // an area without briefEvery's
	areas, err := b.parseAreas(`[{"id": "work", "name": "Work", "dir": "` + work + `"}, {"id": "kith", "name": "Kith", "dir": "` + kith + `", "briefEvery": "1h"},
		{"id": "nixos", "dir": "~/dev/nixos"}, {"id": "personal", "name": "Personal", "dir": "~", "briefEvery": "0"}]`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Area{{"work", "Work", work, defaultRoadmap, "", "", 6 * time.Hour}, {"kith", "Kith", kith, defaultRoadmap, "", "1h", time.Hour},
		{"nixos", "nixos", "/home/dev/nixos", defaultRoadmap, "", "", 6 * time.Hour}, {"personal", "Personal", "/home", defaultRoadmap, "", "0", 0}}
	if !reflect.DeepEqual(areas, want) {
		t.Errorf("parseAreas = %v, want %v", areas, want)
	}
	for _, bad := range []string{``, `{}`, `[{"id": "Kith", "dir": "/"}]`, `[{"id": "work", "dir": "/"}, {"id": "work", "dir": "/"}]`,
		`[{"name": "no id", "dir": "/"}]`, `[{"id": "work"}]`, `[{"id": "kith", "dir": "/"}]`,
		`[{"id": "work", "dir": "/", "briefEvery": "daily"}]`} {
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
	// A task saved before areas and levels were required loads as work, medium; the gone groups as levels and status:
	// "urgent" high urgency, "later" low, "waiting_on_others" an open task waiting (a closed one stays closed).
	b.path = filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(b.path, []byte(`{"tasks": {"old": {"id": "old"}, "k": {"id": "k", "area": "kith"},
		"u": {"id": "u", "group": "urgent"}, "ul": {"id": "ul", "group": "urgent", "urgency": "low"},
		"l": {"id": "l", "group": "later"}, "w": {"id": "w", "group": "waiting_on_others", "status": "todo"},
		"wd": {"id": "wd", "group": "waiting_on_others", "status": "done"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	if b.st.Tasks["old"].Area != "work" || b.st.Tasks["k"].Area != "kith" {
		t.Errorf("load: areas %q, %q; want work, kith", b.st.Tasks["old"].Area, b.st.Tasks["k"].Area)
	}
	for id, want := range map[string]string{"old": "medium", "u": "high", "ul": "low", "l": "low", "w": "medium"} {
		if got := b.st.Tasks[id]; got.Urgency != want || got.Importance != "medium" || got.Group != "" {
			t.Errorf("load %s: urgency %q, importance %q, group %q; want %s, medium, none", id, got.Urgency, got.Importance, got.Group, want)
		}
	}
	if w, wd := b.st.Tasks["w"], b.st.Tasks["wd"]; w.Status != "waiting" || wd.Status != "done" || wd.Group != "" {
		t.Errorf("load waiting_on_others: status %q, %q, group %q; want waiting, done, none", w.Status, wd.Status, wd.Group)
	}
}

func TestKithBack(t *testing.T) {
	dir := t.TempDir()
	lastOK := time.Now()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, wake: make(chan struct{}, 1),
		areas: []Area{{ID: "work"}, {ID: "personal"}}}
	b.st.Briefs = map[string]*Job{"work": {StartedAt: lastOK.Add(-time.Hour)}}
	if b.kithBack(lastOK) || len(b.rerun) != 0 {
		t.Error("a brief from before the outage was re-run")
	}
	b.st.Briefs["personal"] = &Job{StartedAt: lastOK.Add(time.Minute)} // ran while kith was down
	if !b.kithBack(lastOK) || !maps.Equal(b.rerun, map[string]bool{"personal": true}) || len(b.wake) != 1 {
		t.Errorf("re-run %v; want only the brief from during the outage", b.rerun)
	}
	b.kithBack(lastOK) // a second nudge must not block on the full channel
}

func TestHandleBriefRun(t *testing.T) {
	kith := t.TempDir()
	if err := os.WriteFile(filepath.Join(kith, "roadmap.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Board{wake: make(chan struct{}, 1), areas: []Area{{ID: "work"}, {ID: "kith", Dir: kith, Roadmap: defaultRoadmap},
		{ID: "nixos", Dir: t.TempDir(), Roadmap: defaultRoadmap}}}
	run := func(body string) int {
		rec := httptest.NewRecorder()
		b.handleBriefRun(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/api/brief/run", strings.NewReader(body)))
		return rec.Code
	}
	for body, want := range map[string]int{``: 400, `{"areas": []}`: 400, `{"areas": ["gone"]}`: 400, `{"areas": ["nixos"]}`: 409} {
		if code := run(body); code != want || len(b.rerun) != 0 {
			t.Errorf("%s: %d, rerun %v; want %d and no run", body, code, b.rerun, want)
		}
	}
	b.st.Briefs = map[string]*Job{"work": {Status: "running"}}
	if code := run(`{"areas": ["work"]}`); code != 409 || len(b.rerun) != 0 {
		t.Errorf("while running: %d, rerun %v; want 409 and no run", code, b.rerun)
	}
	// Only the areas asked for, and of them only those with a brief that isn't running.
	if code := run(`{"areas": ["work", "kith", "nixos"]}`); code != 202 || !maps.Equal(b.rerun, map[string]bool{"kith": true}) || len(b.wake) != 1 {
		t.Errorf("run: %d, rerun %v, wake %d; want 202 and kith nudged", code, b.rerun, len(b.wake))
	}
}

func TestDueBriefs(t *testing.T) {
	kith := t.TempDir()
	if err := os.WriteFile(filepath.Join(kith, "roadmap.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	b := &Board{areas: []Area{{ID: "work", every: 6 * time.Hour}, {ID: "personal", every: time.Hour}, {ID: "kith", Dir: kith, Roadmap: defaultRoadmap},
		{ID: "nixos", Dir: t.TempDir(), Roadmap: defaultRoadmap, every: time.Hour}}}
	b.st.Brief = map[string]any{"at": now.Add(-time.Hour).Format(time.RFC3339)} // the brief work pushed counts as its last
	b.st.Briefs = map[string]*Job{"personal": {StartedAt: now.Add(-2 * time.Hour)}}
	if got := b.dueBriefs(); !slices.Equal(got, []string{"personal"}) {
		t.Errorf("on schedule: %v, want personal", got)
	}
	// kith has no schedule but is named; nixos has nothing to brief; a running one's name waits for it to end.
	b.st.Briefs["personal"].Status = "running"
	b.briefNow("kith", "nixos", "personal")
	if got := b.dueBriefs(); !slices.Equal(got, []string{"kith"}) || !maps.Equal(b.rerun, map[string]bool{"personal": true}) {
		t.Errorf("named: %v, left %v; want kith, personal waiting", got, b.rerun)
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
	for _, g := range []string{"urgent", "later", "waiting_on_others"} {
		if code := put(`{"group": "` + g + `"}`); code != 400 {
			t.Errorf("group %s: %d, want 400", g, code)
		}
	}
	for trig, want := range map[string]int{
		`{"on": "main", "do": "x"}`:                 200,
		`{"jiraStatus": "Done", "do": "x"}`:         200,
		`{"at": "2026-11-18T09:00:00Z", "do": "x"}`: 200,
		`{"do": "x"}`:    400,
		`{"on": "main"}`: 400,
		`{"on": "main", "at": "2026-11-18T09:00:00Z", "do": "x"}`: 400,
	} {
		if code := put(`{"triggers": [` + trig + `]}`); code != want {
			t.Errorf("trigger %s: %d, want %d", trig, code, want)
		}
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
	// The key brought pull request 7 to "jira", so it is jira's now; "pr" linked it already.
	if got := taskPulls(b.st.Tasks["jira"]); !slices.Equal(got, []string{"https://github.com/o/r/pull/7"}) || len(b.st.Tasks["pr"].Links) != 1 {
		t.Errorf("jira's pulls = %v, pr's links = %v", got, b.st.Tasks["pr"].Links)
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

func TestLinkPulls(t *testing.T) {
	task := &Task{Links: []map[string]string{{"label": "PR #7", "url": "https://github.com/o/r/pull/7#discussion_r1"}}}
	linkPulls(task, "Opened #9 https://github.com/o/r/pull/9, review on https://github.com/o/r/pull/7/files and https://github.com/o/r/pull/9.")
	want := []map[string]string{task.Links[0], {"label": "PR #9", "url": "https://github.com/o/r/pull/9"}}
	if !reflect.DeepEqual(task.Links, want) {
		t.Errorf("links = %v, want %v", task.Links, want)
	}
}

func TestEndsWithPulls(t *testing.T) {
	for w, want := range map[string]bool{
		"": true, "PR review": true, "PR #27 review": true, "pull request merge": true,
		"prod deploy of release #1034, then re-time a finished match": false,
		"Bar/Polina: OK to pay + #1024 re-review":                     false,
	} {
		if got := endsWithPulls(w); got != want {
			t.Errorf("endsWithPulls(%q) = %v, want %v", w, got, want)
		}
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

func TestPullsShipped(t *testing.T) {
	onMain, onDev, closed := ghPull{Merged: true, Shipped: true}, ghPull{Merged: true}, ghPull{State: "closed"}
	for _, c := range []struct {
		pulls []ghPull
		want  bool
	}{
		{[]ghPull{onMain, closed}, true},
		{[]ghPull{onMain, onDev}, false},
	} {
		if got := pullsShipped(c.pulls); got != c.want {
			t.Errorf("pullsShipped(%v) = %v, want %v", c.pulls, got, c.want)
		}
	}
}

func TestReleased(t *testing.T) {
	p := ghPull{MergedAt: "2026-11-16T18:10:27Z"}
	before, after, open := ghPull{MergedAt: "2026-11-16T17:00:00Z"}, ghPull{MergedAt: "2026-11-16T18:48:30Z"}, ghPull{}
	for _, c := range []struct {
		releases []ghPull
		want     bool
	}{
		{[]ghPull{before, open}, false},
		{[]ghPull{before, after}, true},
		{nil, false},
	} {
		if got := released(&p, c.releases); got != c.want {
			t.Errorf("released(%v) = %v, want %v", c.releases, got, c.want)
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

func TestFullNames(t *testing.T) {
	b := &Board{}
	b.st.Tasks = map[string]*Task{"a": {Who: "Polina Shapiro"}, "b": {Who: "Bar Worcel"}, "c": {Who: "Sagi Chizik, Sagi Levi"}}
	for in, want := range map[string]string{
		"Polina":                  "Polina Shapiro",
		"polina":                  "Polina Shapiro",
		"Bar + Polina":            "Bar Worcel, Polina Shapiro",
		"Bar/Polina Shapiro":      "Bar Worcel, Polina Shapiro",
		"Bar and Polina, Bar":     "Bar Worcel, Polina Shapiro",
		"Evgeny":                  "Evgeny",
		"Evgeny Subotin & Evgeny": "Evgeny Subotin",
		"Sagi":                    "Sagi",
		"  ":                      "",
	} {
		task := &Task{Who: in}
		if b.fullNames(task); task.Who != want {
			t.Errorf("%q: %q, want %q", in, task.Who, want)
		}
	}
}

func TestFireTriggers(t *testing.T) {
	// Pull request 1 merged into development; release 9 into main came after it and carries its merge commit, release 8 came before.
	bin, dir := t.TempDir(), t.TempDir()
	gh := `case "$1" in
*/pulls/1) echo '{"number":1,"merged":true,"merged_at":"2026-11-16T18:00:00Z","merge_commit_sha":"abc","base":{"ref":"development"}}';;
*/pulls\?*base=main) echo '[{"number":9,"merged_at":"2026-11-16T19:00:00Z"},{"number":8,"merged_at":"2026-11-16T17:00:00Z"}]';;
*/pulls\?*) echo '[]';;
*/pulls/9/commits*) echo '[{"sha":"abc"}]';;
*/pulls/8/commits*) echo '[{"sha":"old"}]';;
*) exit 1;;
esac`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nshift\n"+gh+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for branch, want := range map[string]bool{"development": true, "main": true, "staging": false} {
		if got, err := onBranch("https://github.com/o/r/pull/1", branch); got != want || err != nil {
			t.Errorf("onBranch(%s) = %v, %v; want %v", branch, got, err, want)
		}
	}

	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, claude: "claude", terminal: []string{"true"},
		areas: []Area{{ID: "work", Dir: dir}}}
	pr := []map[string]string{{"url": "https://github.com/o/r/pull/1"}}
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	b.st.Tasks = map[string]*Task{
		"dev":     {ID: "dev", Area: "work", Status: "done", Links: pr, Triggers: []Trigger{{On: "development", Do: "tell Bar"}, {On: "staging", Do: "later"}}},
		"dropped": {ID: "dropped", Area: "work", Status: "dropped", Links: pr, Triggers: []Trigger{{On: "main", Do: "no"}}},
		"busy":    {ID: "busy", Area: "work", Links: pr, Triggers: []Trigger{{On: "main", Do: "wait"}}, Job: &Job{Status: "running"}},
		// Neither needs GitHub: a time that came and one that hasn't, a Jira status reached and one that isn't.
		"at":   {ID: "at", Area: "work", Triggers: []Trigger{{At: &past, Do: "ping"}, {At: &future, Do: "not yet"}}},
		"jira": {ID: "jira", Area: "work", Jira: []string{"BE-1", "BE-2"}, JiraStatus: map[string]string{"BE-2": "QA - Staging"}, Triggers: []Trigger{{JiraStatus: "Done", Do: "no"}, {JiraStatus: "qa - staging", Do: "tell QA"}}},
	}
	b.fireTriggers(false)
	if b.st.Tasks["dev"].Triggers[0].Fired != nil {
		t.Error("a branch trigger fired without GitHub")
	}
	if at := b.st.Tasks["at"]; at.Triggers[0].Fired == nil || at.Triggers[1].Fired != nil || at.History[0].Actor != "board" {
		t.Errorf("at = %+v, want only the past time fired, by the board", at)
	}
	if j := b.st.Tasks["jira"]; j.Triggers[0].Fired != nil || j.Triggers[1].Fired == nil || j.History[0].Text != "BE-2 reached QA - Staging; do: tell QA" {
		t.Errorf("jira = %+v, want the QA trigger fired", j)
	}
	b.fireTriggers(true)
	dev := b.st.Tasks["dev"]
	if dev.Triggers[0].Fired == nil || dev.Triggers[1].Fired != nil {
		t.Errorf("dev triggers = %+v, want only development fired", dev.Triggers)
	}
	if dev.Job == nil || dev.Job.Action != "trigger" || dev.Job.Status != "running" {
		t.Errorf("dev job = %+v, want a running trigger session", dev.Job)
	}
	if e := dev.History[0]; e.Kind != "trigger" || e.Text != "a pull request it links reached development; do: tell Bar" {
		t.Errorf("dev note = %+v", e)
	}
	if b.st.Tasks["dropped"].Triggers[0].Fired != nil || b.st.Tasks["busy"].Triggers[0].Fired != nil {
		t.Error("a dropped or busy task's trigger fired")
	}
}

func TestSaveSettings(t *testing.T) {
	dir := t.TempDir()
	for _, k := range settingKeys { // not the live service's env
		t.Setenv(k, "")
	}
	areas := `[{"id": "work", "dir": "` + dir + `"}]`
	t.Setenv("BOARD_AREAS", areas)
	b := &Board{path: filepath.Join(dir, "state.json"), triage: make(chan struct{}, 1), restart: make(chan struct{}, 1),
		env: map[string]string{"BOARD_AREAS": areas, "BOARD_PR_URL": "https://env/pull/"}}
	save := func(body string) int {
		rec := httptest.NewRecorder()
		b.handleSaveSettings(rec, httptest.NewRequestWithContext(t.Context(), "PUT", "/api/settings", strings.NewReader(body)))
		return rec.Code
	}
	for _, body := range []string{`{"BOARD_JIRA_POLL": "soon"}`, `{"BOARD_ADDR": ":1"}`, `{"BOARD_AREAS": "[]"}`,
		`{"BOARD_AREAS": "[{\"id\": \"work\", \"dir\": \"/nonexistent\"}]"}`, `{"BOARD_ISSUE_KEY": "("}`, `{"BOARD_SESSION_IN": "pane"}`} {
		if code := save(body); code != 400 {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	b.st.Briefs = map[string]*Job{"work": {Status: "running"}}
	if code := save(`{"BOARD_GITHUB_POLL": "5m"}`); code != 409 {
		t.Errorf("while a brief runs: %d, want 409", code)
	}
	if _, err := os.Stat(settingsPath(b.path)); !os.IsNotExist(err) || len(b.restart) != 0 {
		t.Fatalf("a refused save wrote settings.json (%v) or restarted (%d)", err, len(b.restart))
	}
	b.st.Briefs = nil
	// Only what differs from the env is kept; the PR link set back to the env's drops its override.
	if code := save(`{"BOARD_GITHUB_POLL": " 5m ", "BOARD_PR_URL": "https://env/pull/"}`); code != 202 || len(b.restart) != 1 {
		t.Fatalf("save: %d, restart %d; want 202 and a restart", code, len(b.restart))
	}
	data, err := os.ReadFile(settingsPath(b.path))
	var got map[string]string
	if err != nil || json.Unmarshal(data, &got) != nil || !maps.Equal(got, map[string]string{"BOARD_GITHUB_POLL": "5m"}) {
		t.Fatalf("settings.json %s (%v); want only the GitHub poll", data, err)
	}
	if before := applySettings(settingsPath(b.path)); before["BOARD_GITHUB_POLL"] != "" || os.Getenv("BOARD_GITHUB_POLL") != "5m" {
		t.Errorf("BOARD_GITHUB_POLL %q, before %q; want 5m over the env's empty", os.Getenv("BOARD_GITHUB_POLL"), before["BOARD_GITHUB_POLL"])
	}
}
