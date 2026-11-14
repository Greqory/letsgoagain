# LetsGoAgain

**Your OpenCode agents hit the Claude usage limit at 3 am. LetsGoAgain wakes them up the minute it resets.**

You start a few agents before bed. Somewhere in the night the Claude Max 5-hour limit runs out, every
session stops, and in the morning you find them all waiting for you. LetsGoAgain fixes that:

```
02:51  "refactor the parser": limit stop recorded, reset from the meridian quota (five_hour) at Sat 03:30
03:08  "task queue":          limit stop recorded, reset from the meridian quota (five_hour) at Sat 03:31
03:31  "refactor the parser": resume sent (reset reached, attempt 1/5)
03:31  "task queue":          resume sent (reset reached, attempt 1/5)
03:31  "refactor the parser": running again, stop done
03:32  "task queue":          running again, stop done
```

When the limit resets, each stopped session gets one message, **"Continue where you left off."**, and
carries on. It's like typing it yourself, without the 3 am alarm.

## Is this for me?

LetsGoAgain is built for one setup and does that one well:

- **[OpenCode](https://opencode.ai) v2** (tested with 2.3.0)
- **Claude Max through [meridian](https://github.com/rynfar/meridian)**, usually via the
  [`opencode-with-claude`](https://github.com/ianjwhite99/opencode-with-claude) plugin
- **Linux** with systemd (x86_64 or arm64)

Any terminal works: tmux, herdr, Zellij or a plain one. LetsGoAgain never touches the terminal; it talks
to OpenCode directly.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/Greqory/letsgoagain/main/install.sh | sh
```

This downloads the latest release, checks its checksum, puts `letsgoagain` in `~/.local/bin` and starts
it as a systemd user service. That's it: there is nothing to configure and nothing to restart in OpenCode.

Check that it's running:

```sh
letsgoagain status
```

<details>
<summary>Install options</summary>

Set these in front of `sh`, e.g. `curl -fsSL … | LETSGOAGAIN_DRY_RUN=1 sh`:

| Variable | Effect |
|---|---|
| `LETSGOAGAIN_DRY_RUN=1` | Watch only: record and log stops, never send anything. Good for a first night. |
| `LETSGOAGAIN_VERSION=v0.1.0` | Install a specific release instead of the latest. |
| `LETSGOAGAIN_INSTALL_DIR=DIR` | Put the binary somewhere other than `~/.local/bin`. |
| `LETSGOAGAIN_NO_SERVICE=1` | Only install the binary; start the service later with `letsgoagain install`. |

</details>

<details>
<summary>With Go, or from source</summary>

With Go 1.27 or newer:

```sh
go install github.com/Greqory/letsgoagain/cmd/letsgoagain@latest
letsgoagain install
```

From a clone:

```sh
git clone https://github.com/Greqory/letsgoagain && cd letsgoagain
go build -o ~/.local/bin/letsgoagain ./cmd/letsgoagain
letsgoagain install
```

</details>

## Use

Mostly you don't: start your agents and go to sleep. When you want to look:

| Command | What it does |
|---|---|
| `letsgoagain status` | Every tracked stop: when it stopped, when it resumes, how many tries. |
| `letsgoagain doctor` | Checks everything it depends on: OpenCode, its event stream, meridian's quota, the service, the daemon. |
| `letsgoagain resume-now <session-id>` | Resume one stopped session right away. `--all` for every one. |
| `letsgoagain update` | Update to the latest release (checks first; `--check` only tells you). |
| `letsgoagain version` | Show the installed version. |

Everything it decides is written to `~/.local/state/letsgoagain/letsgoagain.log`, one line per decision:

```sh
tail -f ~/.local/state/letsgoagain/letsgoagain.log
```

## Update

```sh
letsgoagain update
```

It fetches the latest release, verifies the checksum, swaps the binary and restarts the service. Running
the install script again does the same.

## Uninstall

```sh
letsgoagain uninstall                  # stops and removes the service
rm ~/.local/bin/letsgoagain            # the binary
rm -rf ~/.local/state/letsgoagain      # its state and log
```

## How it works

1. **It listens.** OpenCode v2 runs all sessions in one background service. LetsGoAgain connects to it
   (address and password from `~/.local/state/opencode/service.json`) and follows its events.
2. **It notices a limit stop.** When the limit hits, OpenCode retries for about 80 seconds and then gives
   up with a rate-limit error. That's the signal. Subagents are covered through their main session.
3. **It works out when the limit resets**, in this order:
   - meridian's own quota (`/v1/usage/quota`): which window is used up and the exact reset time;
   - the `retry_after` in meridian's error;
   - if neither is known, it checks the quota again every 15 minutes (at most 5 hours).
4. **It waits**, checking the clock every 30 seconds, so a laptop that slept through the reset resumes
   as soon as it wakes.
5. **It resumes**, one minute after the reset, but only if the session is exactly as the limit left it.
   If you already continued it yourself, LetsGoAgain stays out of the way.

If a resumed session runs into the limit again, it waits for the next reset. After 5 tries for the same
stop it gives up and shows that in `status`.

## What it never does

- It never sends a message before the limit has reset. It waits; it doesn't get around limits.
- It never approves permissions or answers questions for your agent. A session waiting for you stays
  waiting.
- It never reads your Claude credentials. The quota comes from meridian.
- It never types into your terminal.
- It never sends anything anywhere except to your own OpenCode service and meridian on `127.0.0.1`.
  `letsgoagain update` talks to GitHub, and only when you run it.

## Troubleshooting

Start with `letsgoagain doctor`: one line per dependency, and a hint for each one that isn't fine.

```
ok    letsgoagain      v0.2.0 (/home/you/.local/bin/letsgoagain)
ok    OpenCode         http://127.0.0.1:49374, OpenCode 2.3.0 (tested: 2.3)
ok    event stream     subscribed
ok    meridian         meridian 1.79.0 (tested: 1.79); five_hour 51% (resets Sat 15:29); seven_day 65% (resets Mon 02:59)
ok    service          active
ok    daemon           answering, 0 open stops, 2 tracked
```

**A WARN says OpenCode or meridian is newer than the tested version.** LetsGoAgain relies on how they
report a limit; an update can change that. Most likely it still works. If a limit stop goes unnoticed,
please open an issue with the versions `doctor` shows. The daemon log notes the versions too, at every
change.

**`status` says the daemon is not running.** Look at `systemctl --user status letsgoagain` and the log.
`letsgoagain install` starts it again.

**The log says "OpenCode service unreachable".** OpenCode isn't running, or it's an OpenCode v1 without
the shared service. LetsGoAgain retries every 30 seconds by itself.

**Nothing happens after a limit.** Check the log for "limit stop recorded". If it's missing, the stop
didn't look like meridian's limit error; please open an issue with the log lines and your OpenCode and
meridian versions.

**It stops working when I log out.** systemd user services end with your session, and so does OpenCode.
Stay logged in overnight (a locked screen is fine; suspend pauses everything until wake-up).

## Development

```sh
tools/check.sh                    # gofmt, go vet, tests with -race, build
python3 -I tools/sandbox/e2e.py   # end to end against a real OpenCode and a fake meridian (~5 min)
```

The end-to-end run uses its own OpenCode in `/tmp/opencode/lga-sandbox` and never touches yours.
[`AGENTS.md`](AGENTS.md) holds the measured facts the design rests on and the project rules.

Releases: push a tag like `v0.1.0`; the release workflow tests, builds the Linux binaries and publishes
them with `checksums.txt`.

## License

[MIT](LICENSE). Not affiliated with Anthropic, OpenCode or meridian.
