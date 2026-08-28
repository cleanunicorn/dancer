# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

One operator (confirmed: the author, on a second monitor, all day) who runs several
coding agents at once and keeps one eye on them while doing other work. The job is
supervision, not conversation: know which threads need a human, answer a permission or
question prompt in one click, read a closing line, open a thread to read what the agent
did. Occasionally a second person writes in the same thread from Slack (the product
supports it; not the primary scene).

## Product Purpose

dancer orchestrates coding agents (Claude Code today) from chat. The web UI is one
transport onto the same conversations: every thread of every transport, Slack's included,
is listed, readable, and answerable here. It exists so the operator does not have to keep
Slack open to supervise agents, and so prompts can be answered from wherever the operator
is. Success: the operator sees at a glance what is running, what is waiting, what failed;
answers a prompt in under two seconds; and reads an agent's transcript without fatigue.

## Positioning

A conversation belongs to dancer, not to a transport. The web UI has no memory of its own;
it renders the event log the coordinator keeps, so what Slack shows and what the browser
shows are one record. Permission prompts are first-class and cross-surface: a prompt
rendered in Slack can be answered in the browser and the Slack buttons settle.

## Operating Context

- A single Go binary, self-hosted, plain HTTP on localhost (TLS is the operator's job).
- Threads live in channels; channels belong to a transport (`web`, `slack`). A thread
  hosted by Slack shows a "via slack" provenance; a human who wrote from Slack is named.
- While an agent works the thread carries one live status line edited in place
  (`🔧 Bash \`go test ./...\` · 1m05s · 6 tool calls`); the thread list marks it ⏳
  (working) / ✋ (waiting for a human). The tab title repeats the count.
- Message kinds, each a fact of the log: human text (plain, with links), agent text
  (Markdown), dancer's own lines (Slack mrkdwn: `*bold*`, `_italic_`, backticks, fences,
  leading @mention), prompts (permission: allow/deny; question: options, optional free text),
  decisions (who chose what, via which transport), file attachments (images inline, other
  files by name and size; bytes are not kept in the log).
- Commands typed into the composer are the same as in Slack: a bare prompt starts a task
  with the channel's default agent, `run <agent> <prompt>`, `default <agent>`, `status`,
  `cancel`, `close`, `agent list/add/edit/delete`. Guided wizards answer with prompts.
  A message that is one of the *agent's* own commands (`/model opus`, `/clear`, `/compact`,
  a plugin's) is not dancer's to read: it goes to the agent as typed, which is how all of
  them work without any being implemented. `commands` lists what the session accepts.
  Only a name the chat app runs itself needs care — Slack answers its own `/rename` and
  never delivers it, so that one is addressed to the bot first (`@dispatch /rename`).
- Accounts are local (`dancer user add`); the session's name signs what the user writes.

## Capabilities and Constraints

- Toolchain is fixed (confirmed): React 19, HeroUI 3, Tailwind 4, react-markdown, Vite;
  the build is committed under `internal/transport/web/static` and embedded in the Go
  binary. No new JavaScript dependency without a stated reason.
- Everything the UI shows comes from `GET /api/...` and a `/api/events` stream defined in
  `internal/transport/web/web.go`; the redesign changes rendering, not the API.
- The UI must keep: login, change-password, the command help, thread list grouped by
  transport and channel with a "new thread" action, the live status line, unread counts,
  the "new messages ↓" affordance, prompt cards that settle once answered (by anyone, from
  any transport), image attachments inline, keyboard: Enter sends, Shift+Enter newline,
  `/` focuses the composer, Esc closes.
- Mobile layout must still work (the sidebar collapses), but the desktop second-monitor
  scene is primary.
- Light/dark: system preference is the switch today. Not pinned by the user; the chosen
  world decides, but both must remain usable.

## Brand Commitments

- Name: `dancer`, lowercase. The 🕺 glyph is the favicon and the only mark today; not
  pinned, may be replaced by a drawn mark.
- Voice (from the README and Slack lines): terse, lowercase-ish, operator-to-operator
  ("task started with agent *coder* (local)", "✅ done · 2m13s · 7 tool calls").
- Confirmed pain with the current UI: "generic / looks like a template". The result must
  be recognizable as dancer with the content removed.

## Evidence on Hand

- Real transcripts are whatever is in the operator's own event log; no demo data ships.
- Status-line and closing-line formats: README.md and `internal/surface/chat`.
- No logo, no illustration, no screenshots to reuse.

## Product Principles

1. Glanceable before readable: state (working / waiting / failed / done) is visible from
   across the room; detail is one click away.
2. Answering a prompt is the most important action on the page; it is never buried.
3. Provenance is always visible: which transport, which human, which agent.
4. The log is the truth; the UI adds no state the coordinator does not know.
5. Dense is fine, noisy is not: an all-day surface earns trust by being quiet.

## Accessibility & Inclusion

Keyboard-operable throughout (composer shortcuts above, focus order through prompt
buttons). Colour never the sole carrier of state (each state has a glyph or word).
