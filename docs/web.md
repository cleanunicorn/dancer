# Web UI

A browser transport: dancer serves a single-page chat UI and streams it every message.
It is an *observer* — it shows every conversation dancer has, whichever transport hosts
it — so one dancer serves the browser and [Slack](slack.md) at the same time.

Other transports: [Slack](slack.md), [Terminal](terminal.md).

## Configure

```toml
[server]
transports = ["slack", "web"]   # or just ["web"] for a dancer without Slack

[web]
listen = "127.0.0.1:8788"       # plain HTTP — keep it on localhost or put TLS in front
channels = ["general"]          # the web UI's own channels, for threads that do not live in Slack

[[channels]]                    # optional: a default agent for a web channel
transport = "web"
id = "general"
agent = "coder"
```

- `listen` defaults to `127.0.0.1:8788`. The server speaks plain HTTP; to reach it from
  another machine, put a TLS-terminating reverse proxy in front rather than binding
  `0.0.0.0`.
- `channels` are one word each, no slash. They exist only in the web UI; a thread started
  in one is `"<channel>/<id>"`. Leave the list empty if every thread should live in Slack.
- `bin/dancer run -web` starts the web transport alone, whatever `transports` says
  (`make run-web` does the same from the repo).

Already running Slack? Add `"web"` to `transports`, add the `[web]` block, create an
account (below), restart dancer (`sudo systemctl restart dancer`). Existing Slack threads
appear in the sidebar at once — their history is read from dancer's log.

## Accounts

Accounts live in dancer's database, not in the config:

```sh
bin/dancer user add daniel          # prints a generated password; or: user add daniel <password>
bin/dancer user passwd daniel       # new password, ends the user's sessions
bin/dancer user rm daniel
bin/dancer user list
```

There is no anonymous mode. Passwords are stored as PBKDF2 hashes and sessions by token
hash; you can change your own password from the UI. The account name is the identity of
everything you send: the agent's closing line and prompts address you by it, and two
people in one thread are two names.

## Use it

Open http://127.0.0.1:8788 and sign in.

- The sidebar lists channels per transport (Slack channels by name when the app has
  `channels:read`/`groups:read`, else by id) and their threads. ⏳ means the agent is
  working, ✋ that it waits for an answer; the tab title shows the same, plus an unread
  count, and a browser notification fires when a prompt needs you, if you allowed them.
- Pick a thread to read it, write in it, or answer its prompts. Pick a channel to start a
  new thread there — in a Slack channel, the bot posts your text at top level and the
  thread lives in Slack from then on.
- Commands are the same as on every other transport (`?` in the header lists them; the
  table is in the [README](../README.md#commands)).
- Attachments: a thread's past attachments are shown by name only (dancer never logs file
  bytes). Sending files from the browser is not supported yet; use Slack for that.

### What the web UI remembers

Nothing a reload could lose. Channel and thread lists and every message come from the
coordinator's event log whenever a page asks; what arrives live is pushed to open pages
over server-sent events. The only in-memory state is the moment: the status line of a
running turn, whether a prompt is open, and threads opened here that have no task yet.

## One conversation, every transport

A conversation belongs to dancer, not to the transport that started it. What you write
here about a Slack thread is relayed into Slack as `💬 *name* via web: …`; a decision you
make here settles the prompt's buttons there; and a decision made in Slack closes the
prompt here. The log keeps one record per message — the inbound — never the relays, so
each transport renders the whole exchange its own way. Details from the Slack side:
[slack.md → One conversation, every transport](slack.md#one-conversation-every-transport).

## Developing the UI

The UI is a React + [HeroUI](https://heroui.com) app under `internal/transport/web/ui`
(Vite, TypeScript, Tailwind; react-markdown renders the agent's Markdown) — the one place
in dancer with a JavaScript toolchain. Its build is committed under
`internal/transport/web/static` and embedded, so `go build` needs no Node.

```sh
make ui-dev     # live dev server against a running dancer
make ui         # rebuild static/ after changing ui/ — commit the result
make run-web    # dancer with the web transport only
```

## Troubleshooting

- **"config: web.listen … "** — `listen` must be `host:port`; `:8788` and `127.0.0.1:8788`
  both work.
- **"config: web channel … one word, no slash"** — channel names cannot contain `/`,
  spaces or tabs.
- **Login fails** — `bin/dancer user list` shows the accounts; `user passwd` resets one.
  Make sure dancer and the `user` command use the same database (`$DANCER_CONFIG`).
- **Slack channels show as ids** — add `channels:read` and `groups:read` to the Slack app
  and reinstall it.
- **No browser notifications** — the page asks once; check the site's notification
  permission in the browser.
