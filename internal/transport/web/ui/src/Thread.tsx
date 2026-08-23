import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Button, Chip, TextArea, TextField } from "@heroui/react";
import type { Message } from "./api";
import { ME } from "./api";
import { LiveLine, MessageRow } from "./Message";
import { label, promptOpen, store, useStore } from "./store";

function Header({ menu }: { menu: () => void }) {
  const st = useStore();
  const t = st.current ? st.threads.get(st.current) : null;
  const draftChannel = st.draft ? st.channels.get(st.draft.channel) : null;
  return (
    <header className="flex items-center gap-3 border-b border-border bg-surface px-4 py-2.5">
      <Button isIconOnly size="sm" variant="ghost" className="md:hidden" aria-label="Channels" onPress={menu}>
        ☰
      </Button>
      <div className="min-w-0 flex-1">
        {t ? (
          <>
            <div className="truncate font-medium">{t.title || t.id}</div>
            <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted">
              <span>#{st.channels.get(t.channel)?.name || t.channel}</span>
              {t.transport && t.transport !== ME ? (
                <Chip size="sm" variant="soft">
                  {label(t.transport)}
                </Chip>
              ) : null}
              {t.status ? (
                <Chip size="sm" variant="soft" color={t.closed ? "default" : t.status === "failed" ? "danger" : t.status === "running" ? "accent" : "default"}>
                  {t.closed ? "closed" : t.status}
                </Chip>
              ) : null}
              {t.requester ? <span>· started by {t.requester}</span> : null}
            </div>
          </>
        ) : draftChannel ? (
          <>
            <div className="truncate font-medium">New thread in #{draftChannel.name}</div>
            <div className="text-xs text-muted">
              {draftChannel.transport === ME
                ? "Your first message starts the thread — and the task."
                : `Your first message is posted in ${label(draftChannel.transport)} as the start of a new thread; the task runs there.`}
            </div>
          </>
        ) : (
          <div className="font-medium text-muted">Pick a thread</div>
        )}
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
      className="relative flex-1 overflow-y-auto px-4 py-4"
      onScroll={() => {
        const el = box.current!;
        const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
        setStick(atBottom);
        if (atBottom) setBehind(false);
      }}
    >
      <div className="mx-auto flex max-w-3xl flex-col gap-3">
        {list.map((m, i) =>
          m.key ? null : m.decision && list.some((p) => p.prompt?.id === m.decision!.promptId) ? null : (
            <MessageRow key={m.id} m={m} list={list} open={!!m.prompt && promptOpen(list, i)} me={me} />
          ),
        )}
        {live ? <LiveLine text={live.text} /> : null}
      </div>
      {behind && !stick ? (
        <Button
          size="sm"
          variant="secondary"
          className="sticky bottom-2 left-1/2 -translate-x-1/2"
          onPress={() => {
            box.current!.scrollTop = box.current!.scrollHeight;
            setStick(true);
            setBehind(false);
          }}
        >
          New messages ↓
        </Button>
      ) : null}
    </div>
  );
}

function Composer({ placeholder }: { placeholder: string }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const ref = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "/" && document.activeElement !== ref.current && !document.querySelector("[role=dialog]")) {
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
      className="flex items-end gap-2 border-t border-border bg-background px-4 py-3"
      onSubmit={(e) => {
        e.preventDefault();
        send();
      }}
    >
      <TextField className="flex-1" aria-label="Message">
        <TextArea
          ref={ref}
          autoFocus
          rows={1}
          placeholder={placeholder}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              send();
            }
          }}
          className="max-h-48 min-h-10 resize-none"
        />
      </TextField>
      <Button type="submit" variant="primary" isDisabled={busy || !text.trim()}>
        Send
      </Button>
    </form>
  );
}

export function ThreadPane({ menu }: { menu: () => void }) {
  const st = useStore();
  const list = st.current ? st.messages.get(st.current) : undefined;
  return (
    <main className="flex min-h-0 min-w-0 flex-1 flex-col">
      <Header menu={menu} />
      {st.current ? (
        list ? (
          <Messages list={list} me={st.me} />
        ) : (
          <div className="flex-1" />
        )
      ) : st.draft ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-2 text-muted">
          <div className="text-5xl">✨</div>
          <p className="text-sm">{st.draft.text ? "Opening the thread…" : "What should the agent do?"}</p>
        </div>
      ) : (
        <div className="flex flex-1 flex-col items-center justify-center gap-2 text-muted">
          <div className="text-5xl">🕺</div>
          <p className="text-sm">Pick a thread on the left, or start one with +</p>
        </div>
      )}
      {st.current || st.draft ? (
        <Composer placeholder={st.current ? "Reply… (Enter to send, Shift+Enter for a new line)" : "Describe the task, or `run <agent> <prompt>` to pick the agent"} />
      ) : null}
    </main>
  );
}
