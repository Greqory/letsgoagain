# AGENTS.md — LetsGoAgain

Read this file first in every session. It is the project constitution: it wins over code comments and
any other document; only the user's chat instructions win over it.

## 1. Project

**LetsGoAgain** keeps OpenCode sessions from sleeping through a Claude usage limit. When a session stops
because the Claude Max 5-hour or weekly limit was hit, LetsGoAgain records the stop, waits until the
limit resets and then tells the same session to continue. "Let's go again!", every time.

It replaces `unsnooze` for one narrow setup, the user's, and does that one well:

- **OpenCode v2** (2.3.0 measured) is the only agent. No Claude Code, Codex or others.
- **Claude Max via the meridian proxy**: the OpenCode plugin `opencode-with-claude` starts meridian
  (`@rynfar/meridian`, 1.79.0 measured) on `127.0.0.1:3456`, and `~/.config/opencode/opencode.json` points
  the `anthropic` provider there. meridian drives Claude Code underneath.
- **Any terminal**: herdr (the usual one), tmux or a plain terminal. LetsGoAgain never reads or types into a
  terminal; it talks to OpenCode's own API.
- Linux, single user, single machine.

Out of scope: other agents, other providers, multiple machines, GUI, telemetry, getting around a limit
(LetsGoAgain only waits for the reset; it never sends a prompt early to "test" the limit).

## 2. Facts it is built on (measured 2026-11-13, sandbox: real OpenCode 2.3.0 + `tools/fakemeridian`)

- **OpenCode v2 runs one shared service** (`opencode serve --service`); every TUI is a client of it, and
  plugins and meridian run inside it. `~/.local/state/opencode/service.json` holds
  `{id, version, url, pid, password}`; the API takes HTTP Basic auth, user `opencode`, that password. The
  URL and password change when the service restarts.
- **meridian's limit error**: it classifies the limit as 429, but the stream has already started with
  `200 OK`, so it sends an SSE frame
  `event: error` / `{"type":"error","error":{"type":"rate_limit_error","message":"Claude Max rate limit reached. Wait a moment and try again.","retry_after":4868}}`.
  `retry_after` is seconds, clamped to 1…86400; **60 is meridian's default when it does not know the reset**.
- **OpenCode retries by itself, then gives up**: `session.retry.scheduled` ×10 over ~79 s (backoff capped
  near 10 s, `retry_after` ignored), then `session.step.failed` and **`session.execution.failed`** with
  `data.error.type = "provider.rate-limit"`, the meridian body as a string in `data.error.response.body`.
  That event is the stop signal. Fixtures: `internal/opencode/testdata/`.
- **After the stop**: `GET /api/session/{id}` shows `outcome: "failed"` and `time.idle`; the newest message
  (`GET /api/session/{id}/message?order=desc`) is `type: "idle"`, `outcome: "failed"`, same `error`.
- **Resuming works**: `POST /api/session/{id}/prompt` with `{"text": "…"}` on the failed session starts a
  new execution (`session.execution.started` → `succeeded`).
- **Subagents** are sessions with `parentID`; roots have none. A child's finished round wakes its parent.
- **Events**: `GET /api/event` (SSE), filter with repeated `types=` (a comma list is a 400); the stream
  starts with `server.connected`.
- **meridian's quota**: `GET http://127.0.0.1:3456/v1/usage/quota` (no auth) returns `buckets` of
  `{type: five_hour | seven_day | seven_day_fable, status: allowed | allowed_warning | rejected,
  utilization (0…1), resetsAt (epoch ms), observedAt}`. meridian sets `rejected` when utilization ≥ 1 or
  Claude Code reported the window rejected (that status can stick after the reset until the next request:
  judge by `resetsAt` too). Fixture: `internal/meridian/testdata/`.
- Why unsnooze failed: it scrapes the terminal for known banners, did not know meridian's, and the reset
  time is not on screen. It never caught a stop in two weeks.

## 3. Architecture

One Go program, `letsgoagain`, no OpenCode plugin.

- `letsgoagain daemon [--dry-run]`: runs as a systemd user service.
  1. Reads `service.json`, connects to the service, subscribes to
     `session.execution.failed|started|succeeded`. Reconnects (re-reading `service.json`) every 30 s when the
     service is down or restarted.
  2. On every (re)connect, scans recent root sessions with `outcome: "failed"` for limit stops it missed.
  3. A `session.execution.failed` with `provider.rate-limit` on a **root** session records a stop (child
     sessions are ignored: their parent fails too and is the one resumed).
  4. Checks the wall clock every 30 s (a laptop that slept through a reset resumes on wake); resumes due stops.
  5. `--dry-run` records and logs, sends nothing.
- `letsgoagain status`: tracked stops, reset time and its source, attempts.
- `letsgoagain resume-now [sessionID | --all]`: resume without waiting for the reset.
- `letsgoagain install` / `uninstall`: write, enable and remove the systemd user unit; idempotent.
- The CLI talks to the daemon over `$XDG_RUNTIME_DIR/letsgoagain.sock`; `status` falls back to reading the
  state file when the daemon is not running.

### Reset time, in order of trust
1. **meridian quota**: buckets that are exhausted (`rejected` or utilization ≥ 1) with `resetsAt` in the
   future → the latest of their `resetsAt`. Also says 5h vs weekly.
2. **`retry_after`** from the error body, counted from the stop, only when it is above 60 and below 86400
   (60 = meridian did not know, 86400 = clamped).
3. **Probing**: neither → ask the quota again every 15 min; at the latest 5 h after the stop, try once.

A reset time gets a 60 s margin before the resume.

### Resuming a due stop
1. Quota still shows an exhausted bucket with a future reset → reschedule to it; no attempt used.
2. The session must be unchanged since the stop: `outcome` still `"failed"`, `time.idle` equal to the stop's,
   not running (`GET /api/session/active`). Otherwise close the stop as "moved on" (someone else resumed it).
3. Send `Continue where you left off.` via `POST /api/session/{id}/prompt`. Count the attempt.
4. `session.execution.succeeded` closes the stop as done. Another limit failure on the same session before
   a success is the same stop again: reschedule; after 5 attempts mark it failed and stop trying.
5. The service unreachable at the due time → keep the stop and try on the next tick (the service may be
   restarting); a session that no longer exists (404) → "gone".

### State and files
- State: `~/.local/state/letsgoagain/state.json` (atomic: temp file + rename); log:
  `~/.local/state/letsgoagain/letsgoagain.log`.
- Socket: `$XDG_RUNTIME_DIR/letsgoagain.sock`.
- No config file until a setting needs one; flags with defaults first.

## 4. Still to verify

- [ ] That the resume message shows up live in the user's TUI (it goes through the shared service, as the
      TUI's own prompts do; the sandbox had no TUI attached).
- [ ] A real limit stop end to end (the next time the user hits one): event, reset source, resume.
- [ ] Whether a short burst limit also arrives as `rate_limit_error`; if so, a quota showing no exhausted
      bucket must not make LetsGoAgain wait 5 h (probing covers it: next check after 15 min).

## 5. Rules

### Always
- Tests with every behaviour change. Pure logic (reset calculation, state transitions, payload parsing)
  is plain Go with table tests; HTTP against `httptest` fakes; fixtures from real payloads.
- Handle every error: return it wrapped (`fmt.Errorf("…: %w", err)`), or log it deliberately.
- Log every decision the user may later ask about ("stop recorded, resets 03:12 from quota",
  "resume sent", "skipped: session moved on") in one line each.

### Ask first
- Adding any Go module outside the standard library.
- Anything that writes outside `~/.local/state/letsgoagain/` and the systemd user unit.
- Changing what is sent to a session, or when.

### Never
- Log or store the service password, or print full request headers.
- Read or touch Claude's credentials (`~/.claude/`): the quota comes from meridian.
- Type into terminals, scrape panes, or touch herdr/tmux.
- Auto-approve permissions: the resume is one normal user message, nothing else.
- Resume a session earlier than its reset (apart from the user's `resume-now`), or more than 5 times per stop.
- Point a sandbox run at the user's real service or meridian: sandboxes use their own `XDG_*` dirs,
  `tools/fakemeridian` and their own ports.
- Push to a remote (the user pushes).

## 6. Code and commands

- Go 1.27 (installed: go1.27.3 linux/amd64), standard library only.
- Layout: `cmd/letsgoagain/` (main, subcommands); `internal/opencode` (service discovery, API client, event
  stream, limit-error parsing), `internal/meridian` (quota), `internal/state` (stops and their file),
  `internal/reset` (reset-time decision), `internal/daemon` (event loop, scheduler, control socket),
  `internal/fakemeridian` (test double of meridian); `tools/fakemeridian/` (the double as a command for
  sandbox runs); `dist/letsgoagain.service`.
- Every package starts with a package comment saying what it holds.
- Comments only for the non-obvious *why*.

| Purpose | Command |
|---------|---------|
| Format check | `gofmt -l .` (must print nothing; fix with `gofmt -w .`) |
| Vet | `go vet ./...` |
| Tests | `go test -race ./...` |
| Build | `go build -o bin/letsgoagain ./cmd/letsgoagain` |

All four pass before a change is called done; show the tail of the output.

**Sandbox run** (a real OpenCode against the fake meridian, never the user's): own `HOME` and `XDG_*`
dirs under `/tmp/opencode/lga-sandbox`, an `opencode.json` whose `anthropic` provider has
`baseURL: http://127.0.0.1:47812`, `tools/fakemeridian` on 47812, `opencode serve --port 47811`.

Environment: Linux, user greg, **shell fish** (commands given to the user must be fish-safe: `env VAR=x cmd`,
no bash-only syntax). Scratch directory: `/tmp/opencode`.

## 7. Coexistence with unsnooze

unsnooze 1.21.2 is installed and carries a local patch (`~/.unsnooze/local-claude-max.patch`) that teaches it
the meridian banner and reads reset times from `ai-usagebar`. While both run, a stop could be resumed twice.
Before LetsGoAgain resumes anything for real, the user turns unsnooze off (or uninstalls it); until then the
daemon runs with `--dry-run`.
