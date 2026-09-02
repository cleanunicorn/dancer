# WORKFLOWS.md — the design and the plan

*Written to match the shape of `DECIDER.md`: the design and the plan in one
file, kept current as work lands. All of it is implemented.*

## Progress

- [x] Read the existing seams (`finish.go`, `internal/work`, `coordinator.drive`, wizards, decider)
- [x] Review folded in: the turn id is a floor, a workflow can be stopped, resume grades before
      it resends, `report` is graded on the result, the model pin is restored, retry is bounded,
      prompts are rendered before they are sent
- [x] M0 — a turn has an id (`internal/coordinator/turns.go`: ends carry a result seq, waiters
      hold a floor, `Done` settles a waiter whose turn wrote no record)
- [x] M1 — `internal/workflow`: schema, validation, templating, judging, state, tests
- [x] M2 — config `[[workflow]]` + the `workflows` table (`store.PutWorkflow`/`ListWorkflows`)
- [x] M3 — the runner (`internal/coordinator/workflow.go`); `review`/`merge` re-expressed as
      quiet one-step runs of it (`finish.go` keeps the words' refusals and lines)
- [x] M4 — `workflows`, `workflow <name> <ask>`, status rendering, feed
- [x] M5 — gates (cross-surface, persisted, re-asked after a restart)
- [x] M6 — restart resume (grades before it resends) + `make restart-drill` workflow leg
- [x] M7 — the planner: on-demand workflows from a message (`plan <what you want, and how>`),
      shown and confirmed before anything runs, refused by the same `workflow.Validate` a config
      workflow goes through, planning in an empty room with no tools
- [x] M8 — `workflow save <name>` write-back (`config.AppendWorkflow`) + docs

---

## Can dispatch do this?

Yes, and most of it already exists under another name.

`internal/coordinator/finish.go` is a two-step workflow engine with the steps hard-coded:

- `review` opens a thread *beside* this one (`transport.ThreadOpener`), starts an agent there
  with a prompt built out of `internal/work`, and posts the link home.
- `merge` runs off the inbox goroutine, sends a templated prompt to the thread's agent,
  **waits for that turn to end** (`awaitTurnEnd` / `waitForTurn`), then **reads the log back**
  (`work.State.Merged`) to decide whether the step actually happened — and closes the thread on
  that evidence and nothing else.

Ask a named agent something → wait for its turn → read the log back → decide the next step.
That is the whole primitive. A workflow is that loop with the steps in a list instead of in Go.

The second half — evidence — is the part dispatch has that a generic orchestrator does not.
`internal/work` already grades what a thread did from its own records: a PR it opened, a branch
it pushed, a merge it performed. So a step can be *verified* rather than trusted, exactly the way
`merge` is today: an agent that says "done" without the log agreeing advances nothing.

---

## What a workflow is

A **workflow** is an ordered list of **steps**, run on a **home thread**, carrying **state**.

A **step** is:

| field     | meaning |
|-----------|---------|
| `name`    | how the step is referred to in later prompts and in the status line |
| `agent`   | definition name; empty = the thread's current agent |
| `model`   | override for this step only, restored when the step ends (a leaked `ModelPin` would outlive its step) |
| `thread`  | `same` (follow-up on the home thread) or `new` (a fresh thread beside it, fresh session) |
| `prompt`  | Go template over the workflow's state |
| `builtin` | `review` / `merge` — the words that already exist, as steps |
| `expect`  | what the log must show: `pr`, `push`, `merged`, `report`, `judge`, `none` |
| `check`   | a command dispatch runs itself in the step's environment; exit 0 or the step failed |
| `gate`    | a question for a human instead of a turn for an agent |
| `on_fail` | `ask` (default) · `retry` (bounded by `max_retries`, default 2) · `stop` |

Nothing else. No conditionals, no loops, no parallelism in v1 — a linear list with a human gate is
already the whole of the example that prompted this, and every added construct is a construct that
has to survive a restart.

Four rules a step carries that the table cannot say:

- `pr`, `push` and `merged` are `internal/work` sightings, graded on the records the step itself
  produced. A `report` is not something a log can be mined for — every turn ends in text — so it
  is graded on the `EventResult` itself: the turn ended, not in error, with something to say.
  `none` asks nothing and is satisfied by the turn ending — but a turn that never *did* end (a
  restart cut it short; the window holds no result) fails any step. A `check` is observed, not
  mined: dispatch execs it in the step's own environment. `judge` asks a decider — whose static
  answer is `ask`, so without one it becomes a human question.
- `retry` retries up to `max_retries` (twice by default), then takes the ordinary failure path
  (`ask`, by default) — an unbounded retry loop is a bill, not a workflow.
- a per-step `model` lands as `TaskState.ModelPin`, which persists per task, so a `same`-thread
  step that overrides the model puts the previous pin back when the step ends; a pin that
  outlived its step would change every later step on that thread.
- a prompt is rendered before anything is sent, and a render over state that is not there —
  `{{.PR}}` before any pull request exists — never reaches an agent as "Review .": the step takes
  its `on_fail`. Step names are unique or `Validate` refuses the definition —
  `{{.Steps.<name>…}}` is a lookup by name, and two of one name means whatever the array happened
  to keep.

### The example, in config

```toml
[[workflow]]
name = "feature"
description = "implement → review with a second model → fix → approve → merge"

  [[workflow.step]]
  name   = "implement"
  agent  = "coder"
  thread = "same"
  prompt = "{{.Ask}}\n\nWhen the change is done and the tests pass, open a pull request."
  expect = "pr"

  [[workflow.step]]
  name   = "review"
  agent  = "reviewer"          # a different definition = a different model, or a different kind entirely
  thread = "new"               # a reader who has to get its opinion from the diff
  prompt = "Review {{.PR}}. Report only — do not push, commit or merge."
  expect = "report"

  [[workflow.step]]
  name   = "fix"
  thread = "same"
  prompt = """A second reviewer said this about {{.PR}}:

{{.Steps.review.Report}}

Fix what is real and push. Say plainly what you rejected and why."""
  expect = "push"

  [[workflow.step]]
  name = "approve"
  gate = "{{.PR}} has been reviewed and fixed. Merge it?"

  [[workflow.step]]
  name    = "merge"
  builtin = "merge"
  expect  = "merged"
```

Started with `workflow feature <what you want built>`. `{{.Ask}}` is that message.

### Template state

```
.Ask                     what the human asked for when the workflow started
.Repo .Branch .PR .Issue internal/work, re-read before every step
.Steps.<name>.Report     the last assistant text of that step's turn
.Steps.<name>.Thread     the thread that step ran on (a link, for the home thread's log)
```

Read before every step, never cached: the PR number exists only after step 1 ran.

---

## Where it lives

```
internal/workflow/        pure: Definition, Step, State, Validate, Next, Render — no coordinator, no I/O
internal/coordinator/workflow.go   the runner: the loop finish.go already has, over a list
```

The split follows `internal/work` and `internal/decider`: the judgement is a package that can be
tested without an agent CLI, the driving is coordinator code that owns goroutines and locks.

**Not a surface.** A surface is about how humans interact; a workflow is about what dispatch does
with tasks. It belongs beside the coordinator, with the tasks.

**The runner runs beside the inbox, not on it.** `merge` already works on its own goroutine
because a thread must keep hearing everything while its turn runs, and a workflow runner lives
for possibly an hour and owes the home thread the same. A human typing on the home thread
mid-workflow is a follow-up the runner did not ask for — the turn id is what keeps it from being
mistaken for the step's turn. And `cancel` grows the workflow: it stops the turn and the run,
written as a record, so a restart cannot resume a workflow a human stopped.

**The log is still the source of truth.** A `workflow` record per step transition; `WorkflowState`
is a projection over those, so a restart replays into the same place — the same contract as
`store.FlowState` for the wizards, and resumed next to `resumeFlows`. Resuming grades before it
resends: a step whose turn the restart cut short is graded on its own window first — if the
records before the crash already satisfy `expect`, the step happened and the run advances;
otherwise the step failed and takes its `on_fail`. Re-sending would duplicate work a half-finished
turn may already have done, and waiting would wait forever.

**`review` and `merge` are re-expressed as built-in steps** rather than kept as a parallel path.
They stay bare words in chat; the words become one-step workflows. That is the test that the
abstraction is real, and it stops there being two ways to run a turn and read the log back.

---

## On-demand workflows (the planner)

The second type: say what you want and how you want it done, and dispatch composes the workflow.

```
plan add a health endpoint, have a second model review it, then let me approve the merge
```

`internal/coordinator/plan.go` hands the ask, the vocabulary and the agents that exist to
`server.planner_agent` and gets a workflow back. The safety argument is one sentence:

> **The generated thing takes the same path as the written one.**

A plan is a `workflow.Definition`. It goes through the same `workflow.Validate`. It runs on the
same runner. A plan naming an agent nobody defined, an `expect` nobody implements, two steps with
one name or a step with two shapes is refused exactly as a `[[workflow]]` in config would be —
`TestAPlannedWorkflowIsValidatedLikeAWrittenOne` pins that. There is no second, looser road for a
plan a model wrote, which is what makes this worth having at all.

Three things bound it, each borrowed from something that already works here.

**It is opt-in, and every failure is a no-op.** Without `server.planner_agent` the word is refused.
No planner, a turn that would not start, a reply with no JSON in it, a plan `Validate` refuses —
each leaves the thread with a message and nothing started, which is dispatch exactly as it is
without the feature.

**It is shown before it runs.** `workflow.Describe` writes out what each step *is* — who runs it,
where, what will make it count as done — rather than quoting a page of template back:

```
🧭 here is the plan for:
> add a health endpoint, have a second model review it, then let me approve

_implement then review_
1. *implement* — {{.Ask}} · with coder · done when: a pull request was opened
2. *review* — Review {{.PR}}. Report only. · with reviewer · in a thread of its own · done when: it reported
3. *approve* — asks a human: Merge {{.PR}}?
4. *merge* — `merge` · done when: gh confirmed the merge

Run it? Every step is a real agent turn.
```

Confirmed with a button, on the same cross-surface question machinery a gate uses. The refusals
are re-checked *after* the answer, because that question has no timeout and a turn can start on
the thread while it is open.

**It plans in an empty room.** A scratch directory, no tools, no system prompt, no MCP — for the
reason `decider.Claude` does it: dispatch is normally started from a checkout, and a CLI started
there would read that project's CLAUDE.md, its hooks and its MCP config *ahead of its own
instructions*. The ask the planner is judging is data, so it is judged somewhere with nothing in
it. A tool request is denied, which is both the safe answer and the one that ends the turn
quickest.

Two smaller things that turned out to matter.

**The vocabulary is generated from the validator's own constants.** `workflow.Schema()` is built
out of `Expects()`, `OnFails()`, `Threads()` and `Builtins()`, so what the planner is told it may
write and what `Validate` will accept cannot drift apart; `TestSchemaNamesEveryConstant` fails if
a new `expect` is added to only one of them. Without that, adding a value would silently mean
either "refused for a value it was never offered" or "told to use a value that is refused".

**The planning turn ends on its answer.** An executor keeps a finished turn's process warm for
`idle_timeout` so a follow-up is instant — right for a conversation, pure waiting for a one-shot
question that has no follow-up. `planSink` closes a channel on the result and the planner cancels
the run there; the `context.Canceled` that unwinding reports is not an error.

**Who plans.** The planner is *a dispatch agent* — a definition named in config — not a new API
client. No new dependency, no second way to reach a model, and it borrows the operator's login
like everything else. The alternative was a `[planner]` section reusing `decider/claude.go` and
`decider/openai.go` as transports: one HTTP call instead of a CLI spawn, at the cost of a config
surface and a second path to a model. If the latency ever annoys, that is the move.

**Trigger.** An explicit word. The codebase is emphatic that a message which is not one of
dispatch's bare words is the agent's prompt, and silently routing prompts through a planner would
change what every message in a channel means.

**Keeping one.** `workflow save <name>` writes the thread's last plan into `config.toml` through
`config.AppendWorkflow` — textual, comment-preserving, validated, restored if the result does not
load, the same contract `AppendDefinition` has. It is also added to the running coordinator's
list, so it is startable by name straight away rather than after a restart. A plan nobody saved
is a draft and is deliberately kept in memory only.

---

## What had to be fixed first

Both of these were found while planning and are now in the tree.

**A turn had no id.** `awaitTurnEnd` announced an ending turn by *task* id, and on a warm session
the turn before a `merge` and the merge itself are the same task — `finish.go` worked around it by
registering the waiter before it posted a line and draining after (`drainEnds`), which is a timing
argument, not an identity. One workflow runs several turns on one thread and loses that argument
on its first step.

The id could not be the result record's seq: a waiter has to name its turn *before* the turn
exists, and a result seq is only knowable when the turn ends. So the identity is a **floor**, not
a name — `internal/coordinator/turns.go`: the caller writes a record before it asks for the turn
and holds that seq, ends carry the result seq, and a waiter settles on the first end past its
floor. `drainEnds` is gone. Two details had to come with it: an end also carries `Done`, because a
turn that never reached the agent writes no record to outrank a floor and its waiter would
otherwise sit until its timeout; and a full waiter channel still drops rather than blocking the
agent's event loop, so the buffer is sized for the handful of ends a waiter can be behind by.

**`internal/work` scanned a whole thread.** A step's evidence has to be *its own*: a workflow whose
step 1 opened #51 must not have step 3 satisfied by that same sighting. `overviewSince` filters
the records to the ones past the step's floor — the same number the turn is recognised by, built
once and used twice, because a turn *is* the records it produced.

**A report can be long.** `{{.Steps.review.Report}}` is a whole review going into the next prompt.
`workflow.Trim` cuts at a rune boundary and says in the prompt that it did, because a silently
truncated report reads as one that stopped mid-sentence and the agent has no way to know.

## What this does not become

- **Not unattended.** Permission prompts still reach a human mid-workflow unless `auto_allow` or a
  decider covers them. A workflow makes the steps automatic, not the approvals.
- **Not a CI system.** Checks, approvals and branch protection stay somebody's decision to report,
  not an obstacle to work past — the same line `mergePrompt` already draws.
- **Not cheap.** Five steps is five real agent turns, one of them on a second model. That is the
  point, and `plan` says it out loud on the button: "Every step is a real agent turn."

---

## Milestones

Each is a checkbox in one PR unless noted.

| # | what | effort |
|---|------|--------|
| M0 | a turn has an id: ends announce a result seq, waiters hold a floor, no end is droppable | 0.5d |
| M1 | `internal/workflow`: schema, `Validate`, templating, `Next`, tests | 1d |
| M2 | `[[workflow]]` in config; `workflow` records + `WorkflowState` projection | 0.5d |
| M3 | the runner; `review`/`merge` re-expressed as built-in steps — gate: every refusal and close rule in `finish_test.go` still holds, and `make restart-drill` still drills | 2d |
| M4 | `workflows`, `workflow <name> <ask>`, status rendering, feed | 0.5d |
| M5 | gates | 0.5d |
| M6 | restart resume (grades before it resends) + `make restart-drill` coverage | 0.5d |
| M7 | the planner: `plan <description>`, confirm, run | 1d |
| M8 | `save as workflow` write-back, `WORKFLOWS.md`, `config.example.toml` | 0.5d |

All eight shipped in one pull request. The split worth arguing for was M7–M8 — the planner is a
separate feature behind its own config key, and it is useful only once M0–M6 exist — but it is
small, it is off by default, and it shares `workflow.Validate` and the runner with the rest, so
splitting it would have meant landing that shared surface twice.

## Validation

- `make test-race` — the whole suite under the race detector.
- `make lint` — `gofmt` and `go vet`.
- `make e2e` — the binary through the terminal transport.
- `make restart-drill` — SIGTERM mid-tool-call, drain, resume, including the workflow leg: a run
  cut short mid-step is graded before it is resent, so a step that already happened is not asked
  for twice.
