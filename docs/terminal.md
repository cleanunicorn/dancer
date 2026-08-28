# Terminal

A stdin/stdout transport: one constant thread, no Slack app, no browser. It is the
fastest way to try dancer or a chat change, and what `make e2e` drives.

Other transports: [Slack](slack.md), [Web UI](web.md).

## Start it

```sh
bin/dancer run -terminal        # the terminal transport alone, whatever the config says
make run-terminal               # same, from the repo (builds first)
```

`-terminal` overrides `server.transports`. To make it the configured transport instead:

```toml
[server]
transports = ["terminal"]
```

With no `transports` line at all, dancer picks `["terminal"]` when `slack.app_token` is
empty and `["slack"]` when it is set — so a config written before the Slack step just
works here.

## Use it

```
dancer terminal — type `help` for commands
> agents
> run coder create hello.py that prints hi, then run it
```

- Commands are the same as everywhere else (`run`, `default`, `agent add/edit/delete`,
  `status`, `cancel`, `close`, `help`; table in the [README](../README.md#commands)).
  There is no mention: type the text — including the agent's own commands, which nothing
  here can intercept: `/model opus`, `/clear`, `/compact`. `commands` lists them.
- The thread is always the same one, `terminal`. A plain line is a follow-up to the task
  on it, or starts one with the default agent when there is none; `run` while a task is
  running is refused ("a task is already running on this thread") — `cancel` first, or
  reply to it. `close` ends the thread; typing again reopens it.
- You are `local`: that is the `UserID` of everything you type, and what other transports
  show as the author.

### Prompts

A permission or question prompt ends with an input hint; the next line you type answers it:

```
Allow Bash `go test ./...`?
[allow/deny] > allow

Which database?
  1. SQLite
  2. Postgres
[1-2 or text] > 1
```

Type the number, the option's label, or — when the prompt allows free text — your own
answer. A line that matches nothing is sent as an ordinary message and the prompt stays
open. `Outbound.Mention` is ignored: there is one person here.

### Status line and files

The live status line (`⏳ thinking · 4s`, `🔧 Bash … · 6 tool calls`) is redrawn in place
on the last line when stdout is a terminal. When stdout is a pipe, every update is printed
as an ordinary line, so logs and `scripts/e2e.py` see each one.

Files the agent sends are listed as `📎 name (n bytes)`; nothing is written to disk.
Sending files to the agent is not supported here — use [Slack](slack.md).

## Next to the web UI

`transports = ["terminal", "web"]` works: the [web UI](web.md) lists the `terminal`
thread with everything else, and what a web user writes there arrives on your terminal
as `💬 name via web: …`, their decisions as `→ allow by name via web`. The terminal has no
channels of its own, so the web UI cannot start a thread *on* it — only follow the one
that exists.

## Testing

```sh
make e2e              # scripts/e2e.py: the whole binary through this transport
make restart-drill    # SIGTERM mid-tool-call → drain → resume, also through it
```

Both need a logged-in `claude`. They talk to dancer over a pipe and wait for the prompt
hints above (`[allow/deny] >`), so changing those strings means updating the scripts.
