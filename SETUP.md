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
default agent; `run <agent> <prompt>` picks a specific one.

When the agent asks a question (Claude Code's `AskUserQuestion`), the thread
shows the options as buttons; click one, or reply in the thread with your own
answer. Multi-select questions are answered one option at a time for now. When the agent wants to run
something not pre-approved you get **Allow / Deny** buttons. Reply in the
thread to continue the conversation; `status`, `cancel`, `agents`, `help` work
anywhere. DMs to the bot work the same way without the mention.

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
