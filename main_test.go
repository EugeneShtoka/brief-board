package main

import (
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
