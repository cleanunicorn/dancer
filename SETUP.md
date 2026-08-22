# Setting up dancer on a Linux machine

About 20 minutes. You need: a Linux box (or VM) you can run a service on,
Go 1.25+ to build, Claude Code installed and logged in, and admin rights on a
Slack workspace.

## 1. Build

```sh
git clone https://github.com/cleanunicorn/dancer && cd dancer
make build          # -> bin/dancer
```

## 2. Install and log in to Claude Code (as the user that will run dancer)

```sh
curl -fsSL https://claude.ai/install.sh | bash   # or see https://code.claude.com/docs/en/setup
claude                                           # then type /login and follow the browser flow
claude -p --model haiku "Reply with OK"          # prints OK when authenticated
```

dancer drives `claude -p` as a subprocess; it uses whatever login that user has.

## 3. Create the Slack app

1. Open https://api.slack.com/apps → **Create New App** → **From a manifest**.
2. Pick your workspace, paste `deploy/slack-manifest.yaml`, create.
3. **Basic Information → App-Level Tokens → Generate Token and Scopes**:
   name it `socket`, add scope `connections:write`, generate. Copy the `xapp-…` token.
4. **Install App → Install to Workspace**. Copy the **Bot User OAuth Token** (`xoxb-…`).
5. In Slack, invite the bot to a channel: `/invite @dancer`.

The manifest enables Socket Mode, so the machine needs outbound HTTPS only —
no public URL, no inbound ports.

To find your Slack user ID (for `allowed_users`): click your profile → **⋯** →
**Copy member ID**.

## 4. Run the wizard

```sh
bin/dancer setup
```

It asks for the database/workdir paths, checks `claude`, takes the two Slack
tokens, defines your first agent, writes `~/.config/dancer/config.toml`, then
runs `dancer doctor`. Fix anything marked ✘ and re-run `bin/dancer doctor`.

You can edit the file by hand afterwards — `deploy/config.example.toml` shows
every option, including docker and ssh environments and a second surface.

More agents can be added later from Slack (or the terminal) without a
restart: see [Managing agents from chat](#managing-agents-from-chat).

## 5. Try it

Terminal first (no Slack needed):

```sh
bin/dancer run -terminal
> agents
> run coder create hello.py that prints hi, then run it
```

Then with Slack:

```sh
bin/dancer run
```

In the channel you invited the bot to:

```
@dancer create hello.py that prints hi, then run it
```

dancer answers **in a thread under your message** (look for "1 reply"), using the
channel's default agent; `run <agent> <prompt>` picks a specific one. Slack
has no autocomplete for text after a mention, so a bare `@dancer run` posts
the agent list as buttons (a searchable menu from five agents up) and then
asks for the prompt in the thread; `run <agent>` alone skips straight to the
prompt question.

Each channel can have its own default agent: `@dancer default reviewer` in
`#code-review` makes plain messages there run *reviewer*. That appends a
`[[channels]]` block to `config.toml` (you can also write one by hand with
the channel id from the channel's details pane); `default` shows the
current one and `agents` marks it. Without a channel default,
`server.default_agent` applies.

When the agent asks a question (Claude Code's `AskUserQuestion`), the thread
shows the options as buttons; click one, or reply in the thread with your own
answer. Multi-select questions are answered one option at a time for now. When the agent wants to run
something not pre-approved you get **Allow / Deny** buttons. Reply in the
thread to continue the conversation; `status`, `cancel`, `agent list` (or `agents`), `help` work
anywhere. DMs to the bot work the same way without the mention.

## Managing agents from chat

```
@dancer agent add
```

dancer asks, one question per message in the thread: name, model, where it
runs (local / docker / ssh and the image, host or directory that goes with
it), permission mode, pre-approved tools (presets or a comma-separated list)
and an optional system prompt, then shows a summary with **Save / Cancel**.
Click a button or type the answer; `cancel` at any point abandons the flow.
Type paths in backticks (`` `/home/me/app` ``): Slack refuses to send a
message that starts with `/` because it looks like a slash command.
Answers are saved as you go, so a dancer restart mid-way re-asks the next
question when it is back.

Saving appends a `[[definitions]]` block to `config.toml` (the rest of the
file is left untouched) and registers the agent immediately — `agents` lists
it and `run <name> <prompt>` works without a restart. Settings the flow does
not ask for (`sub_agents`, `mcp_config`, `key_path`, container `env`) can be
added to that block by hand afterwards.

`@dancer agent edit <name>` shows the agent's settings with a menu — model,
environment, permissions, tools, system prompt — re-asks the field you pick,
and on **Save** rewrites that agent's `[[definitions]]` block (tasks already
running keep their old settings). `@dancer agent delete <name>` asks for a
confirmation and removes the block; an agent that is the global
`default_agent` or a channel default is refused until the default points
elsewhere. Both pick the agent from a list when the name is left out.

## 6. Install as a service

```sh
make service-install   # installs /usr/local/bin/dancer and a systemd unit for the current user
make service-logs
```

`make service-install` substitutes your user, group and home into
`deploy/dancer.service`, enables it and starts it. The unit runs as *your*
user so it finds your Claude Code login and ssh/docker config. Edit
`/etc/systemd/system/dancer.service` if the binary or config live elsewhere.

Remove with `make service-uninstall`. Rebuild and restart after a code change with `make service-restart`; logs with `make service-logs`.

## Files from the agent

When the agent mentions a file path in its reply (`/tmp/settings-top.png`,
`out/report.pdf`) and the file exists in its environment, dancer uploads it
into the thread — images and PDFs show inline. Agents are told this in their
system prompt, so "send me a screenshot" works. Up to 10 files per message,
20 MiB each. Requires the `files:write` bot scope (in the manifest; if you
created the app before it was added, add the scope and reinstall the app).

## Restarting dancer

`make service-restart` (or Ctrl-C / `systemctl restart dancer`) is safe while
agents run:

1. Every live thread gets "⏸️ dancer is restarting".
2. Agents that are in the middle of a tool call (a test run, a build) are given
   `drain_timeout` (default 2m) to finish that call; then their processes stop.
   Files in the workdir stay.
3. On start, every task that was *mid-execution* is resumed (`--resume`) on its
   own and told to carry on — you do not have to type anything in the threads. A
   task that never got as far as a session is simply run again from its original
   message.
4. A task whose agent had already answered is left alone: its process was only
   being kept alive for a follow-up (`idle_timeout`), so the stop cut nothing
   short and the thread is still waiting for you, not for dancer.

`status` shows such tasks as *interrupted* until they are picked up. `cancel` is
still immediate. The systemd unit's `TimeoutStopSec` is set above `drain_timeout`.

Auto-resume is on by default and tunable in `[server]`:

| key                  | default | what it does                                         |
|----------------------|---------|------------------------------------------------------|
| `auto_resume`        | `true`  | `false` goes back to "▶️ dancer is back — reply in this thread to continue" |
| `auto_resume_within` | `12h`   | tasks last touched longer ago than this wait for a reply instead |
| `max_auto_resumes`   | `3`     | consecutive automatic resumes of one task before it waits for a human — a task that keeps taking dancer down cannot restart-loop |
| `resume_prompt`      | built-in | the message an auto-resumed session is given          |

The counter behind `max_auto_resumes` is cleared as soon as a resumed agent
finishes a turn.

## The decider (optional)

Dancer's restart rules are blunt on purpose: everything cut mid-execution is
picked up with the same "carry on" sentence. Switching on a decider hands those
judgement calls to a small model — it can leave a task for you with a reason, or
word the resume in the task's own terms. See [DECIDER.md](DECIDER.md).

```toml
[decider]
kind = "claude"       # off (default) | claude
model = "haiku"
uses = ["resume"]     # question kinds it may answer; [] = never asked
timeout = "15s"
max_per_task = 20
```

It can only ever narrow what the rules already allow: `auto_resume_within`,
`max_auto_resumes` and a definition's `allowed_tools` are applied before it is
asked, and an answer outside the offered options is discarded. If it fails,
times out or is not configured, dancer behaves exactly as it does without one.
Every verdict, with its reason, is in the event log and in `status`.

## Environments

**local** — the agent runs on the dancer host in `workdir` (or a fresh
directory under `workdir_root` per task).

**docker** — one container per task from `image`, with `workdir` mounted at
`/work`, run as your uid so files stay yours. The image must contain `claude`
and needs credentials, since the container has no `~/.claude` login:

```toml
[definitions.environment]
kind  = "docker"
image = "my/claude-dev:latest"
[definitions.environment.env]
CLAUDE_CODE_OAUTH_TOKEN = "…"     # from `claude setup-token` on a logged-in machine, or use ANTHROPIC_API_KEY
```

A minimal image:

```Dockerfile
FROM node:22-slim
RUN apt-get update && apt-get install -y git curl ca-certificates && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://claude.ai/install.sh | bash && ln -s /root/.local/bin/claude /usr/local/bin/claude
```

**ssh** — the agent runs on `host` in `workdir`; `claude` must be installed and
logged in there for the ssh user. `dancer doctor` checks both. Uses your
`~/.ssh/config`, agent and known_hosts; set `key_path` to pin a key.

## Permission modes

| mode                | behaviour                                                        |
|---------------------|------------------------------------------------------------------|
| `manual` (default)  | every tool not in `allowed_tools` asks in Slack                  |
| `acceptEdits`       | file edits auto-approved, commands still ask                     |
| `auto`              | Claude Code's own heuristics                                     |
| `bypassPermissions` | never asks — only for throwaway containers                       |

`allowed_tools` takes Claude Code rule syntax: `Read`, `Edit`, `Bash(git:*)`, `Bash(npm test)`.

## Surfaces

A *transport* is the wire (Slack, terminal). A *surface* is an interface on it.
Default: one `chat` surface per transport. Add a `feed` to mirror every task's
start, approvals and results into an ops channel — and approve from there:

```toml
[[surfaces]]
name = "chat"
kind = "chat"
transport = "slack"

[[surfaces]]
name = "ops"
kind = "feed"
transport = "slack"
thread = "C0123456789/"     # channel id + "/" posts at top level
approvals = true
```

## Troubleshooting

- **`claude` check fails in doctor** — run `claude` interactively as the service user and `/login`. If dancer runs under systemd, the unit's `HOME=` must point at that user's home.
- **Bot does not react to mentions** — re-install the app after changing the manifest; make sure the bot was invited to the channel; check `journalctl -u dancer` for `slack connected`.
- **Buttons do nothing** — Interactivity must be on (the manifest enables it) and Socket Mode must be enabled.
- **Permission prompt never appears, task fails with `permission_denials`** — the definition's `permission_mode` is not `manual`/`acceptEdits`, or `claude` is older than 2.1; dancer needs the `--permission-prompt-tool stdio` handshake.
- **Files in the docker workdir owned by root** — containers run as your uid:gid by default; this only happens if the docker factory `User` was overridden to `root`.
- Everything dancer saw is in the SQLite `log` table: `sqlite3 ~/.config/dancer/dancer.db 'select seq,kind,substr(payload,1,120) from log order by seq desc limit 20'`.
