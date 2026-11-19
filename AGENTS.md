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

- **OpenCode 2 has its own installer**: `curl -fsSL https://opencode.ai/v2/install | bash`. The plain
  `opencode.ai/install` and npm's `opencode-ai@latest` install OpenCode 1 (1.19.5 on 2026-11-14), which has
  no shared service and no v2 API (the first canary run failed on exactly that).
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
  (`GET /api/session/{id}/message?order=desc`) is `type: "idle"`, `outcome: "failed"`, same `error`, after the
  failed `assistant` message. The messages are written by the time `session.execution.failed` arrives.
- **OpenCode sometimes runs an empty execution right after a limit failure** (2026-11-14: 6 times in two
  weeks, 5–60 ms after the failure, root and child sessions): `execution.started` → `succeeded` with no model
  call and no message, just one more `idle` entry with `outcome: "succeeded"`; the session's `outcome` and
  `time.idle` change with it. So neither a started nor a succeeded execution, nor `outcome`/`time.idle`,
  says that anyone went on. Only the messages do: while the newest message that isn't `idle` is still the
  failed assistant message, the session stands at the stop (`opencode.StopOf`). Fixture:
  `messages_noop_after_limit_desc.json` (the real case; trusting `execution.started` closed that stop as
  "continued without LetsGoAgain" and the session was never resumed).
- **Resuming works**: `POST /api/session/{id}/prompt` with `{"text": "…"}` on the failed session starts a
  new execution (`session.execution.started` → `succeeded`).
- **A note in the TUI without running the session** (measured 2026-11-14): `POST /api/session/{id}/synthetic`
  with `{"text", "description", "resume": false}` queues an inbox item (`session.inbox.enqueued` only): no
  execution, `outcome` and `time.idle` unchanged (`time.updated` moves). The TUI shows only the
  `description`, as `◈ <description>` under the session (the user saw it); the `text` reaches the agent only
  if the item is delivered with the next input. `DELETE /api/session/{id}/inbox/{inboxID}` removes it (a
  delivered or unknown item is a no-op); `PATCH` only changes the delivery, and steering wakes the session.
  There is no endpoint for a real toast (`tui.toast.show` is an event type with no way to raise it from
  outside; that would take an OpenCode plugin).
- **Subagents** are sessions with `parentID`; roots have none. A child's finished round wakes its parent.
  When a child hits the limit, its root fails with the limit too; resuming the root is enough, the root
  wakes the child again (real run, 2026-11-14).
- **Events**: `GET /api/event` (SSE), filter with repeated `types=` (a comma list is a 400); the stream
  starts with `server.connected`.
- **meridian's quota**: `GET http://127.0.0.1:3456/v1/usage/quota` (no auth) returns `buckets` of
  `{type: five_hour | seven_day | seven_day_fable, status: allowed | allowed_warning | rejected,
  utilization (0…1), resetsAt (epoch ms), observedAt}`. meridian sets `rejected` when utilization ≥ 1 or
  Claude Code reported the window rejected. **That status sticks after the reset until the next request,
  and the next window then has a new future `resetsAt`**: measured 2026-11-15 09:08, `five_hour`
  `rejected` at utilization 0.37 with the reset 5 h ahead, no limit hit; trusting it kept a session waiting
  from 01:32 on, rescheduled every 5 h. So `rejected` counts only with utilization ≥ 0.95 (or none), and a
  past `resetsAt` never counts. Fixture: `internal/meridian/testdata/`.
- Why unsnooze failed: it scrapes the terminal for known banners, did not know meridian's, and the reset
  time is not on screen. It never caught a stop in two weeks.

## 3. Architecture

One Go program, `letsgoagain`, no OpenCode plugin.

- `letsgoagain daemon [--dry-run]`: runs as a systemd user service.
  1. Reads `service.json`, connects to the service, subscribes to `session.execution.failed|succeeded`
     (not `started`: §2, empty executions). Reconnects (re-reading `service.json`) every 30 s when the
     service is down or restarted.
  2. On every (re)connect, logs the OpenCode version (from `service.json`) and meridian's (the `version`
     field of its `/health`, nothing else from it) against the tested ones in `internal/compat`, with a
     warning when they differ (only when that changed since the last connect), and scans the root sessions
     updated within 12 h (and those with an open stop) by their messages: a stop it missed is recorded, an
     open stop whose session went on is closed.
  3. A `session.execution.failed` with `provider.rate-limit` on a **root** session records a stop, keyed by
     the failed assistant message's ID (child sessions are ignored: their parent fails too and is the one
     resumed). A `succeeded` closes an open stop only if the messages show the session went on (§2).
  4. Checks the wall clock every 30 s (a laptop that slept through a reset resumes on wake); resumes due stops.
  5. `--dry-run` records and logs, sends nothing.
  6. **TUI notice** (off with `--no-notice`, never in dry run): a recorded stop queues
     `LetsGoAgain · Claude limit reached · continues automatically <Mon 15:04>` (`reset unknown, checking
     again …` while probing) in the session (§2, synthetic with `resume: false`); a new plan replaces it; it is
     removed **before** the resume prompt (otherwise it would be delivered to the agent with it) and whenever
     the stop closes otherwise. A notice that fails is logged and never blocks a resume.
- `letsgoagain status [--all] [--12h] [--no-update-check]`: the daemon (connected, dry run), the quota windows with their reset
  times (meridian, as the daemon sees it), the running root sessions ("Listening": `GET /api/session/active`
  without subagents), and the stops with reset time, its source and attempts. Open stops first (soonest
  first), then closed ones, most recently closed first; by default only open ones and those changed in the
  last 24 h, `--all` every tracked one with full dates. 24-hour clock regardless of locale (the user's own
  `LANG` is en_US with a 24-hour day); `--12h` for AM/PM. The daemon assembles all of it; without a daemon only
  the stops from the state file are shown. The one network call the CLI itself makes is the update check
  (user, 2026-11-19): the newest release tag from GitHub, 4 s timeout, run in parallel with the overview,
  first line of the output (above the daemon line) "up to date" / "Update available … run: letsgoagain update" / a failure note;
  `--no-update-check` skips it. Only when the user runs `status`, never from the daemon.
- `letsgoagain doctor`: one line per dependency (the binary, the OpenCode service and its event stream,
  meridian's quota, the systemd unit and which binary it runs, the daemon's socket), ok/WARN/FAIL with a
  hint; exit 1 on a FAIL. Reads only; never prints the service password or meridian's `/health` (it holds
  the account's email).
- `letsgoagain resume-now [sessionID | --all]`: resume without waiting for the reset.
- `letsgoagain install [--dry-run] [--no-notice]` / `uninstall`: write, enable and remove the systemd user
  unit (the flags go to the daemon's `ExecStart`); idempotent.
- `letsgoagain update [--check]` / `version`: replace the binary with the latest GitHub release (checksum
  verified against the release's `checksums.txt`, atomic rename, service restarted); only when the user
  runs it. `install.sh` (repo root) installs or updates from the releases without Go.
- The CLI talks to the daemon over `$XDG_RUNTIME_DIR/letsgoagain.sock`; `status` falls back to reading the
  state file when the daemon is not running.

### Reset time, in order of trust
1. **meridian quota**: buckets that are exhausted (utilization ≥ 1, or `rejected` with utilization ≥ 0.95 or unknown) with `resetsAt` in the
   future → the latest of their `resetsAt`. Also says 5h vs weekly.
2. **`retry_after`** from the error body, counted from the stop, only when it is above 60 and below 86400
   (60 = meridian did not know, 86400 = clamped).
3. **Probing**: neither → ask the quota again every 15 min; at the latest 5 h after the stop, try once.

A reset time gets a 60 s margin before the resume.

### Resuming a due stop
1. Quota still shows an exhausted bucket with a future reset → reschedule to it; no attempt used.
2. The session must still stand at the stop: not running (`GET /api/session/active`) and its newest
   non-`idle` message still the stop's failed assistant message (§2). Another message → close as "moved on"
   (someone else continued it); a newer limit failure → follow that stop instead (reschedule).
3. Send `The Claude usage limit has reset. Continue where you left off.` (the user's choice, 2026-11-14:
   naming the limit tells the agent why it stopped; `--message` overrides it) via
   `POST /api/session/{id}/prompt`. Count the attempt.
4. `session.execution.succeeded` after the resume closes the stop as done. Another limit failure on the same session before
   a success is the same stop again: reschedule; after 5 attempts mark it failed and stop trying.
5. The service unreachable at the due time → keep the stop and try on the next tick (the service may be
   restarting); a session that no longer exists (404 on `GET /api/session/{id}`) → "gone". A session
   that is still listed but whose prompt answers 404 (its worktree was removed while it was stopped,
   2026-11-15) is tried on 3 ticks in a row (`state.MaxRefusals`), then marked failed.

### State and files
- State: `~/.local/state/letsgoagain/state.json` (atomic: temp file + rename); log:
  `~/.local/state/letsgoagain/letsgoagain.log`.
- Socket: `$XDG_RUNTIME_DIR/letsgoagain.sock`.
- No config file until a setting needs one; flags with defaults first.

## 4. Still to verify

- [x] The resume message shows up live in the user's TUI (user saw it, 2026-11-14).
- [x] A real limit stop end to end (night of 2026-11-14, two root sessions): stops recorded at 02:51 and
      03:08 from the meridian quota (five_hour, reset 03:30), resumes sent at 03:31, both ran to the end of
      their work (one coordinator until 05:06), one attempt each. A subagent had hit the limit first
      (02:50); resuming its root was enough: the root woke the subagent itself.
- [ ] A session whose *first* message hits the limit (the README's "queue tonight's work" case), with a
      real agent: the mechanics are the sandbox's "live" scenario, but a real agent starting its task from
      the resume message has not been seen yet.
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
- Anything that writes outside `~/.local/state/letsgoagain/`, the systemd user unit and the binary itself
  (`install.sh`, `letsgoagain update`).
- Any network access besides the OpenCode service, meridian and, on the user's `update`, GitHub.
- Changing what is sent to a session, or when.

### Never
- Log or store the service password, or print full request headers.
- Read or touch Claude's credentials (`~/.claude/`): the quota comes from meridian.
- Type into terminals, scrape panes, or touch herdr/tmux.
- Auto-approve permissions: the resume is one normal user message, nothing else.
- Resume a session earlier than its reset (apart from the user's `resume-now`), or more than 5 times per stop.
- Point a sandbox run at the user's real service or meridian: sandboxes use their own `XDG_*` dirs,
  `tools/fakemeridian` and their own ports.
- Push `main` or tags (the user does). Feature branches and pull requests only when the user asks for
  them (user, 2026-11-14); the user merges.

## 6. Code and commands

- Go 1.27 (installed: go1.27.3 linux/amd64), standard library only.
- Layout: `cmd/letsgoagain/` (main, subcommands); `internal/opencode` (service discovery, API client, event
  stream, limit-error parsing), `internal/meridian` (quota), `internal/state` (stops and their file),
  `internal/reset` (reset-time decision), `internal/daemon` (event loop, scheduler, control socket),
  `internal/update` (self-update from GitHub releases), `internal/compat` (the tested OpenCode and
  meridian versions: raise `TestedOpenCode` after `tools/sandbox/e2e.py` passes on a new OpenCode,
  `TestedMeridian` after a real limit stop worked with a new meridian, and the measured versions in §1),
  `internal/fakemeridian` (test double of meridian);
  `tools/fakemeridian/` (the double as a command for sandbox runs); `tools/check.sh` (the check set);
  `tools/sandbox/e2e.py` (the end-to-end run); `install.sh` (installer for users); `.github/workflows/`
  (`ci.yml`: the check set on pushes and pull requests; `canary.yml`: `tools/sandbox/e2e.py` against the
  newest OpenCode, weekly and by hand (`gh workflow run canary --ref <branch>`), not on pull requests: the
  agent runs `e2e.py` locally before a PR that touches the daemon, the OpenCode client or the sandbox (user,
  2026-11-14: PRs stay fast); a failing canary means an OpenCode update changed something: read its log,
  fix, raise the tested version; `release.yml`: on a `v*.*.*` tag, checks, runs the same end-to-end check
  against the newest OpenCode, builds
  `letsgoagain_linux_{amd64,arm64}` with `-X main.version=<tag>` and `checksums.txt`, publishes the
  release); `.github/dependabot.yml` (monthly updates of the workflows' actions; there are no Go module
  dependencies); `.github/rulesets/*.json` (the repo's rulesets as applied on GitHub: `main` cannot be
  deleted or force-pushed, `v*` tags cannot be deleted or moved; GitHub does not read these files, they
  are applied with `gh api`); `README.md` (for users), `LICENSE` (MIT). The systemd unit is generated by
  `letsgoagain install` (`cmd/letsgoagain/install.go`).
- The release asset names, `checksums.txt` and the repo `Greqory/letsgoagain` are shared by
  `release.yml`, `install.sh` and `internal/update`: change them together.
- Every package starts with a package comment saying what it holds.
- Comments only for the non-obvious *why*.

| Purpose | Command |
|---------|---------|
| All four below, stopping at the first failure | `tools/check.sh` |
| Format check | `gofmt -l .` (must print nothing; fix with `gofmt -w .`) |
| Vet | `go vet ./...` |
| Tests | `go test -race ./...` |
| Build | `go build -o bin/letsgoagain ./cmd/letsgoagain` |
| End to end against a real OpenCode (~5 min) | `python3 -I tools/sandbox/e2e.py` |
| Release (the user pushes the tag) | `git tag v0.1.0 && git push origin v0.1.0` |

All four checks pass before a change is called done; show the tail of the output. Run the end-to-end
check after changes to the daemon or the OpenCode client, and after an OpenCode or meridian update.

**Sandbox run** (a real OpenCode against the fake meridian, never the user's): own `HOME` and `XDG_*`
dirs under `/tmp/opencode/lga-sandbox`, an `opencode.json` whose `anthropic` provider has
`baseURL: http://127.0.0.1:47812`, `tools/fakemeridian` on 47812, `opencode serve --port 47811`.
`tools/sandbox/e2e.py` sets all of this up and tears it down. Never stop sandbox processes with
`pkill -f`/`pgrep -f` patterns: they match the agent's own shell; use the PIDs from `ss -ltnp`.

Environment: Linux, user greg, **shell fish** (commands given to the user must be fish-safe: `env VAR=x cmd`,
no bash-only syntax). Scratch directory: `/tmp/opencode`.

## 7. Coexistence with unsnooze

unsnooze 1.21.2 is installed and carries a local patch (`~/.unsnooze/local-claude-max.patch`) that teaches it
the meridian banner and reads reset times from `ai-usagebar`. While both run, a stop could be resumed twice.
Before LetsGoAgain resumes anything for real, the user turns unsnooze off (or uninstalls it); until then the
daemon runs with `--dry-run`.
