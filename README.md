# dispatch

Orchestrate coding agents from Slack. A single Go binary runs a coordinator
that turns Slack messages into tasks, runs each task as a Claude Code session
in a local folder, a Docker container or an SSH host, relays permission
prompts back as buttons, and keeps every event in SQLite so sessions survive
restarts — a restarted dispatch resumes the tasks it cut short by itself.

```
@dispatch add retries to the HTTP client and run the tests          ⏳
  ▶️ task `a1b2c3d4` started with agent *coder* (local)
  🤖 `claude-sonnet-4-5` · acceptEdits · claude 2.1.239 · subscription · local /srv/work/a1b2c3d4
  @you 🔐 Bash wants to run: go test ./...        [Allow] [Deny]
  Added exponential backoff in client.go; 12 tests pass.
  @you ✅ done · 2m13s · 7 tool calls
  📊 max plan
  ▰▰▱▱▱▱▱▱▱▱ 15% · 5h
  ▰▰▰▱▱▱▱▱▱▱ 28% · 7d
```

While the agent works, the last message in the thread is a live status line
(`🔧 Bash \`go test ./...\` · 1m05s · 6 tool calls`) that is edited in place
every few seconds, and your message carries ⏳ — ✋ while the agent waits for
your answer. When the turn ends the status line goes and the mark becomes 📬:
the thread waits for your next message, or for `close`, which turns it ✅. A
task thread always carries one mark, so a channel shows which ones need you. The
lines that need you — a
permission or question prompt, the closing line, an error — mention whoever
started the task, so you can mute the thread and still be told when to look.
The closing line ends with the charge on an API key; on a Claude subscription
a meter follows it a moment later instead, one bar per plan window — the
5-hour and 7-day windows, and a model's own weekly window when it has one —
showing how much is used after the turn and, past 80%, when the window resets.

- **Transports**: Slack (Socket Mode, no public URL), a web UI, terminal. Telegram later.
  Conversations are shared: the web UI lists every thread, Slack's included, shows what
  was said there and answers its prompts; what you write in the web UI about a Slack thread
  is relayed into Slack, and a thread you start from the web UI in a Slack channel lives in
  Slack. The web UI keeps no history of its own — it reads the event log.
- **Surfaces**: `chat` (threads, commands, approvals), `feed` (ops channel mirror). Several per transport.
- **Agents**: Claude Code via `claude -p` stream-json. Codex later.
- **Environments**: local directory, Docker container, SSH host.
- **Definitions**: named agent configs (model, tools, permission mode, environment); many instances run concurrently, one per thread.

## Quick start

```sh
make build
bin/dispatch setup      # wizard: paths, claude check, Slack tokens, first agent, doctor
bin/dispatch run        # or: bin/dispatch run -terminal, or bin/dispatch run -web
make service          # systemd unit for the current user
```

### Transports

One dispatch can run several at once; conversations are shared between them.

- [Slack](docs/slack.md) — the app manifest, tokens, scopes, what a thread looks like.
- [Web UI](docs/web.md) — `transports = ["slack", "web"]`, accounts (`bin/dispatch user add`), the
  flight-strip board every thread is racked on, the React app.
- [Terminal](docs/terminal.md) — `bin/dispatch run -terminal`: one thread on stdin/stdout, no Slack needed; what `make e2e` drives.

Full instructions, Slack app manifest, docker/ssh notes: [SETUP.md](SETUP.md).
Architecture and conventions: [CLAUDE.md](CLAUDE.md).

## Commands

The same on every transport; on Slack, `@dispatch` is the mention, on the web UI and the terminal just type.

| message                      | effect                                      |
|------------------------------|---------------------------------------------|
| `@dispatch <prompt>`           | start a task with the channel's default agent (replies in a thread under your message) |
| `run <agent> <prompt>`       | start a task with a specific agent          |
| `run`                        | pick the agent from a menu, then type the prompt in the thread |
| `default <agent>`            | make `<agent>` the default for this channel (saved to config.toml); `default` shows it |
| reply in the thread          | follow-up to that task (resumes if idle)    |
| attach a file or image       | copied into the agent's environment, path added to the message (see [SETUP.md](SETUP.md#files-to-the-agent)) |
| button / reply to a question | answers the agent's `AskUserQuestion`       |
| `agent add`                  | define a new agent question by question; saved to config.toml, usable at once |
| `agent edit <name>`          | change an agent's model, environment, permissions, tools or system prompt; `agent edit` picks from a list |
| `agent delete <name>`        | remove an agent after a confirmation (refused while it is a default) |
| `close`                      | stop the task and end the thread: dispatch goes quiet there and marks it ✅ (mention it in the thread to pick it up again) |
| `status` / `cancel` / `agent list` / `help` | what they say                 |

## Layout

```
cmd/dispatch            run | setup | doctor
internal/transport    slack, web, terminal       — the wire
internal/surface      chat, feed                 — interaction on a transport
internal/coordinator                              — tasks, intents, event fan-out, decisions
internal/executor     local                      — one worker per task, idle timeout
internal/agent        claude                     — stream-json driver, permission handshake
internal/environment  local, docker, ssh         — where processes run
internal/store        sqlite                     — event log + projections
deploy/               systemd unit, Slack manifest, example config
```

## Tests

```sh
make test          # unit tests; docker and ssh tests use a real daemon / throwaway sshd, skipped if absent
make test-live     # also drives the real claude CLI (haiku, a few cents)
scripts/e2e.py bin/dispatch   # whole binary through the terminal transport (needs logged-in claude)
```
