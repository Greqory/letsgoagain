# AGENTS.md — LetsGoAgain

Read this file first in every session. It is the project constitution: it wins over code comments and
any other document; only the user's chat instructions win over it.

## 1. Project

**LetsGoAgain** keeps OpenCode sessions from sleeping through a Claude usage limit. When a session stops
because the Claude Max 5-hour or weekly limit was hit, LetsGoAgain records the stop, waits until the
limit resets and then tells the same session to continue. "Let's go again!", every time.

It replaces `unsnooze` for one narrow setup, the user's, and does that one well:

- **OpenCode** (v2.3.0 measured) is the only agent. No Claude Code, Codex or others.
- **Claude Max via the meridian proxy**: the OpenCode plugin `opencode-with-claude` starts meridian
  (`@rynfar/meridian`) on `127.0.0.1:3456`, and `~/.config/opencode/opencode.json` points the `anthropic`
  provider there. meridian drives Claude Code underneath.
- **Any terminal**: herdr (the usual one), tmux or a plain terminal. LetsGoAgain never reads or types into a
  terminal; it talks to OpenCode itself.
- Linux, single user, single machine.

Out of scope: other agents, other providers, multiple machines, GUI, telemetry, getting around a limit
(LetsGoAgain only waits for the reset, it never retries early to "test" the limit more than §4 allows).

## 2. Why it exists (the problem, measured 2026-11-13)

- meridian classifies a limit as HTTP 429 internally (`classifyError`), but by then the response stream has
  already started with `200 OK`, so the error arrives as an SSE error event inside a 200 response.
- OpenCode does not treat that as retryable: the session simply stops. (A real 429 with `retry-after`
  would make OpenCode wait and retry by itself.)
- The error body carries the exact wait:
  `{"type":"error","error":{"type":"rate_limit_error","message":"Claude Max rate limit reached. Wait a moment and try again.","retry_after":4868}}`
  OpenCode stores it in its SQLite db (`~/.local/share/opencode/opencode.db`, table `session_message`,
  `data.error.type = "provider.rate-limit"`, body in `data.error.response.body`).
- unsnooze scrapes the terminal for known banners, did not know this one, and the reset time is not on
  screen. It never caught a single stop in two weeks.

## 3. Architecture

Two parts:

1. **`plugin/letsgoagain.js`**: a small OpenCode plugin (OpenCode plugins must be JS/TS; they run inside
   OpenCode). Installed to `~/.config/opencode/plugins/`. It only reports, it decides nothing: on a
   `session.error` event it sends `{sessionID, serverUrl, directory, error, at}` to the daemon over a Unix
   socket, plus the events the daemon needs to know a session moved on (a new user message, the session
   going busy). It never blocks OpenCode: unreachable daemon = log a warning and carry on.
   Pattern to copy: `~/.config/opencode/plugins/herdr-agent-state.js` (herdr's own plugin, same idea over
   `node:net`); do not edit that file.
2. **The Go program `letsgoagain`**: everything else.
   - `letsgoagain daemon`: listens on the socket, keeps the state, sleeps until the next reset, resumes.
     Runs as a systemd user service.
   - `letsgoagain status`: tracked stops, their reset time and source, attempts.
   - `letsgoagain resume-now [sessionID | --all]`: resume without waiting.
   - `letsgoagain install` / `uninstall`: copy the plugin, write and enable the systemd user unit; both
     reversible and idempotent.

### Reset time, in order of trust
1. **`retry_after`** from the error body (seconds, counted from the error's time). Exact, no extra request.
2. **Anthropic's usage endpoint** (what `ai-usagebar` uses; no dependency on `ai-usagebar`):
   `GET https://api.anthropic.com/api/oauth/usage` with the Claude Code OAuth access token from
   `~/.claude/.credentials.json`. Gives each window's utilisation and reset time (5h session, 7d weekly).
   Used when (1) is missing, to tell 5h from weekly, and to check before a resume that the window is over.
3. **Probing**: no time known → check again every 15 min, at most 5 h after the stop.

### Resuming
- Send the resume text to `POST {serverUrl}/session/{sessionID}/prompt_async` of the OpenCode instance
  that reported the stop. Default text: `Continue where you left off.` (configurable).
- Only when, at that moment: the instance answers, the session is idle, and nothing happened in the session
  since the stop (no newer user message). Otherwise the stop is closed as "resumed by someone else".
- Only root sessions are resumed. A subagent (child) session hitting the limit resumes its root session.
- After a resume, if the limit error comes back, reschedule; at most 5 attempts, then mark failed.
- The clock is checked every 30 s against wall time, so a laptop that slept through the reset still
  resumes at once on wake. No long single timers.
- An OpenCode instance that is gone (closed, crashed) is not revived: mark the stop "orphaned" and show it
  in `status`. (Reviving via `opencode -s <id>` in a new terminal: later, if the user wants it.)

### State and files
- State: `~/.local/state/letsgoagain/state.json` (written atomically: temp file + rename). Log:
  `~/.local/state/letsgoagain/letsgoagain.log`.
- Socket: `$XDG_RUNTIME_DIR/letsgoagain.sock`.
- Config (optional): `~/.config/letsgoagain/config.toml` only if a setting really needs one; defaults first.

## 4. To verify before relying on it

Facts above that come from reading code or types, not from a run. Each needs a check against the real
thing (log the raw payload once, then write a test fixture from it):

- [ ] The exact `session.error` payload OpenCode 2.3.0 hands to a plugin for the meridian limit. The
      installed plugin SDK types are 1.17.11 (`~/.config/opencode/node_modules/@opencode-ai/`); the v2 SDK
      types list `EventSessionError {id, type, properties: {sessionID?, error?}}`. Is `retry_after`
      reachable from the event, or only from the stored message?
- [ ] `prompt_async` request body and whether it shows up live in the TUI of that instance.
- [ ] How to read a session's status and parent (root vs child) through the instance's API.
- [ ] Usage endpoint: response fields, the required headers (`anthropic-beta`), behaviour with an expired
      access token. Seen in `ai-usagebar`'s binary, not documented by Anthropic: treat it as optional and
      fragile. Everything must work from `retry_after` alone.
- [ ] That a limit stop is the only thing that produces `rate_limit_error` here (not, e.g., a short burst
      limit that clears in seconds).

## 5. Rules

### Always
- Tests with every behaviour change. Pure logic (reset calculation, state transitions, payload parsing)
  is plain Go with table tests; HTTP against `httptest` fakes of the OpenCode instance and the usage
  endpoint; fixtures from real payloads, secrets removed.
- Handle every error: return it wrapped (`fmt.Errorf("…: %w", err)`), or log it deliberately.
- Log every decision the user may later ask about ("stop recorded, resets 03:12 from retry_after",
  "resume sent", "skipped: session busy") in one line each.

### Ask first
- Adding any Go module outside the standard library.
- Anything that writes outside `~/.local/state/letsgoagain/`, `~/.config/letsgoagain/`,
  `~/.config/opencode/plugins/letsgoagain.js` and the systemd user unit.
- Changing what is sent to a session, or when.

### Never
- **Refresh, rewrite or copy the Claude OAuth tokens.** `~/.claude/.credentials.json` is read-only for us;
  refreshing would rotate the refresh token under Claude Code and meridian. An expired access token means
  "no usage data", not "refresh it".
- Log tokens, the credentials file or full request headers.
- Type into terminals, scrape panes, or touch herdr/tmux.
- Auto-approve permissions or pass any "skip permissions" flag: the resume is one normal user message.
- Resume a session earlier than its reset (apart from the user's `resume-now`), or more than 5 times per stop.
- Push to a remote (the user pushes).

## 6. Code and commands

- Go 1.27 (installed: go1.27.3 linux/amd64), standard library first.
- Layout: `cmd/letsgoagain/` (main, subcommands), `internal/…` (one package per concern: `state`,
  `reset`, `opencode` (instance client), `usage` (Anthropic endpoint), `daemon`), `plugin/letsgoagain.js`,
  `dist/letsgoagain.service`.
- Every package starts with a package comment saying what it holds.
- Comments only for the non-obvious *why*.

| Purpose | Command |
|---------|---------|
| Format check | `gofmt -l .` (must print nothing; fix with `gofmt -w .`) |
| Vet | `go vet ./...` |
| Tests | `go test -race ./...` |
| Build | `go build -o bin/letsgoagain ./cmd/letsgoagain` |

All four pass before a change is called done; show the tail of the output.

Environment: Linux, user greg, **shell fish** (commands given to the user must be fish-safe: `env VAR=x cmd`,
no bash-only syntax). Scratch directory: `/tmp/opencode`.

## 7. Coexistence with unsnooze

unsnooze 1.21.2 is installed and carries a local patch (`~/.unsnooze/local-claude-max.patch`) that teaches it
the meridian banner and reads reset times from `ai-usagebar`. While both run, a stop could be resumed twice.
Before LetsGoAgain resumes anything for real, the user turns unsnooze off (or uninstalls it); until then the
daemon runs with resuming disabled (`--dry-run`: records and logs, sends nothing).
