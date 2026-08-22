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

One Go binary (`cmd/dancer`: `run` | `setup` | `doctor`). Data flows in one loop:

```
transports --Inbound--> surfaces --Intent--> Coordinator --Task--> Executor --> Agent --> Environment
transports <-Outbound-- surfaces <--Event--- Coordinator <-----agent.Event------------------┘
                                                 ↓
                                            Store (SQLite event log + projections)
```

Each layer is an interface defined in the package doc of `internal/<pkg>/<pkg>.go`; read those
files first — they carry the contract, the concrete packages under them are implementations.

- **`transport`** (slack, terminal) — dumb on purpose: text, prompt-with-choices, `ThreadID`
  (Slack: `"<channel>/<thread_ts>"`, `"<channel>/"` posts top level). It never interprets a message.
- **`surface`** (chat, feed) — everything about *how* humans interact. `Handle` turns an inbound
  message into `[]Intent` (returning `ok=false` passes it to the next surface on that transport);
  `Render` turns a coordinator `Event` into outbound messages. Several surfaces share one transport,
  so a new interaction style on Slack is a **new surface, not a new Slack client**.
- **`coordinator`** — the only stateful brain: intents → tasks, event fan-out to every surface,
  permission/question decision relay (`pending`/`askText` maps keyed by prompt id), guided wizards
  (`wizard.go`: add/edit/delete agent, the bare-`run` agent picker), restart recovery.
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
  namespaced per surface and resolved on a base id in the coordinator.
- **`AskUserQuestion` reuses the same path** as permissions, via `EventQuestion` + `Question.Answers`.
- **The Claude handshake is protocol-sensitive** (`internal/agent/claude`): spawn with
  `--permission-prompt-tool stdio`, send a `control_request`/`initialize` *first*, then answer each
  `can_use_tool` with a `control_response`. Verified against claude 2.1.239; `parse_test.go` fixtures
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
- Package docs carry the design rationale; keep them accurate when the contract changes.
- A feature's plan lives in its PR: a Progress checkbox list kept current as work lands, and the
  validation that was run (`make test-race`, `make e2e`, `make restart-drill`) named in the body.
- `.claude/worktrees/` holds other worktrees' full copies of the tree (gitignored). Scope greps and
  `find` to `cmd/ internal/` or exclude it, or you will read another branch's files as if they were
  yours.
