# brief-board

The daily brief as a task board, served over HTTP. Each task has buttons that open an interactive
[Claude Code](https://claude.com/claude-code) session on it, in the task's own project folder, to reply to
someone, act on the task or move its Jira ticket to QA. The session reports back to the board through its API.

One Go binary, standard library only; the whole page is `index.html`, embedded at build time.

## How it works

- **Tasks** live in one JSON state file. Each belongs to an *area*: a project folder its sessions run in.
  Every change is also appended to a weekly history log (`log/<monday>.jsonl` next to the state file).
- **Buttons** (`reply`, `act`, `qa`) start `claude` in that area's folder, in a new wezterm mux tab or window
  (or any terminal, see `BOARD_TERMINAL`), with the task as JSON and the rule for that action.
- **The brief** runs on a schedule per area (`briefEvery`) or from the page, and fills the board from the
  area's sources and roadmap files.
- **Watchers**, each optional:
  - Jira: reads the kith room where an area's Jira bot posts, and starts an `update` session for each changed
    ticket.
  - GitHub: reads notifications through `gh`, links pull requests to their tasks, and settles a task once its
    pull requests are merged.
  - Triggers: a task can fire an action when a branch ships, a Jira status is reached or a time comes.

## Requirements

- Go (the version in `go.mod`'s `toolchain` line, which `make` downloads)
- [`claude`](https://claude.com/claude-code) on `PATH`
- wezterm, unless `BOARD_TERMINAL` names another terminal
- Optional: `gh` (GitHub watcher), a kith MCP server (Jira watcher)

## Quick start

```sh
make build
BOARD_AREAS='[{"id":"board","name":"Brief Board","dir":"~/dev/brief-board"}]' ./brief-board
```

Then open <http://localhost:8787>. `make run` does the same from source.

## Configuration

Settings come from the environment. Those the page's settings form edits are saved in `settings.json`
next to the state file, which wins over the environment; saving them restarts the board.

| Variable | Default | Meaning |
| --- | --- | --- |
| `BOARD_AREAS` | required | JSON list of areas: `{"id", "name", "dir", "roadmap", "jiraRoom", "briefEvery", "issueUrl", "prUrl"}`; the settings form edits it field by field |
| `BOARD_ADDR` | `:8787` | Listen address |
| `BOARD_STATE` | `~/.local/state/brief-board/state.json` | State file; logs and `settings.json` live next to it |
| `BOARD_URL` | `http://localhost` + `BOARD_ADDR` | How sessions reach the board |
| `BOARD_CLAUDE` | `claude` | The Claude Code command |
| `BOARD_TERMINAL` | wezterm mux | A terminal command the session is appended to, e.g. `kitty`, `alacritty -e` |
| `BOARD_SESSION_IN` | `tab` | wezterm: a new `tab` in the last used window, or its own `window` |
| `BOARD_BRIEF_EVERY` | off | Default brief interval for areas without `briefEvery` (a Go duration) |
| `BOARD_JIRA_POLL` | `2m` | How often to read each area's `jiraRoom` |
| `BOARD_KITH_MCP` | `kith-mcp` | The kith MCP server command |
| `BOARD_GITHUB_POLL` | off | How often to read GitHub notifications |
| `BOARD_ISSUE_KEY` | `\b[A-Z][A-Z0-9]+-\d+\b` | Issue key pattern in the Jira feed and GitHub |
| `BOARD_ISSUE_URL`, `BOARD_PR_URL` | none | Link bases for issue keys and `#PR` numbers on the page, for areas without `issueUrl` / `prUrl`; an area without `prUrl` first takes its folder's GitHub `origin` |
| `BOARD_CLEAN_MERGED` | off | `1`: a task done with a merged pull request closes its session and removes the worktree |

The comment at the top of `main.go` documents each one in full.

## API

JSON over HTTP; a task update merges the fields it sends.

| Route | Does |
| --- | --- |
| `GET /api/state` | The board: brief, tasks, log, config |
| `GET /api/log` | History from the weekly logs |
| `PUT /api/tasks/{id}` | Create or update a task |
| `POST /api/tasks/{id}/notes` | Add a history entry (`actor`, `kind`, `text`) |
| `POST /api/tasks/{id}/files` | Attach files (multipart `file`) for the next session |
| `POST /api/tasks/{id}/act` | Start a session for an action |
| `PUT /api/brief`, `POST /api/brief/run` | Write the brief, run it now |
| `GET`/`PUT /api/settings` | Read or save settings |

## Development

| Command | Does |
| --- | --- |
| `make test` | Tests with the race detector |
| `make lint` / `make fmt` | golangci-lint / gofmt |
| `make check` | Every CI gate, with CI's toolchain and pinned tools |
| `make hooks` | Enable the pre-commit and pre-push hooks |
| `make deploy` | Build and install to `~/.local/bin` (no gates) |
