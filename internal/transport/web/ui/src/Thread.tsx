// The desk: the open thread's strip pulled out of the rack at full width,
// the log beneath it, and the printer (composer) along the bottom edge.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Button, TextArea, TextField, Tooltip } from "@heroui/react";
import type { Message, Thread } from "./api";
import { ME } from "./api";
import { Choices, LiveLine, MessageRow, ToolStrip } from "./Message";
import { plain } from "./mrkdwn";
import { label, promptOpen, store, useStore } from "./store";
import { FLAG, Flag, Lamp, Mark, TITLE, elapsed, state, type StripState } from "./strip";

function useNarrow(): boolean {
  const [narrow, set] = useState(
    () => window.matchMedia("(max-width: 767px)").matches,
  );
  useEffect(() => {
    const mq = window.matchMedia("(max-width: 767px)");
    const on = () => set(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);
  return narrow;
}

function useTick(on: boolean, ms = 1000) {
  const [, set] = useState(0);
  useEffect(() => {
    if (!on) return;
    const id = window.setInterval(() => set((n) => n + 1), ms);
    return () => window.clearInterval(id);
  }, [on, ms]);
}

// askOf is the rack's copy of a thread's open prompt as a log line, for
// the pulled strip to answer from before the log is fetched.
function askOf(t: Thread): Message {
  return { id: 0, thread: t.id, at: t.updated, text: t.ask || "", prompt: t.prompt, mention: t.mention };
}

// Actions are the chat commands that fit the strip's state, printed on
// it: cancel while the agent moves, close once it stopped, status always.
// Each sends the command as if typed.
function Actions({ t, state: s }: { t: Thread; state: StripState }) {
  const [busy, setBusy] = useState("");
  if (!t.status || s === "closed") return null;
  const run = async (cmd: string) => {
    setBusy(cmd);
    await store.send(cmd);
    setBusy("");
  };
  const moving = s === "run" || s === "wait" || s === "queue";
  return (
    <div className="actions ml-auto flex items-center gap-1.5">
      {moving ? (
        <Button size="sm" variant="danger" isDisabled={!!busy} onPress={() => run("cancel")} className="font-strip h-7 text-[11px] uppercase tracking-wider">
          {busy === "cancel" ? "cancelling…" : "cancel"}
        </Button>
      ) : (
        <Button size="sm" variant="secondary" isDisabled={!!busy} onPress={() => run("close")} className="font-strip h-7 border border-ink/25 bg-surface text-[11px] uppercase tracking-wider">
          {busy === "close" ? "closing…" : "close"}
        </Button>
      )}
      <Button size="sm" variant="ghost" isDisabled={!!busy} onPress={() => run("status")} className="font-strip h-7 text-[11px] uppercase tracking-wider text-ink">
        status
      </Button>
    </div>
  );
}

function DeskStrip({ t, menu }: { t: Thread; menu: () => void }) {
  const st = useStore();
  const s = state(t);
  const list = st.messages.get(t.id);
  // the open prompt rides on the pulled strip, so the answer is always at hand
  let ask: Message | null = null;
  if (s === "wait" && list) {
    for (let i = list.length - 1; i >= 0; i--) {
      if (list[i].prompt && promptOpen(list, i)) {
        ask = list[i];
        break;
      }
    }
  }
  // until the log is fetched, the rack's own copy of the prompt will do
  if (s === "wait" && !ask && !list && t.prompt) ask = askOf(t);
  const running = s === "run" || s === "wait";
  // the clock counts this turn, not the whole thread: from the prompt that
  // waits, or from the last thing a human said, which is what set the agent
  // going. Until the log is fetched, all we know is when the thread moved.
  let since = t.updated;
  if (ask) since = ask.at;
  else if (list) {
    for (let i = list.length - 1; i >= 0; i--) {
      if (list[i].from) {
        since = list[i].at;
        break;
      }
    }
  }
  const stamp = running ? elapsed(since) : elapsed(t.updated);
  useTick(running);
  return (
    <header className="px-3 pt-3 md:px-5 md:pt-4">
      <div
        className="desk-strip"
        data-state={s}
        data-host={t.transport && t.transport !== ME ? t.transport : undefined}
      >
        <div className="flex items-stretch">
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            className="my-2 ml-2 text-ink md:hidden"
            aria-label="Show the rack"
            onPress={menu}
          >
            ☰
          </Button>
          <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-4 gap-y-1.5 px-3 py-2 md:gap-x-5 md:gap-y-2 md:px-5 md:py-2.5">
            <div className="flex min-w-0 basis-full items-center gap-3 md:basis-auto md:flex-1">
              <Lamp state={s} className="!static" />
              <h1
                className="min-w-0 flex-1 truncate text-[15px] font-semibold leading-tight"
                title={t.title || t.id}
              >
                {t.title || t.id}
              </h1>
            </div>
            <div className="field md:w-[9rem]">
              <span className="k">channel</span>
              <span className="v">
                {t.transport && t.transport !== ME
                  ? label(t.transport).toLowerCase() + " · "
                  : ""}
                #{st.channels.get(t.channel)?.name || t.channel}
              </span>
            </div>
            <div className="field md:w-[7rem]">
              <span className="k">started by</span>
              <span className="v">{t.requester || "—"}</span>
            </div>
            <div className="field md:w-[5.5rem]">
              <span className="k">{running ? (ask ? "waiting" : "elapsed") : "updated"}</span>
              <span className="v">{running || stamp === "—" ? stamp : stamp + " ago"}</span>
            </div>
            {t.agent ? (
              <div className="field md:w-[6rem]" title={[t.model, t.env].filter(Boolean).join(" · ")}>
                <span className="k">agent</span>
                <span className="v">{t.agent}</span>
              </div>
            ) : null}
            {t.model ? (
              <div className="field md:w-[9rem]" title={t.model}>
                <span className="k">model</span>
                <span className="v">{t.model.replace(/^claude-/, "")}</span>
              </div>
            ) : null}
            {t.env ? (
              <div className="field md:w-[4rem]">
                <span className="k">env</span>
                <span className="v">{t.env}</span>
              </div>
            ) : null}
            {t.session ? (
              <div className="field md:w-[5rem]">
                <span className="k">session</span>
                <button
                  type="button"
                  className="v cursor-copy text-left hover:underline"
                  title={t.session + " — click to copy"}
                  onClick={() => navigator.clipboard?.writeText(t.session!).then(() => store.toast("session id copied"))}
                >
                  {t.session.slice(0, 8)}
                </button>
              </div>
            ) : null}
            <div className="field" title={TITLE[s]}>
              <span className="k">state</span>
              <span className="v">
                <Flag state={s} />
              </span>
            </div>
            <Actions t={t} state={s} />
          </div>
        </div>
        {ask ? (
          <div className="ask flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-2 md:px-5">
            <span className="text" title={ask.text}>
              {plain(ask.text)}
            </span>
            <Choices m={ask} open inline />
          </div>
        ) : null}
      </div>
    </header>
  );
}

function DraftStrip({
  channel,
  menu,
}: {
  channel: { name: string; transport: string };
  menu: () => void;
}) {
  return (
    <header className="px-3 pt-3 md:px-5 md:pt-4">
      <div
        className="desk-strip flex items-center gap-3 px-4 py-2.5 md:px-5"
        data-state="new"
      >
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          className="text-ink md:hidden"
          aria-label="Show the rack"
          onPress={menu}
        >
          ☰
        </Button>
        <Lamp state="new" className="!static" />
        <div className="min-w-0 flex-1">
          <h1 className="truncate text-[15px] font-semibold leading-tight">
            New strip in #{channel.name}
          </h1>
          <p className="font-strip text-[11px] text-ink-2">
            {channel.transport === ME
              ? "your first line starts the thread, and the task"
              : `your first line is posted in ${label(channel.transport)} as a new thread; the task runs there`}
          </p>
        </div>
        <span className="flag" data-state="new">
          {FLAG.new}
        </span>
      </div>
    </header>
  );
}

// group folds each run of consecutive tool calls into one array, so the
// log prints them as one line between the messages around them.
function group(list: Message[]): (Message | Message[])[] {
  const out: (Message | Message[])[] = [];
  for (const m of list) {
    if (m.tool) {
      const last = out[out.length - 1];
      if (Array.isArray(last)) last.push(m);
      else out.push([m]);
    } else out.push(m);
  }
  return out;
}

function Messages({ list, me }: { list: Message[]; me: string }) {
  const box = useRef<HTMLDivElement>(null);
  const [stick, setStick] = useState(true);
  const [behind, setBehind] = useState(false);
  const live = list.find((m) => m.key);
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    if (stick) el.scrollTop = el.scrollHeight;
    else setBehind(true);
  }, [list, stick]);
  return (
    <div
      ref={box}
      className="relative flex-1 overflow-y-auto px-3 py-4 md:px-5"
      onScroll={() => {
        const el = box.current!;
        const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
        setStick(atBottom);
        if (atBottom) setBehind(false);
      }}
    >
      <div className="log mx-auto max-w-[56rem]">
        {group(list).map((g) =>
          Array.isArray(g) ? (
            <ToolStrip key={"t" + g[0].id} calls={g} />
          ) : g.key ? null : g.decision &&
            list.some((p) => p.prompt?.id === g.decision!.promptId) ? null : (
            <MessageRow
              key={g.id}
              m={g}
              list={list}
              open={!!g.prompt && promptOpen(list, list.indexOf(g))}
              me={me}
            />
          ),
        )}
        {live ? <LiveLine text={live.text} /> : null}
      </div>
      {behind && !stick ? (
        <Button
          size="sm"
          variant="secondary"
          className="sticky bottom-2 left-1/2 -translate-x-1/2 font-strip text-xs"
          onPress={() => {
            box.current!.scrollTop = box.current!.scrollHeight;
            setStick(true);
            setBehind(false);
          }}
        >
          new lines ↓
        </Button>
      ) : null}
    </div>
  );
}

// Suggestions sit on the printer's edge and change with what is open: a
// new strip offers the agents (one fills in `run <agent> `), a closed
// strip says a reply reopens it.
function Suggestions({ thread, draft, pick }: { thread: Thread | null; draft: boolean; pick: (text: string) => void }) {
  const st = useStore();
  if (draft && st.agents.length) {
    return (
      <div className="mx-auto flex w-full max-w-[56rem] flex-wrap items-center gap-x-2 gap-y-1 pb-2 font-strip text-[11px] text-muted">
        <span className="uppercase tracking-wider">agent</span>
        {st.agents.map((a) => (
          <Tooltip key={a.name} delay={400}>
            <Tooltip.Trigger>
              <button type="button" className="chip" onClick={() => pick(`run ${a.name} `)}>
                {a.name}
              </button>
            </Tooltip.Trigger>
            <Tooltip.Content>{[a.model, a.env].filter(Boolean).join(" · ") || "default model"} — fills in `run {a.name}`</Tooltip.Content>
          </Tooltip>
        ))}
      </div>
    );
  }
  if (thread?.closed) {
    return <div className="mx-auto w-full max-w-[56rem] pb-2 font-strip text-[11px] text-muted">this strip is closed — a reply reopens it and reaches the agent</div>;
  }
  return null;
}

function Printer({
  placeholder,
  short,
}: {
  placeholder: string;
  short: string;
}) {
  const narrow = useNarrow();
  const st = useStore();
  const thread = st.current ? st.threads.get(st.current) || null : null;
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const ref = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (
        e.key === "/" &&
        document.activeElement !== ref.current &&
        !document.querySelector("[role=dialog]")
      ) {
        e.preventDefault();
        ref.current?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);
  const send = async () => {
    const t = text.trim();
    if (!t || busy) return;
    setBusy(true);
    if (await store.send(t)) setText("");
    setBusy(false);
    ref.current?.focus();
  };
  return (
    <form
      className="printer flex flex-col px-3 py-3 md:px-5"
      onSubmit={(e) => {
        e.preventDefault();
        send();
      }}
    >
      <Suggestions
        thread={thread}
        draft={!st.current && !!st.draft}
        pick={(t) => {
          setText(t);
          ref.current?.focus();
        }}
      />
      <div className="mx-auto flex w-full max-w-[56rem] items-end gap-2">
        <TextField className="flex-1" aria-label="Message">
          <TextArea
            ref={ref}
            autoFocus
            rows={1}
            placeholder={narrow ? short : placeholder}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (
                e.key === "Enter" &&
                !e.shiftKey &&
                !e.nativeEvent.isComposing
              ) {
                e.preventDefault();
                send();
              }
            }}
            className="max-h-48 min-h-10 resize-none"
          />
        </TextField>
        <Button
          type="submit"
          variant="primary"
          isDisabled={busy || !text.trim()}
          className="font-strip text-xs uppercase tracking-wider"
        >
          Send
        </Button>
      </div>
    </form>
  );
}

function EmptyDesk({
  draft,
  opening,
  menu,
}: {
  draft: boolean;
  opening: boolean;
  menu: () => void;
}) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-4 px-6 text-center text-muted">
      <Button
        isIconOnly
        size="sm"
        variant="ghost"
        className="absolute left-3 top-3 md:hidden"
        aria-label="Show the rack"
        onPress={menu}
      >
        ☰
      </Button>
      <Mark size={40} />
      {opening ? (
        <p className="font-strip text-[12px]">printing the strip…</p>
      ) : draft ? (
        <p className="max-w-[36ch] text-sm">
          Type what the agent should do. The first line you send becomes the
          strip.
        </p>
      ) : (
        <p className="max-w-[40ch] text-sm">
          An empty desk. Pull a strip from the rack, or start one with{" "}
          <span className="kbd">+</span> next to a channel.
        </p>
      )}
    </div>
  );
}

export function ThreadPane({ menu }: { menu: () => void }) {
  const st = useStore();
  const t = st.current ? st.threads.get(st.current) : null;
  const list = st.current ? st.messages.get(st.current) : undefined;
  const draftChannel = st.draft ? st.channels.get(st.draft.channel) : null;
  return (
    <main className="relative flex min-h-0 min-w-0 flex-1 flex-col">
      {t ? (
        <DeskStrip t={t} menu={menu} />
      ) : draftChannel ? (
        <DraftStrip channel={draftChannel} menu={menu} />
      ) : null}
      {st.current ? (
        list ? (
          <Messages list={list} me={st.me} />
        ) : (
          <div className="flex-1" />
        )
      ) : (
        <EmptyDesk draft={!!st.draft} opening={!!st.draft?.text} menu={menu} />
      )}
      {st.current || st.draft ? (
        <Printer
          placeholder={
            st.current
              ? t?.closed
                ? "Reply to reopen — Enter sends, Shift+Enter for a new line"
                : "Reply — Enter sends, Shift+Enter for a new line"
              : "Describe the task, or `run <agent> <prompt>` to pick the agent"
          }
          short={st.current ? "Reply…" : "What should the agent do?"}
        />
      ) : null}
    </main>
  );
}
