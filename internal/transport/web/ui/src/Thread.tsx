// The desk: the open thread's strip pulled out of the rack at full width,
// the log beneath it, and the printer (composer) along the bottom edge.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Button, TextArea, TextField } from "@heroui/react";
import type { Message, Thread } from "./api";
import { ME } from "./api";
import { Choices, LiveLine, MessageRow } from "./Message";
import { label, promptOpen, store, useStore } from "./store";
import { FLAG, Flag, Lamp, Mark, TITLE, elapsed, state } from "./strip";

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

function DeskStrip({ t, menu }: { t: Thread; menu: () => void }) {
  const st = useStore();
  const s = state(t);
  const list = st.messages.get(t.id);
  const started = list?.find((m) => m.from)?.at;
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
  useTick(s === "run" || s === "wait");
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
              <span className="k">
                {s === "run" || s === "wait" ? "elapsed" : "updated"}
              </span>
              <span className="v">
                {s === "run" || s === "wait"
                  ? elapsed(started || t.updated)
                  : elapsed(t.updated) + " ago"}
              </span>
            </div>
            <div className="field" title={TITLE[s]}>
              <span className="k">state</span>
              <span className="v">
                <Flag state={s} />
              </span>
            </div>
          </div>
        </div>
        {ask ? (
          <div className="ask flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-2 md:px-5">
            <span className="text" title={ask.text}>
              {ask.text.replace(/[*_`]/g, "").replace(/\s+/g, " ").trim()}
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
        {list.map((m, i) =>
          m.key ? null : m.decision &&
            list.some((p) => p.prompt?.id === m.decision!.promptId) ? null : (
            <MessageRow
              key={m.id}
              m={m}
              list={list}
              open={!!m.prompt && promptOpen(list, i)}
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

function Printer({
  placeholder,
  short,
}: {
  placeholder: string;
  short: string;
}) {
  const narrow = useNarrow();
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
      className="printer flex items-end gap-2 px-3 py-3 md:px-5"
      onSubmit={(e) => {
        e.preventDefault();
        send();
      }}
    >
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
              ? "Reply — Enter sends, Shift+Enter for a new line"
              : "Describe the task, or `run <agent> <prompt>` to pick the agent"
          }
          short={st.current ? "Reply…" : "What should the agent do?"}
        />
      ) : null}
    </main>
  );
}
