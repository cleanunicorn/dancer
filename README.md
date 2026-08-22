# dancer

Orchestrate coding agents from Slack. A single Go binary runs a coordinator
that turns Slack messages into tasks, runs each task as a Claude Code session
in a local folder, a Docker container or an SSH host, relays permission
prompts back as buttons, and keeps every event in SQLite so sessions survive
restarts — a restarted dancer resumes the tasks it cut short by itself.

```
@dancer add retries to the HTTP client and run the tests
  ▶️ task `a1b2c3d4` started with agent *coder* (local)
  🤖 `claude-sonnet-4-5` · acceptEdits · claude 2.1.239 · subscription · local /srv/work/a1b2c3d4
  🔐 Bash wants to run: go test ./...        [Allow] [Deny]
  Added exponential backoff in client.go; 12 tests pass.
  ✅ done · $0.31
```

- **Transports**: Slack (Socket Mode, no public URL), terminal. Telegram later.
- **Surfaces**: `chat` (threads, commands, approvals), `feed` (ops channel mirror). Several per transport.
- **Agents**: Claude Code via `claude -p` stream-json. Codex later.
- **Environments**: local directory, Docker container, SSH host.
- **Definitions**: named agent configs (model, tools, permission mode, environment); many instances run concurrently, one per thread.

## Quick start

```sh
make build
bin/dancer setup      # wizard: paths, claude check, Slack tokens, first agent, doctor
bin/dancer run        # or: bin/dancer run -terminal
make service          # systemd unit for the current user
```

Full instructions, Slack app manifest, docker/ssh notes: [SETUP.md](SETUP.md).
Architecture and conventions: [CLAUDE.md](CLAUDE.md).

## Commands in Slack

| message                      | effect                                      |
|------------------------------|---------------------------------------------|
| `@dancer <prompt>`           | start a task with the channel's default agent (replies in a thread under your message) |
| `run <agent> <prompt>`       | start a task with a specific agent          |
| `run`                        | pick the agent from a menu, then type the prompt in the thread |
| `default <agent>`            | make `<agent>` the default for this channel (saved to config.toml); `default` shows it |
| reply in the thread          | follow-up to that task (resumes if idle)    |
| button / reply to a question | answers the agent's `AskUserQuestion`       |
| `agent add`                  | define a new agent question by question; saved to config.toml, usable at once |
| `agent edit <name>`          | change an agent's model, environment, permissions, tools or system prompt; `agent edit` picks from a list |
| `agent delete <name>`        | remove an agent after a confirmation (refused while it is a default) |
| `close`                      | stop the task and end the thread: dancer goes quiet there and marks it ✅ (mention it in the thread to pick it up again) |
| `status` / `cancel` / `agent list` / `help` | what they say                 |

## Layout

```
cmd/dancer            run | setup | doctor
internal/transport    slack, terminal            — the wire
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
scripts/e2e.py bin/dancer   # whole binary through the terminal transport (needs logged-in claude)
```
