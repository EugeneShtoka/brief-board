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
