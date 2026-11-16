package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
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
