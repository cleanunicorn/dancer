# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Start in a worktree

**Always begin work in a git worktree, never directly on a checkout of `main`.** Create one before
the first edit, do the whole change there, and open the PR from that branch.

```sh
git worktree add .claude/worktrees/<short-topic> -b <short-topic>
```

`.claude/worktrees/` is gitignored and is where existing worktrees live. In Claude Code, the
`EnterWorktree` tool does the same thing and switches the session into it.

## Stopping a dancer: gracefully, and never by pattern

dancer runs agents that work on *this repo*, so an agent's cleanup command can stop the
dancer that is running it. Two rules.

**Shut it down, do not kill it.** SIGTERM is the contract: dancer notifies live threads,
lets in-flight tool calls finish for `drain_timeout` (default 2m), persists final state and
exits 0 — and interrupted tasks then resume themselves on the next start. `kill -9` skips
all of that, cutting tool calls mid-write and leaving tasks that have to be picked up by
hand. Always wait for the process to actually exit instead of assuming it is gone:

```sh
kill "$pid"                                   # SIGTERM: drain, persist, exit 0
while kill -0 "$pid" 2>/dev/null; do sleep 1; done
```

For the deployed service, let systemd do it — it sends SIGTERM and waits `TimeoutStopSec=150`:

```sh
sudo systemctl stop dancer        # or: restart
```

**Never find a dancer by command-line pattern.** `-f` matches anywhere in the command line, so
`"bin/dancer run"` also matches the deployed `/usr/local/bin/dancer run`. This has taken the
production instance down mid-task twice — the second time via `pgrep -f "bin/dancer run"`
followed by `kill <pid>`, so killing "by pid" is no safer when the pid came from a pattern.
`pgrep`, `pkill`, `ps | grep` and `killall` are all the same hazard.

Keep the pid from the process you started, and use only that:

```sh
env DANCER_CONFIG=/tmp/dancer-test/config.toml bin/dancer run & pid=$!
# ... test ...
kill "$pid"; while kill -0 "$pid" 2>/dev/null; do sleep 1; done
```

If you truly have no pid, anchor to the absolute path you launched and check what you matched
before signalling anything:

```sh
pgrep -af "^/tmp/dancer-test/bin/dancer run"   # -a: read it first, confirm no /usr/local/bin
```

A `pgrep`/`pkill` pattern that could match `/usr/local/bin/dancer` is always a bug.

## Commands

```sh
make build            # bin/dancer
make test             # go test ./...
make test-race        # go test -race -count=1 ./...
make lint             # gofmt -l check + go vet   (run before finishing a change)
make fmt tidy
make run              # bin/dancer run -config $CONFIG   (Slack)
make run-terminal     # same, but the terminal transport — the fastest way to try a chat change
make run-web          # same, but the web transport (browser UI on web.listen)
make doctor           # config, claude login, docker, ssh hosts, Slack tokens
make help             # every target
```

Single test / package:

```sh
go test ./internal/coordinator -run TestName -v
go test -race -count=1 ./internal/coordinator     # coordinator is concurrent; use -race
DANCER_LIVE=1 go test -count=1 ./internal/agent/claude   # drives the real claude CLI (~cents, haiku)
DANCER_LIVE=1 go test -count=1 ./internal/decider ./internal/coordinator -run Live   # real decider verdicts
make e2e              # scripts/e2e.py: whole binary through the terminal transport
make restart-drill    # scripts/restart-drill.py: SIGTERM mid-tool-call → drain → resume
```

`DANCER_DOCKER_PROVISION=1 go test ./internal/environment/docker` builds a real image from
`ubuntu:24.04` (~60s, downloads packages); it is skipped otherwise.

Tests that need real infrastructure skip themselves rather than fail: docker tests need a live
daemon, ssh tests spin up a throwaway `sshd`, live claude tests need `DANCER_LIVE=1` plus a
logged-in `claude`. Keep that pattern for new integration tests.

Config path is `$DANCER_CONFIG`, else `~/.config/dancer/config.toml`. `deploy/config.example.toml`
is the reference for every key.

## Architecture

One Go binary (`cmd/dancer`: `run` | `setup` | `doctor` | `user`). Data flows in one loop:

```
transports --Inbound--> surfaces --Intent--> Coordinator --Task--> Executor --> Agent --> Environment
transports <-Outbound-- surfaces <--Event--- Coordinator <-----agent.Event------------------┘
                                                 ↓
                                            Store (SQLite event log + projections)
```

Each layer is an interface defined in the package doc of `internal/<pkg>/<pkg>.go`; read those
files first — they carry the contract, the concrete packages under them are implementations.

- **`transport`** (slack, web, terminal) — dumb on purpose: text, prompt-with-choices, files, `ThreadID`
  (Slack: `"<channel>/<thread_ts>"`; web: `"<channel>/<id>"`). It never interprets a message.
  Files go both ways: `Outbound.Files` are uploaded after the text; `Inbound.Files` are the
  attachments a human sent, downloaded by the transport (Slack: `files:read`), and the executor
  copies them into the environment under `/tmp/dancer/inbox/<task>/` and appends the paths to the
  message. `File.Data` is never written to the event log, only the name — so the web UI shows a
  thread's past attachments by name only, and the bytes while the page is open.
  **A conversation belongs to dancer, not to a transport.** The transport that minted the id
  *hosts* it (`TaskState.Transport`) and renders it natively; a `transport.Observer` (the web UI)
  is shown every thread of every transport, and anyone may write into any thread. The
  coordinator relays what humans write to the host and the observers as `Outbound.From`
  (`Decision` for answers), so each transport shows the whole exchange its own way — Slack
  posts "💬 *name* via web: …" and settles a prompt's buttons, the web shows a bubble — and the
  log keeps one record (the inbound), never the relays. An inbound to `"<channel>/"` asks the
  channel's owner (`ChannelLister`) to open a thread (`ThreadOpener`), so a web user can start
  work in a Slack channel. The web transport has no memory: lists and history come from the
  coordinator through `transport.History` (`coordinator/threads.go`); only the live status line
  and open prompts are kept in memory. Its users are accounts in the store (`dancer user add`,
  `web/auth.go`: PBKDF2 hashes, sessions by token hash); the session's name is the
  `Inbound.UserID`. `Inbound.UserName` is the display name when a transport has one (Slack:
  users.info, cached).
  Keyed messages (`Outbound.Key`) are its one stateful feature: Slack edits/deletes the message it
  posted under the key and mirrors the text into the thread's assistant status; the terminal redraws
  the line. `Outbound.Mention` addresses one user (Slack: `<@U…>` in front of the text; terminal
  ignores it); the chat surface sets it to the task's `Requester` on the lines that need a human —
  prompts, the closing line, errors, the restart notices that ask someone to pick a task up — never
  on the agent's Markdown text, which the markdown block does not render mentions in.
- **`surface`** (chat, feed) — everything about *how* humans interact. `Handle` turns an inbound
  message into `[]Intent` (returning `ok=false` passes it to the next surface on that transport);
  `Render` turns a coordinator `Event` into outbound messages. Several surfaces share one transport,
  so a new interaction style on Slack is a **new surface, not a new Slack client**. The chat surface
  keeps one live status line per running turn (what tool, for how long, how many calls) as a *keyed*
  message (`Outbound.Key`): the transport edits it in place, and the surface moves it below every
  ordinary message and takes it down when the turn ends or a prompt is open. Task events reach
  every surface; `chat` renders only the tasks hosted on its own transport (the coordinator
  copies the result to observers), `feed` renders everything into its own thread.
- **`coordinator`** — the only stateful brain: intents → tasks, event fan-out to every surface,
  permission/question decision relay (`pending`/`askText` maps keyed by prompt id), guided wizards
  (`wizard.go`: add/edit/delete agent, the bare-`run` agent picker), restart recovery. It is also
  the clock: every `Heartbeat` (10s) while a turn runs it broadcasts `EventHeartbeat`, and on a
  `transport.Reactor` it marks the thread's root message ⏳ (working) / ✋ (waiting for a human).
- **`executor`** (local) — one worker per task: provisions the environment, starts the agent,
  keeps a finished turn's process alive for `idle_timeout` so follow-ups are instant, then resumes
  the session with `--resume`.
- **`agent`** (claude) — normalizes vendor protocol into `agent.Event`. Nothing above this layer
  sees stream-json.
- **`environment`** (local, docker, ssh) — "I can exec a command and stream its stdio", nothing more.
  Docker and SSH shell out to the `docker` and `ssh` CLIs deliberately (no SDKs; the user's ssh
  config/agent and docker context just work). Docker also *provisions*: `Spec.Provision` turns a
  plain base image into an agent-ready one (git, Node, the agent CLI, a user with the host uid and
  a writable `$HOME`) and `docker commit`s it as `dancer-env:<hash>`, built once per hash;
  `Spec.Reuse`/`ReuseKey` keep one container per thread or definition with `$HOME` on a volume.
- **`store`** (sqlite) — append-only `Record` log; `TaskState`/`Definition`/`FlowState` are
  projections over it. Crash recovery is a replay: live tasks become `interrupted`/`idle`, and the
  next message resumes the agent session.
- **`decider`** (static, claude, openai) — answers the coordinator's two policy questions, "resume
  this cut-short task?" and "allow this tool call without asking?", from facts read out of the log.
  A verdict can only narrow: the rules' answer is the floor and every failure falls back to it;
  `auto_allow` is the operator's ceiling and is matched before a decider is asked. Off by default.
  `DECIDER.md` is the design and the plan; `decider.ParseAllow` is the one parser of `auto_allow`
  patterns, shared by config validation and the coordinator's matcher.

### Invariants worth knowing before editing

- **The event log is the source of truth.** New state belongs in a projection derived from appended
  records, not in a field that only lives in memory.
- **Permission prompts are first-class and cross-surface.** `agent.EventNeedsPermission` →
  `surface.Event` (with `PromptID`) → `transport.Prompt` → `transport.Decision` → `surface.Decide`
  → `agent.PermissionDecision`. Any surface that rendered a prompt may answer it, so prompt ids are
  namespaced per surface and resolved on a base id in the coordinator; a decision is offered to
  every surface whatever transport it came from, so a prompt rendered by `chat-slack` can be
  answered from the web UI.
- **`AskUserQuestion` reuses the same path** as permissions, via `EventQuestion` + `Question.Answers`.
- **Output on a thread is ordered, and keyed messages depend on it.** `emit` renders and sends under
  a per-thread lock, because a heartbeat (ticker goroutine) and an agent event (executor goroutine)
  can both touch the status line; a surface that posts `[remove status, text, status]` needs those
  to reach the transport in that order. Heartbeat output is not written to the event log.
- **Agent text is Markdown, ours is transport markup.** `Outbound.Markdown` is set only on what the
  agent wrote; Slack renders it through a Block Kit `markdown` block (falling back to plain text)
  while dancer's own lines stay mrkdwn (`*bold*`, backticks).
- **The Claude handshake is protocol-sensitive** (`internal/agent/claude`): spawn with
  `--permission-prompt-tool stdio`, send a `control_request`/`initialize` *first*, then answer each
  `can_use_tool` with a `control_response`. After each turn on a subscription login it also sends a
  `get_usage` control request and emits the answer as `agent.EventUsage` (claude 2.1.240+; older CLIs
  answer with an error and are not asked again). Verified against claude 2.1.240; `parse_test.go` fixtures
  pin the stream-json → `agent.Event` mapping and `testdata/session.jsonl` is a real captured session.
- **Definition vs instance.** `agent.Definition` is stored config; an instance is Definition +
  Environment + session id + thread. Definitions are seeded from config into the store on every start,
  so anything created from chat must *also* be written back to `config.toml` or it is lost on restart.
- **Config write-back preserves the file.** `config.AppendDefinition` / `ReplaceDefinition` /
  `RemoveDefinition` / `AppendChannel` edit `config.toml` textually so comments and formatting
  survive, validate the whole result, and restore the original if it fails to load. Use those helpers
  rather than `config.Save` for chat-driven changes.
- **Closing a thread is thread state, not task state.** `close` writes the `closed_threads`
  table (`store.SetThreadClosed`), which the coordinator mirrors in memory. A closed thread is
  skipped by `seedThreads` and `recover`, and `transport.ThreadCloser.Forget` tombstones it on
  Slack so a late "cancelled" notice cannot resurrect it. Any message a human addresses to the
  bot there reopens it. Nothing is ever deleted.
- **Graceful restart is a tested contract.** On SIGTERM: notify live threads, drain in-flight tool
  calls for `drain_timeout`, persist final state with a *non-cancelled* context, post "back" notices
  in `recover()`. `cancel` stays immediate. `make restart-drill` guards this.

## Conventions

- Only non-stdlib deps: `modernc.org/sqlite`, `slack-go/slack`, `BurntSushi/toml`. Adding a dependency
  is a decision — justify it in the PR and in the package doc of the package that uses it.
  The web UI (`internal/transport/web/ui`: React, HeroUI, Tailwind, react-markdown) is the one
  JavaScript toolchain; its Vite build is committed in `internal/transport/web/static` and embedded,
  so `go build` never needs Node. Run `make ui` after touching `ui/` and commit `static/`.
- Package docs carry the design rationale; keep them accurate when the contract changes.
- A feature's plan lives in its PR: a Progress checkbox list kept current as work lands, and the
  validation that was run (`make test-race`, `make e2e`, `make restart-drill`) named in the body.
- `.claude/worktrees/` holds other worktrees' full copies of the tree (gitignored). Scope greps and
  `find` to `cmd/ internal/` or exclude it, or you will read another branch's files as if they were
  yours.
