# Dancer — agent orchestration plan

Orchestrate multiple coding agents (Claude Code first, Codex later) from Slack.
One Go binary: Coordinator + in-process Executors, SQLite event log.

## Progress

Planning
- [x] Architecture review and language choice (Go; agents driven via `claude -p --output-format stream-json`)
- [x] Module layout and interface definitions
- [x] Split "channel" into Transport (the wire) and Surface (the interaction on it)
- [ ] Slack app created from `deploy/slack-manifest.yaml` and tokens pasted into config (needs: Daniel, Slack workspace admin)

Milestone 1 — walking skeleton (local folder + terminal) ✅
- [x] `environment/local`: exec with stdio pipes, CopyIn/CopyOut (unit test)
- [x] `agent/claude`: spawn `claude -p`, parse stream-json → `agent.Event`, permission handshake (fixture test + live test)
- [x] `store/sqlite`: event log + task/definition tables (unit test)
- [x] `executor/local`: worker per task, idle timeout, permission relay, cancel (unit test)
- [x] `coordinator`: intents → tasks, event fan-out to surfaces, decisions, recovery on restart (unit test, two surfaces on one transport)
- [x] `transport/terminal` + `surface/chat`
- [x] `cmd/dancer run`
- [x] End-to-end through the real binary: `run coder …` → permission prompt → `allow` → result → `status` → follow-up (`scripts/e2e.py`, passes)

Milestone 2 — Slack
- [x] `transport/slack`: Socket Mode, thread per task, Block Kit buttons for prompts, allowed-user filter
- [x] Permission round-trip: `needs_permission` → buttons → `Decide` (logic covered by coordinator test; Slack wire untested)
- [x] `surface/feed`: mirror starts/approvals/results to a fixed thread (second surface on the same transport)
- [ ] Live validation against a real Slack workspace (needs tokens from Daniel)
- [ ] Slash command `/dancer` (optional; mentions + DMs work without it)

Milestone 3 — environments ✅
- [x] `environment/docker`: container per task, workdir bind mount at /work, runs as host uid, HOME=/tmp (test against real daemon)
- [x] `environment/ssh`: `ssh` CLI, quoted remote commands, CopyIn/CopyOut (test against a throwaway local sshd)
- [x] Default `PermissionMode` per environment kind (docker → acceptEdits, else manual)
- [ ] Live `claude` inside docker / over ssh (needs an image with claude + a token; see SETUP.md)

Milestone 4 — definitions and instances
- [x] Definitions seeded from config into the store; `agents` lists them
- [x] Sub-agents passed through as `--agents` JSON (`sub_agents` in config; untested live)
- [x] Multiple instances of one definition run concurrently (one task per thread)
- [ ] Edit definitions from Slack (currently config-file only)

Milestone 5 — deploy-ready on Linux
- [x] `dancer setup` wizard: storage, claude check, Slack tokens, first definition, then runs doctor
- [x] `dancer doctor`: config, workdir, claude auth, docker, ssh hosts, Slack auth, surfaces
- [x] `deploy/dancer.service` + `make service-install`
- [x] `SETUP.md`: Slack app manifest, install steps, first run, docker/ssh notes
- [ ] Validate on this machine: wizard → `make service-install` → Slack message → result (needs Slack tokens)

Deferred
- [ ] `agent/codex` (`codex exec --json`)
- [ ] Browser access via MCP server in `mcp_config`
- [ ] Telegram transport
- [ ] Executors as separate processes on other hosts
- [ ] Streaming partial text to Slack (`--include-partial-messages` + message edits)

## Architecture

```
              Surfaces (chat, feed)                    Executor ──▶ Agent ──▶ Environment
Transports ──Inbound──▶ Handle ──Intent──▶ Coordinator ──Task──▶ (claude)    (local/docker/ssh)
(slack,     ◀─Outbound── Render ◀──Event──     │            ◀──agent.Event──┘
 terminal)                                     ▼
                                             Store (SQLite event log + projections)
```

| Package       | Interface              | What it does                                                        |
|---------------|------------------------|---------------------------------------------------------------------|
| `transport`   | `Transport`            | The wire: Slack, terminal. Moves messages, knows thread addressing. |
| `surface`     | `Surface`              | An interface on a transport: parses intent, decides what to render. Several per transport. |
| `coordinator` | —                      | Owns tasks, runs intents, fans events to surfaces, relays decisions, persists. |
| `executor`    | `Executor`, `Sink`     | Runs one task: provisions env, starts agent, relays permissions, idle timeout. |
| `agent`       | `Agent`, `Run`         | Claude Code over stream-json, normalized to `agent.Event`.          |
| `environment` | `Environment`, `Factory` | Where a process runs: local dir, docker container, ssh host.      |
| `store`       | `Store`                | Append-only record log + task/definition projections.               |

Surfaces shipped: `chat` (commands + thread follow-ups + approvals + results) and
`feed` (mirrors starts/approvals/results into one fixed thread, e.g. #ops).

## Decisions

1. **Go, no Agent SDK.** The SDK is TS/Python only and in-process, which cannot reach
   Docker/SSH anyway. `claude -p --output-format stream-json --input-format stream-json`
   is the seam; it works identically over `exec`, `docker exec`, and `ssh`.
2. **Event log is the source of truth.** Every inbound/outbound/agent event is appended.
   Task state is a projection. Crash recovery = mark live tasks idle, `--resume <session>`
   on the next message.
3. **Permission prompts are first-class.** `agent.EventNeedsPermission` → `surface.Event`
   (PromptID) → `transport.Prompt` → `transport.Decision` → `surface.Decide` →
   `agent.PermissionDecision`. Any surface that rendered the prompt can answer it.
4. **Definition vs instance.** `agent.Definition` is stored config; an instance is
   Definition + Environment + session id + thread.
5. **One binary first.** Coordinator and executors are interfaces, not services. Split
   only when an executor must run on another host.
6. **Permission handshake (verified against claude 2.1.239).** Spawn with
   `--permission-prompt-tool stdio`, send `{"type":"control_request","request":{"subtype":"initialize"}}`
   first; the CLI then emits `control_request`/`can_use_tool` and waits for
   `control_response` `{behavior:"allow",updatedInput}` or `{behavior:"deny",message}`.
7. **Environments shell out to `docker` and `ssh` CLIs.** No Docker SDK / x/crypto/ssh:
   fewer deps, and the user's existing ssh config/agent and docker context just work.
   Only non-stdlib deps: `modernc.org/sqlite`, `slack-go/slack`, `BurntSushi/toml`.
8. **Transport vs Surface.** A transport is dumb on purpose (text, prompt-with-choices,
   thread id). Everything about *how* humans interact lives in surfaces, so a second
   interaction style on Slack is a new surface, not a new Slack client.
9. **A finished turn keeps its process alive for `idle_timeout`** (default 10m) so
   follow-ups are instant; after that the session is resumed with `--resume`.

## Claude stream-json mapping

| stream-json                        | `agent.Event.Type`     |
|------------------------------------|------------------------|
| `system` / `subtype: init`         | `init` (sets Session)  |
| `assistant` text block             | `text`                 |
| `assistant` tool_use block         | `tool_use`             |
| `user` tool_result block           | `tool_result`          |
| `control_request` / `can_use_tool` | `needs_permission`     |
| `result`                           | `result` (Cost set) or `error` when `is_error` |
| process exit non-zero / parse err  | `error`                |

`parent_tool_use_id` on `assistant` messages → `Event.ParentID` (sub-agent activity).

## Validation log

| What                                   | How                                             | Result |
|----------------------------------------|-------------------------------------------------|--------|
| stream-json parser                     | `go test ./internal/agent/claude` (fixture)     | pass   |
| claude live: start → permission → allow → follow-up → resume | `DANCER_LIVE=1 go test ./internal/agent/claude` | pass (15s, haiku) |
| local / docker / ssh environments      | `go test ./internal/environment/...`            | pass (real container, throwaway sshd) |
| executor idle/resume/cancel            | `go test ./internal/executor/...`               | pass   |
| two surfaces on one transport          | `go test -race ./internal/coordinator`          | pass   |
| whole binary via terminal              | `scripts/e2e.py` (run→allow→done→status→follow-up) | pass |
| Slack wire                             | —                                               | pending tokens |
