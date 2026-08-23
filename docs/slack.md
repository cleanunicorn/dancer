# Slack

dancer's first transport: a Slack app over Socket Mode. Outbound HTTPS is all the
machine needs — no public URL, no inbound ports.

Other transports: [Web UI](web.md). Every conversation is shared between them
(see [One conversation, every transport](#one-conversation-every-transport)).

## Create the app

1. Open https://api.slack.com/apps → **Create New App** → **From a manifest**.
2. Pick your workspace, paste `deploy/slack-manifest.yaml`, create.
3. **Basic Information → App-Level Tokens → Generate Token and Scopes**: name it
   `socket`, add scope `connections:write`, generate. Copy the `xapp-…` token.
4. **Install App → Install to Workspace**. Copy the **Bot User OAuth Token** (`xoxb-…`).
5. In Slack, invite the bot to a channel: `/invite @dancer`.

`bin/dancer setup` asks for both tokens and writes them to the config; or add them by hand.

## Configure

```toml
[server]
transports = ["slack"]          # add "web" to run the browser UI next to it

[slack]
app_token = "xapp-..."          # app-level token with connections:write
bot_token = "xoxb-..."          # Bot User OAuth token
allowed_users = []              # Slack user ids allowed to command dancer; empty = everyone

[[channels]]                    # optional: a default agent per channel
id = "C0123ABCDEF"              # channel details → "Channel ID"
agent = "sandbox"
```

`transports` defaults to `["slack"]` when the tokens are set. The config file holds
secrets, keep it `0600`. To find a user id for `allowed_users`: profile → **⋯** →
**Copy member ID**. `default <agent>` in a channel appends a `[[channels]]` block for
you; when a channel appears more than once the last block wins.

Run `bin/dancer doctor` after editing: it checks both tokens against Slack.

## Use it

| message                      | effect                                      |
|------------------------------|---------------------------------------------|
| `@dancer <prompt>`           | start a task with the channel's default agent; dancer replies in a thread under your message |
| DM the bot                   | same, without the mention                   |
| reply in the thread          | follow-up to that task (resumes if idle)    |
| attach a file or image       | copied into the agent's environment, path added to the message ([SETUP.md](../SETUP.md#files-to-the-agent)) |
| button / reply to a question | answers a permission or `AskUserQuestion` prompt |

The full command list (`run`, `default`, `agent add/edit/delete`, `close`, `status`,
`cancel`, `help`) is in the [README](../README.md#commands) and is the same on every
transport.

### What you see in a thread

- **Live status line** — the last message in the thread: `⏳ thinking · 4s`,
  `🔧 Bash \`go test ./...\` · 1m05s · 6 tool calls`, edited in place and moved below
  each new message. It is the only message dancer ever deletes.
- **Reactions on your message** — ⏳ while the agent works, ✋ while it waits for you,
  ✅ once the thread is closed (`reactions:write`).
- **Mentions** — the lines that need a human (prompts, the closing line, errors,
  "dancer is back" notices) tag whoever started the task, so you can mute the thread
  and still be told when to look. The agent's own text never tags anyone.
- **Agent text is Markdown** — rendered through a Block Kit `markdown` block, so
  headings, `**bold**` and fenced code look right. dancer's own lines are Slack mrkdwn.
- **Composer status** — "dancer ⏳ thinking · 4s" above the message box, when the app
  has the *Agents & AI Apps* feature (`assistant_view`, `assistant:write`, the
  `assistant_thread_*` events — all in the manifest). Optional: without it dancer
  logs one line and keeps the in-thread status line.

More on the thread lifecycle: [While the agent works](../SETUP.md#while-the-agent-works),
[Closing a thread](../SETUP.md#closing-a-thread).

## One conversation, every transport

A thread started in Slack belongs to dancer, not to Slack. The [web UI](web.md) lists
it, shows its history, and anyone signed in there can write into it or answer its
prompts:

- A message from the web arrives in the thread as `💬 *name* via web: …`.
- A decision made on the web settles the prompt's buttons here, as a click would.
- A thread a web user starts in one of the bot's channels is posted at top level by
  the bot and lives in Slack from then on.

Channel names in the web sidebar need `channels:read` and `groups:read` (in the
manifest); without them the id stands in.

## Scopes

All in `deploy/slack-manifest.yaml`. If you created the app before a scope was added,
add it under **OAuth & Permissions** and reinstall the app.

| scope | used for |
|-------|----------|
| `app_mentions:read`, `channels:history`, `groups:history`, `im:history` | reading mentions, thread replies and DMs |
| `chat:write`, `im:write` | posting, editing and deleting dancer's own messages |
| `reactions:write` | ⏳ / ✋ / ✅ on the root message |
| `files:read`, `files:write` | attachments to and from the agent |
| `users:read` | showing who wrote a message, on other transports |
| `assistant:write` | the composer status line (optional) |
| `channels:read`, `groups:read` | channel names in the web UI (optional) |

## A feed surface

A second surface on the same transport mirrors every task's start, approvals and
results into an ops channel, and lets you approve from there:

```toml
[[surfaces]]
name = "ops"
kind = "feed"
transport = "slack"
thread = "C0123456789/"     # channel id + "/" posts at top level
approvals = true
```

See [Surfaces](../SETUP.md#surfaces).

## Troubleshooting

- **Bot does not react to mentions** — reinstall the app after changing the manifest;
  make sure the bot was invited to the channel; check `journalctl -u dancer` for
  `slack connected`.
- **Buttons do nothing** — Interactivity and Socket Mode must both be on (the manifest
  enables them).
- **`doctor` marks a token ✘** — the `xapp-` token needs `connections:write`; the `xoxb-`
  token is the *Bot* User OAuth Token, not the user one.
- **Attachment "could not be fetched"** — the app lacks `files:read`, or the file is
  larger than dancer accepts.

More in [SETUP.md → Troubleshooting](../SETUP.md#troubleshooting).
