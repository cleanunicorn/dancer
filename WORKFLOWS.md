# WORKFLOWS.md — the design and the plan

*Draft. Nothing here is implemented yet. Written to match the shape of `DECIDER.md`:
the design and the plan in one file, kept current as work lands.*

## Progress

- [x] Read the existing seams (`finish.go`, `internal/work`, `coordinator.drive`, wizards, decider)
- [x] Review folded in: the turn id is a floor, a workflow can be stopped, resume grades before
      it resends, `report` is graded on the result, the model pin is restored, retry is bounded,
      prompts are rendered before they are sent
- [ ] M0 — a turn has an id
- [ ] M1 — `internal/workflow`: schema, validation, templating, state machine
- [ ] M2 — config `[[workflow]]` + store projection
- [ ] M3 — the runner in the coordinator; `review` and `merge` become built-in steps
- [ ] M4 — chat words, rendering, feed
- [ ] M5 — gates (human approval between steps)
- [ ] M6 — restart resume + `make restart-drill` coverage
- [ ] M7 — the planner: on-demand workflows from a Slack message
- [ ] M8 — `save this as a workflow` write-back + docs

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
| `model`   | override for this step only, restored when the step ends |
| `thread`  | `same` (follow-up on the home thread) or `new` (a fresh thread beside it, fresh session) |
| `prompt`  | Go template over the workflow's state |
| `builtin` | `review` / `merge` — the words that already exist, as steps |
| `expect`  | what the log must show: `pr`, `push`, `merged`, `report`, `none` |
| `gate`    | a question for a human instead of a turn for an agent |
| `on_fail` | `ask` (default) · `retry` · `stop` |

Nothing else. No conditionals, no loops, no parallelism in v1 — a linear list with a human gate is
already the whole of the example that prompted this, and every added construct is a construct that
has to survive a restart.

Four rules a step carries that the table cannot say:

- `pr`, `push` and `merged` are `internal/work` sightings, graded on the records the step itself
  produced. A `report` is not something a log can be mined for — every turn ends in text — so it
  is graded on the `EventResult` itself: the turn ended, not in error, with something to say.
  `none` asks nothing and is satisfied by the turn ending.
- `retry` retries once, then takes the ordinary failure path (`ask`, by default) — an unbounded
  retry loop is a bill, not a workflow.
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

The second type: tag dispatch in Slack, describe what you want and how you want it done, and
dispatch composes the workflow.

The contract that makes this safe is the one `internal/decider` already established:

1. **One execution path.** The planner emits the *same* struct the TOML parses into, validated by
   the same `workflow.Validate`. There is no "dynamic" runner.
2. **It can only name what exists.** Agents must be real definitions, `expect` and `builtin` must
   be known values, models must be ones the named agent's kind can run. Anything else is refused
   and the message becomes an ordinary task — which is exactly today's behaviour, so the fallback
   for every planner failure is "dispatch works as it does now".
3. **It is shown before it runs.** The steps are posted and confirmed with a button — the same
   gate machinery, cross-surface, persisted. On Slack that is also the only honest thing to do
   before spending five agent turns.
4. **It is written to the log**, so a restart resumes the plan that was approved, and a human can
   read what was planned six hours later.
5. **`save as feature`** writes it into `config.toml` via a `config.AppendWorkflow` alongside
   `AppendDefinition` — because anything created from chat that is not written back is lost on
   restart.

**Who plans.** Recommendation: the planner is *a dispatch agent* — a definition with a local
read-only environment, asked for JSON. No new dependency, no second API client, and it borrows the
operator's login like everything else. The alternative is a `[planner]` section reusing
`decider/claude.go` and `decider/openai.go` as clients: faster (one HTTP call, no CLI spawn) at
the cost of a config surface and a second way to reach a model. Start with the agent; move it if
the latency is annoying.

**Trigger.** An explicit word — `plan <description>` — not "every first message in the channel is
planned". The codebase is emphatic that a message which is not one of dispatch's bare words is the
agent's prompt, and silently routing prompts through a planner would change what every message in
a channel means. Off by default, one config key to turn on.

---

## What has to be fixed first

**A turn has no id.** `awaitTurnEnd` announces an ending turn by *task* id, and on a warm session
the turn before a `merge` and the merge itself are the same task — `finish.go` works around it by
registering the waiter before it posts a line and draining after (`drainEnds`), which is a timing
argument, not an identity. One workflow runs several turns on one thread, so this stops being a
workaround and starts being a bug.

The id cannot be the result record's seq: a waiter has to name its turn *before* the turn exists —
`merge` registers before it posts — and a result seq is only knowable when the turn ends. So the
identity is a **floor**, not a name: the runner records the log seq as it sends, the announcement
carries the ended turn's result seq, and a waiter is matched by "the first end on this task past
my floor". That is the same `since seq` the evidence fix below needs, so it is built once and
used twice. One more thing must survive the generalization: `turnEnded` today drops an
announcement rather than block on a full waiter channel, and a dropped end would hang a workflow
step until its timeout — a registered waiter must not be able to miss its end. Half a day, and it
is a genuine standalone fix.

**`internal/work` scans a whole thread.** A step's evidence must be *its own*: a workflow whose
step 1 opened #51 must not have step 3 satisfied by that same sighting. Fix: a `since seq` floor
on the scan, so a step is graded on the records it produced — the same floor the turn id above is
matched by, built once.

**A report can be long.** `{{.Steps.review.Report}}` is a whole review going into the next prompt.
Trim at a sane budget and say in the prompt that it was trimmed, rather than silently truncating.

## What this does not become

- **Not unattended.** Permission prompts still reach a human mid-workflow unless `auto_allow` or a
  decider covers them. A workflow makes the steps automatic, not the approvals.
- **Not a CI system.** Checks, approvals and branch protection stay somebody's decision to report,
  not an obstacle to work past — the same line `mergePrompt` already draws.
- **Not cheap.** Five steps is five real agent turns, one of them on a second model. That is the
  point, but it should be said out loud in the docs.

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

**≈6.5 days.** M0–M6 are the predetermined workflows and ship as one PR. M7–M8 are the planner:
a separate feature behind its own config key that is useful only once M0–M6 exist, and that is the
one place a split is worth arguing for.

## Validation

`make test-race`, `make e2e`, `make restart-drill`, plus a live run of the `feature` workflow
end to end against a scratch repository with two different agent kinds.
