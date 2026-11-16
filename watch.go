package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// mcpConn speaks newline-delimited JSON-RPC to a stdio MCP server, for one tool call.
type mcpConn struct {
	in   io.Writer
	sc   *bufio.Scanner
	tool string
}

func (c *mcpConn) send(msg map[string]any) error {
	msg["jsonrpc"] = "2.0"
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("%s: %w", c.tool, err)
	}
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("%s: send: %w", c.tool, err)
	}
	return nil
}

func (c *mcpConn) await(id int) (json.RawMessage, error) {
	for c.sc.Scan() {
		var r struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(c.sc.Bytes(), &r) != nil || r.ID == nil || *r.ID != id {
			continue
		}
		if r.Error != nil {
			return nil, fmt.Errorf("%s: %s", c.tool, r.Error.Message)
		}
		return r.Result, nil
	}
	if err := c.sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: server closed: %w", c.tool, err)
	}
	return nil, fmt.Errorf("%s: server closed", c.tool)
}

// call initializes the session, then calls the tool with args and returns its result.
func (c *mcpConn) call(args map[string]any) (json.RawMessage, error) {
	if err := c.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "brief-board", "version": "0"}}}); err != nil {
		return nil, err
	}
	if _, err := c.await(1); err != nil {
		return nil, err
	}
	if err := c.send(map[string]any{"method": "notifications/initialized"}); err != nil {
		return nil, err
	}
	if err := c.send(map[string]any{"id": 2, "method": "tools/call", "params": map[string]any{"name": c.tool, "arguments": args}}); err != nil {
		return nil, err
	}
	return c.await(2)
}

// mcpCall runs one tool call against a fresh stdio MCP server (kith-mcp) and returns its text.
func mcpCall(server, tool string, args map[string]any) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, server)
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	if err = cmd.Start(); err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	// Once the answer is in, how the server exits after stdin closes changes nothing.
	defer func() { _ = cmd.Wait() }()
	defer in.Close()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	raw, err := (&mcpConn{in: in, sc: sc, tool: tool}).call(args)
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err = json.Unmarshal(raw, &res); err != nil || len(res.Content) == 0 {
		return "", fmt.Errorf("%s: unexpected result %s", tool, raw)
	}
	if res.IsError {
		return "", fmt.Errorf("%s: %s", tool, res.Content[0].Text)
	}
	return res.Content[0].Text, nil
}

type feedMessage struct {
	EventID string `json:"event_id"`
	Sent    string `json:"sent"`
	Body    string `json:"body"`
}

// watchJira polls an area's Jira feed room and hands each new update to Claude, per ticket.
func (b *Board) watchJira(server string, area Area, keyRe *regexp.Regexp, every time.Duration) {
	var lastOK time.Time
	failing := false
	for {
		if err := b.pollJira(server, area, keyRe); err != nil {
			log.Printf("jira feed %s: %v", area.ID, err)
			failing = true
		} else {
			if failing {
				b.kithBack(lastOK)
			}
			failing, lastOK = false, time.Now()
		}
		time.Sleep(every)
	}
}

// kithBack runs when kith reads work again (keyring unlocked, kithd back). A brief that started
// after the last good read may have run without chat, so it asks scheduleBrief to run it again.
func (b *Board) kithBack(lastOK time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.lastBrief().After(lastOK) {
		return false
	}
	b.logf("kith", "kith reads work again; re-running the brief that ran while it was down")
	b.saveOrLog()
	b.briefNow()
	return true
}

// briefNow asks scheduleBrief to run the brief now. The caller holds b.mu.
func (b *Board) briefNow() {
	b.rerun = true
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Board) pollJira(server string, area Area, keyRe *regexp.Regexp) error {
	text, err := mcpCall(server, "read_room", map[string]any{"room": area.JiraRoom, "limit": 50})
	if err != nil {
		return err
	}
	var read struct {
		Messages []feedMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(text), &read); err != nil {
		return fmt.Errorf("read_room answer: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	seen, ok := b.st.JiraSeen[area.JiraRoom]
	baseline := !ok
	ids := make([]string, 0, len(read.Messages))
	byKey := map[string][]string{}
	var keys []string
	for _, m := range read.Messages {
		ids = append(ids, m.EventID)
		if baseline || slices.Contains(seen, m.EventID) {
			continue
		}
		for _, key := range keyRe.FindAllString(m.Body, -1) {
			if _, ok := byKey[key]; !ok {
				keys = append(keys, key)
			}
			if !slices.Contains(byKey[key], m.Body) {
				byKey[key] = append(byKey[key], m.Body)
			}
		}
	}
	b.st.JiraSeen[area.JiraRoom] = ids
	for _, key := range keys {
		update := strings.Join(byKey[key], "\n")
		var tracked bool
		for _, t := range b.st.Tasks {
			if !slices.Contains(t.Jira, key) {
				continue
			}
			tracked = true
			b.note(t, "jira", "update", update)
			if !b.busy(t) {
				b.start(t, actRequest{Action: "update", Comment: update})
			}
		}
		if !tracked {
			go b.triageKey(area, key, update)
		}
	}
	return b.save()
}

// triageKey asks Claude, in the area's folder, whether an untracked ticket belongs on the board, one at a time.
func (b *Board) triageKey(area Area, key, update string) {
	b.triage <- struct{}{}
	defer func() { <-b.triage }()
	prompt := fmt.Sprintf(`A Jira update mentions %[1]s, which has no task on Eugene's Brief Board (%[2]s). Nobody is watching this run.

The update: %[3]s

Look %[1]s up in this folder's Jira. If it concerns Eugene (assigned to him, mentions or asks him, a ticket he reported,
commented on or is blocked by), add it to the board with area %[4]q, in the brief skill's format (step 5 of
.claude/skills/brief/SKILL.md in the work area's folder, %[5]s):
  curl -s -X PUT %[2]s/api/tasks/<slug> -H 'Content-Type: application/json' -d '{...}'
with the whole story in summary, jira, jiraStatus, and messages when chat is involved. Otherwise change nothing.
Never send messages and never change Jira. Your final message is one line: what you decided and why.`, key, b.selfURL, update, area.ID, b.workdir)
	status, text := runClaude(b.claude, area.Dir, prompt)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logf("triage", "%s (%s): %s", key, status, text)
	b.saveOrLog()
}

// scheduleBrief runs the brief skill whenever the last brief is older than every.
func (b *Board) scheduleBrief(every time.Duration) {
	for {
		b.mu.Lock()
		last := b.lastBrief()
		due := (time.Since(last) >= every || b.rerun) && (b.st.BriefRun == nil || b.st.BriefRun.Status != "running")
		if due {
			b.rerun = false
			b.st.BriefRun = &Job{Action: "brief", Status: "running", StartedAt: time.Now()}
			b.saveOrLog()
		}
		b.mu.Unlock()
		if due {
			status, text := runClaude(b.claude, b.workdir, `Use the brief skill. This is a scheduled run from the Brief Board and nobody is watching:
do the sweep, save the history file and sync the board (step 5), but take no actions and send nothing.
Your final message is the brief's opening paragraph.`)
			b.mu.Lock()
			now := time.Now()
			b.st.BriefRun.Status, b.st.BriefRun.EndedAt = status, &now
			b.logf("brief", "%s: %s", status, text)
			b.saveOrLog()
			b.mu.Unlock()

			// Mail, calendar and personal chats: a global skill, so it runs from home, not the work repo.
			status, text = runClaude(b.claude, b.home, `Use the sweep skill. This is a scheduled run from the Brief Board and nobody is watching:
sync the board, but take no actions and send nothing. Your final message is one line: what changed on the board.`)
			b.mu.Lock()
			b.logf("sweep", "%s: %s", status, text)
			b.saveOrLog()
			b.mu.Unlock()
		}
		select {
		case <-b.wake:
		case <-time.After(10 * time.Minute):
		}
	}
}

// lastBrief is when the last brief started: a scheduled run, or the brief Claude last pushed.
func (b *Board) lastBrief() time.Time {
	var last time.Time
	if b.st.BriefRun != nil {
		last = b.st.BriefRun.StartedAt
	}
	if at, ok := b.st.Brief["at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, at); err == nil && t.After(last) {
			last = t
		}
	}
	return last
}

// ghNotification is the part of a GitHub notification the board reads.
type ghNotification struct {
	Reason    string `json:"reason"`
	UpdatedAt string `json:"updated_at"`
	Subject   struct {
		Title            string `json:"title"`
		URL              string `json:"url"`
		LatestCommentURL string `json:"latest_comment_url"`
	} `json:"subject"`
}

// watchGitHub polls GitHub notifications every so often; never when every is 0.
func (b *Board) watchGitHub(keyRe *regexp.Regexp, every time.Duration) {
	if every <= 0 {
		return
	}
	log.Printf("reading GitHub notifications every %s", every)
	for ; ; time.Sleep(every) {
		if err := b.pollGitHub(keyRe); err != nil {
			log.Printf("github: %v", err)
		}
	}
}

// ghAPI is gh api's answer for args (the path last), as the gh user; what it printed comes back on an error too.
func ghAPI(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", append([]string{"api"}, args...)...).Output() //nolint:gosec // argv, no shell; GitHub's own urls
	if err != nil {
		return out, fmt.Errorf("gh api %s: %w", args[len(args)-1], err)
	}
	return out, nil
}

// pollGitHub notes each new GitHub notification on the tasks it is about (see ghTasks); no Claude runs.
func (b *Board) pollGitHub(keyRe *regexp.Regexp) error {
	b.mu.Lock()
	since := b.st.GitHubSince
	if since == "" {
		defer b.mu.Unlock()
		b.st.GitHubSince = time.Now().UTC().Format(time.RFC3339)
		return b.save()
	}
	b.mu.Unlock()
	// all=true: a notification already read on github.com still reaches the board.
	query := "notifications?all=true&since=" + since
	// A 304 to If-None-Match is free of GitHub's rate limit (If-Modified-Since gets a 200 here), so polling often costs nothing.
	head, err := ghAPI("-i", "-H", "If-None-Match: "+b.ghETag, query)
	status, etag, perr := ghStatus(head)
	if perr == nil && status == http.StatusNotModified { // gh exits 1 on a 304
		return nil
	}
	if err = errors.Join(err, perr); err != nil {
		return fmt.Errorf("pollGitHub: %w", err)
	}
	all, err := ghNotifications(query)
	if err != nil {
		return fmt.Errorf("pollGitHub: %w", err)
	}
	slices.SortFunc(all, func(x, y ghNotification) int { return strings.Compare(x.UpdatedAt, y.UpdatedAt) })
	newest := since
	type update struct {
		n    *ghNotification
		text string
	}
	var updates []update
	for i := range all {
		n := &all[i]
		// shortcut: a notification updated in the same second as the last poll's newest is skipped; dedupe by id if that ever bites.
		if n.UpdatedAt <= since || n.Subject.URL == "" {
			continue
		}
		newest = max(newest, n.UpdatedAt)
		updates = append(updates, update{n, ghUpdate(n)})
	}
	b.mu.Lock()
	var settle []settleCheck
	for _, u := range updates {
		for _, t := range b.ghTasks(u.n, keyRe) {
			b.note(t, "github", "update", u.text)
			if page := ghPageURL(u.n.Subject.URL); slices.Contains(taskPulls(t), page) {
				settle = append(settle, settleCheck{t.ID, page})
			}
		}
	}
	b.st.GitHubSince = newest
	b.ghETag = etag
	err = b.save()
	b.mu.Unlock()
	b.settleMerged(settle, since)
	return err
}

var pullRe = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/\d+`)

// taskPulls are the github.com pages of the pull requests t links, once each.
func taskPulls(t *Task) []string {
	var pulls []string
	for _, l := range t.Links {
		if p := pullRe.FindString(l["url"]); p != "" && !slices.Contains(pulls, p) {
			pulls = append(pulls, p)
		}
	}
	return pulls
}

// ghPull is the part of a pull request settleMerged and cleanMerged read.
type ghPull struct {
	State    string `json:"state"` // open | closed
	Merged   bool   `json:"merged"`
	ClosedAt string `json:"closed_at"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

// settleCheck is a task a notification reached through a pull request it links: that pull request's page.
type settleCheck struct{ id, page string }

// pullsDone is whether every pull request is closed and one of them merged; all closed
// unmerged (abandoned, or replaced by one the task doesn't link yet) leaves the task alone.
func pullsDone(pulls []ghPull) bool {
	merged := false
	for _, p := range pulls {
		if p.State != "closed" {
			return false
		}
		merged = merged || p.Merged
	}
	return merged
}

// settleMerged moves each open task to done once pullsDone holds for the pull requests it links, when the
// notification's own pull request closed after since: a reopened task, or one linking long-merged pull
// requests as context, stays open through later comments on them. No Claude runs.
func (b *Board) settleMerged(checks []settleCheck, since string) {
	for _, c := range checks {
		b.mu.Lock()
		var pages []string
		if t := b.st.Tasks[c.id]; t != nil && t.Status != "done" && t.Status != "dropped" {
			pages = taskPulls(t)
		}
		b.mu.Unlock()
		if len(pages) == 0 {
			continue
		}
		pulls := make([]ghPull, 0, len(pages))
		fresh := false
		for _, page := range pages {
			out, err := ghAPI(ghAPIURL(page))
			var p ghPull
			if err == nil {
				err = json.Unmarshal(out, &p)
			}
			if err != nil {
				log.Printf("github: settle %s: %v", c.id, err)
				break
			}
			pulls = append(pulls, p)
			fresh = fresh || page == c.page && p.ClosedAt > since
		}
		if !fresh || len(pulls) < len(pages) || !pullsDone(pulls) {
			continue
		}
		b.mu.Lock()
		// Re-checked: Eugene may have moved it while GitHub answered.
		if t := b.st.Tasks[c.id]; t != nil && t.Status != "done" && t.Status != "dropped" {
			b.note(t, "github", "status", t.Status+" → done: its pull requests are merged or closed")
			setStatus(t, "done")
			b.doneNow(t)
			b.saveOrLog()
		}
		b.mu.Unlock()
	}
}

// doneNow is t moving to done: with BOARD_CLEAN_MERGED, what its merged pull requests leave behind
// goes (see cleanMerged); callers hold mu.
func (b *Board) doneNow(t *Task) {
	pages := taskPulls(t)
	if !b.clean || len(pages) == 0 {
		return
	}
	pane := ""
	// A session still running has not reported yet: it may be mid-work, so it stays.
	if t.Job != nil && t.Job.Status != "running" {
		pane = t.Job.Pane
	}
	dir, err := b.dirFor(t)
	if err != nil {
		b.note(t, "board", "cleanup", "No cleanup: "+err.Error())
		return
	}
	go b.cleanMerged(t.ID, dir, pane, pages)
}

// cleanMerged runs once a task is done: if one of its pull requests (pages) is merged, it closes the
// task's session (its wezterm pane, "" for none), then runs pruneWorktree in dir for each merged one,
// noting on task id what it did.
func (b *Board) cleanMerged(id, dir, pane string, pages []string) {
	say := func(text string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if t := b.st.Tasks[id]; t != nil {
			b.note(t, "board", "cleanup", text)
			b.saveOrLog()
		}
	}
	var merged []ghPull
	for _, page := range pages {
		out, err := ghAPI(ghAPIURL(page))
		var p ghPull
		if err == nil {
			err = json.Unmarshal(out, &p)
		}
		if err != nil {
			log.Printf("github: clean %s: %v", id, err)
			continue
		}
		if p.Merged {
			merged = append(merged, p)
		}
	}
	if len(merged) == 0 {
		return
	}
	if pane != "" && paneAlive(pane) {
		if _, err := wezterm([]string{"cli", "kill-pane", "--pane-id", pane}); err != nil {
			log.Printf("github: clean %s: %v", id, err)
		} else {
			say("closed its session (wezterm pane " + pane + "): its pull request is merged")
			// shortcut: a session slower than this to exit keeps its worktree (noted as in use); poll inUse if that shows up.
			time.Sleep(time.Second)
		}
	}
	for _, p := range merged {
		text, err := pruneWorktree(dir, p.Head.Ref, p.Head.SHA)
		if err != nil {
			log.Printf("github: clean %s: %v", id, err)
		}
		if text != "" {
			say(text)
		}
	}
}

// pruneWorktree removes branch's worktree from dir/.worktrees/, and then branch, only when nothing in it
// can be lost or is in use: it is still at sha (the merged head), has nothing uncommitted, isn't locked and
// no process runs in it. It returns what it did for the task's history, "" when no worktree there has branch.
func pruneWorktree(dir, branch, sha string) (string, error) {
	dir, err := filepath.EvalSymlinks(dir) // git lists real paths
	if err != nil {
		return "", fmt.Errorf("pruneWorktree: %w", err)
	}
	path, head, locked, err := worktreeOf(dir, branch)
	if path == "" || err != nil {
		return "", err
	}
	name := ".worktrees/" + filepath.Base(path)
	status, err := git(path, "status", "--porcelain")
	switch {
	case err != nil:
		return "", fmt.Errorf("pruneWorktree: %w", err)
	case locked:
		return "kept " + name + ": it is locked", nil
	case head != sha:
		return "kept " + name + ": " + branch + " has moved on from the merged head", nil
	case status != "":
		return "kept " + name + ": it has uncommitted changes", nil
	case inUse(path):
		return "kept " + name + ": a process (a session?) is running in it", nil
	}
	// No --force: git refuses anything it would lose, should that change since the checks.
	if _, err := git(dir, "worktree", "remove", path); err != nil {
		return "", fmt.Errorf("pruneWorktree: %w", err)
	}
	if _, err := git(dir, "branch", "-D", branch); err != nil {
		return "removed " + name + ", merged; kept branch " + branch, fmt.Errorf("pruneWorktree: %w", err)
	}
	return "removed " + name + " and branch " + branch + ", merged", nil
}

// worktreeOf is the worktree directly in dir/.worktrees/ that has branch checked out: its path ("" when
// there is none), its HEAD and whether it is locked.
func worktreeOf(dir, branch string) (path, head string, locked bool, err error) {
	list, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", "", false, fmt.Errorf("worktreeOf: %w", err)
	}
	for block := range strings.SplitSeq(list, "\n\n") {
		path, head, locked = "", "", false
		on := false
		for line := range strings.SplitSeq(block, "\n") {
			k, v, _ := strings.Cut(line, " ")
			switch k {
			case "worktree":
				path = v
			case "HEAD":
				head = v
			case "branch":
				on = v == "refs/heads/"+branch
			case "locked":
				locked = true
			}
		}
		if on && filepath.Dir(path) == filepath.Join(dir, ".worktrees") {
			return path, head, locked, nil
		}
	}
	return "", "", false, nil
}

// inUse is whether some process's working folder is path or inside it; true when /proc can't tell.
func inUse(path string) bool {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, p := range procs {
		cwd, err := os.Readlink("/proc/" + p.Name() + "/cwd")
		if err == nil && (cwd == path || strings.HasPrefix(cwd, path+"/")) {
			return true
		}
	}
	return false
}

// git is git's output for args, run in dir; what it printed comes back on an error.
func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput() //nolint:gosec // argv, no shell; the board's own folders and GitHub's branch names
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ghAPIURL is the API url of a github.com page: github.com/o/r/pull/7 -> .../repos/o/r/pulls/7.
func ghAPIURL(page string) string {
	return strings.Replace(strings.Replace(page, "://github.com/", "://api.github.com/repos/", 1), "/pull/", "/pulls/", 1)
}

// ghNotifications reads every page of a notifications query.
func ghNotifications(query string) ([]ghNotification, error) {
	out, err := ghAPI("--paginate", query)
	if err != nil {
		return nil, err
	}
	var all []ghNotification
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); { // --paginate prints one array per page
		var page []ghNotification
		if err := dec.Decode(&page); err != nil {
			return nil, fmt.Errorf("ghNotifications: %w", err)
		}
		all = append(all, page...)
	}
	return all, nil
}

// ghStatus is the status and ETag of a gh api -i answer.
func ghStatus(out []byte) (int, string, error) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), nil)
	if err != nil {
		return 0, "", fmt.Errorf("ghStatus: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Etag"), nil
}

// ghPageURL is the github.com page of an API url: .../repos/o/r/pulls/7 -> github.com/o/r/pull/7.
func ghPageURL(api string) string {
	return strings.Replace(strings.Replace(api, "://api.github.com/repos/", "://github.com/", 1), "/pulls/", "/pull/", 1)
}

// ghTasks are the tasks a notification is about: those linking its pull request or issue, or tracking
// a Jira key its title names.
func (b *Board) ghTasks(n *ghNotification, keyRe *regexp.Regexp) []*Task {
	page := ghPageURL(n.Subject.URL)
	keys := keyRe.FindAllString(n.Subject.Title, -1)
	var tasks []*Task
	for _, t := range b.st.Tasks {
		linked := slices.ContainsFunc(t.Links, func(l map[string]string) bool {
			return l["url"] == page || strings.HasPrefix(l["url"], page+"/") || strings.HasPrefix(l["url"], page+"#")
		})
		if linked || slices.ContainsFunc(keys, func(k string) bool { return slices.Contains(t.Jira, k) }) {
			tasks = append(tasks, t)
		}
	}
	return tasks
}

// ghUpdate is the history line for a notification: why it came, the page, and what happened last,
// the newest comment or the pull request's state.
func ghUpdate(n *ghNotification) string {
	text := fmt.Sprintf("%s: %s %s", n.Reason, n.Subject.Title, ghPageURL(n.Subject.URL))
	if n.Subject.LatestCommentURL == "" {
		return text
	}
	out, err := ghAPI(n.Subject.LatestCommentURL)
	if err != nil {
		log.Printf("github: %v", err)
		return text
	}
	var last struct {
		User   struct{ Login string } `json:"user"`
		Body   string                 `json:"body"`
		State  string                 `json:"state"`
		Merged bool                   `json:"merged"`
	}
	if err := json.Unmarshal(out, &last); err != nil {
		log.Printf("github: %s: %v", n.Subject.LatestCommentURL, err)
		return text
	}
	if n.Subject.LatestCommentURL == n.Subject.URL { // no comment: the subject itself changed
		if last.Merged {
			last.State = "merged"
		}
		return text + "\nnow " + last.State
	}
	body := []rune(last.Body)
	if len(body) > 500 {
		body = append(body[:500], '…')
	}
	return text + "\n" + last.User.Login + ": " + string(body)
}

func envDuration(key string) time.Duration {
	d, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return 0
	}
	return d
}
