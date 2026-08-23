// One line of the log. Humans write on paper slips; the agent speaks on the
// desk in Markdown; dancer's own lines are the rack's voice; a prompt is a
// lit strip laid across the log, answered in place and stamped once settled.
import { useMemo, useState, type ReactNode } from "react";
import { Button, Input, TextField } from "@heroui/react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { ME, type Message } from "./api";
import { Mrkdwn, linkify } from "./mrkdwn";
import { clock, label, store, useStore } from "./store";
import { Lamp } from "./strip";

function size(n: number): string {
  return n < 1024 ? n + " B" : n < 1048576 ? (n / 1024).toFixed(1) + " KB" : (n / 1048576).toFixed(1) + " MB";
}

function tone(text: string): "ok" | "bad" | "warn" | undefined {
  if (text.startsWith("❌")) return "bad";
  if (text.startsWith("✅")) return "ok";
  if (/^(⏸️|⚠️|🚫|⏹️|♻️)/.test(text)) return "warn";
  return undefined;
}

export function LiveLine({ text }: { text: string }) {
  return (
    <>
      <span className="t" aria-hidden="true" />
      <div className="live" role="status" aria-live="polite">
        <Lamp state="run" />
        <span className="min-w-0 truncate">{text}</span>
      </div>
    </>
  );
}

function Files({ files }: { files: NonNullable<Message["files"]> }) {
  const urls = useMemo(
    () =>
      files.map((f) =>
        f.data ? URL.createObjectURL(new Blob([Uint8Array.from(atob(f.data), (c) => c.charCodeAt(0))])) : "",
      ),
    [files],
  );
  return (
    <div className="mt-2 flex flex-wrap gap-2">
      {files.map((f, i) =>
        !urls[i] ? (
          <span key={i} className="font-strip text-[12px] text-ink-2">
            ⎘ {f.name} · {size(f.size)}
            {f.size > 0 ? " · too large to show" : " · not kept in the log"}
          </span>
        ) : /\.(png|jpe?g|gif|webp|svg)$/i.test(f.name) ? (
          <a key={i} href={urls[i]} target="_blank" rel="noopener" className="block">
            <img src={urls[i]} alt={f.name} className="max-h-80 max-w-full rounded-[2px] border border-ink/20" />
          </a>
        ) : (
          <a key={i} href={urls[i]} download={f.name} className="font-strip rounded-[2px] border border-ink/30 px-2 py-1 text-[12px] hover:bg-ink/5">
            ⎘ {f.name} <span className="text-ink-2">· {size(f.size)}</span>
          </a>
        ),
      )}
    </div>
  );
}

export function Choices({ m, open, inline }: { m: Message; open: boolean; inline?: boolean }) {
  const p = m.prompt!;
  // the store holds the answer in flight, so both copies of a prompt's
  // buttons — the log's and the pulled strip's — go quiet together
  const busy = useStore().answering.has(p.id);
  const [free, setFree] = useState("");
  const send = async (choice: string) => {
    try {
      await store.decide(m.thread, p.id, choice);
    } catch (e) {
      store.toast(String(e));
    }
  };
  if (!open) return <div className="answered">settled elsewhere</div>;
  return (
    <div className={inline ? "flex flex-col gap-2" : "mt-3 flex flex-col gap-2"}>
      <div className="flex flex-wrap gap-2">
        {p.options?.length
          ? p.options.map((o) => (
              <Button key={o.value} variant="secondary" size="sm" isDisabled={busy} onPress={() => send(o.value)} className="h-auto items-start border border-ink/25 bg-surface py-1.5 text-left">
                <span className="flex flex-col">
                  <span>{o.label || o.value}</span>
                  {o.description ? <span className="text-xs font-normal text-muted">{o.description}</span> : null}
                </span>
              </Button>
            ))
          : (p.choices || []).map((c) => (
              <Button
                key={c}
                size="sm"
                isDisabled={busy}
                variant={c === "allow" ? "primary" : c === "deny" ? "danger" : "secondary"}
                onPress={() => send(c)}
                className={"font-strip uppercase tracking-wider " + (c !== "allow" && c !== "deny" ? "border border-ink/25 bg-surface" : "")}
              >
                {c}
              </Button>
            ))}
      </div>
      {p.freeText ? (
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (free.trim()) send(free.trim());
          }}
        >
          <TextField className="flex-1" aria-label="Your own answer">
            <Input placeholder="or type your own answer…" value={free} onChange={(e) => setFree(e.target.value)} className="bg-surface" />
          </TextField>
          <Button type="submit" variant="secondary" isDisabled={busy || !free.trim()} className="font-strip border border-ink/25 bg-surface text-xs uppercase tracking-wider">
            Answer
          </Button>
        </form>
      ) : null}
    </div>
  );
}

function Who({ name, via, at, lamp }: { name: string; via?: string; at: string; lamp?: ReactNode }) {
  return (
    <div className="who">
      {lamp}
      <b>{name}</b>
      {via && via !== ME ? <span className="via">via {label(via)}</span> : null}
      <span className="clock">{clock(at)}</span>
    </div>
  );
}

export function MessageRow({ m, list, open, me }: { m: Message; list: Message[]; open: boolean; me: string }) {
  const kind = m.from ? "human" : m.markdown ? "agent" : "system";
  const forMe = !!m.mention && m.mention === me;
  const answer = m.prompt ? list.find((x) => x.decision?.promptId === m.prompt!.id) || null : null;
  const time = (
    <span className="t" title={m.at}>
      {clock(m.at)}
    </span>
  );

  if (m.prompt) {
    return (
      <>
        {time}
        <div className="prompt" data-open={!!open && !answer} data-mention={forMe ? "me" : undefined}>
          <Who name={m.mention ? "@" + m.mention : "dancer"} at={m.at} lamp={<Lamp state={answer || !open ? "done" : "wait"} />} />
          <div className="text">
            <Mrkdwn text={m.text} />
          </div>
          {m.files?.length ? <Files files={m.files} /> : null}
          {answer ? (
            <div className="answered">
              <b>{answer.decision!.choice}</b>
              {answer.from ? ` ${answer.from.name}${answer.from.via !== ME ? " via " + label(answer.from.via) : ""}` : " decided"} · {clock(answer.at)}
            </div>
          ) : (
            <Choices m={m} open={open} />
          )}
        </div>
      </>
    );
  }

  if (kind === "human") {
    return (
      <>
        {time}
        <div className="slip" data-host={m.from!.via !== ME ? m.from!.via : undefined}>
          <Who name={m.from!.name} via={m.from!.via} at={m.at} />
          {m.decision ? (
            <span className="font-strip text-[12px]">
              → <b>{m.decision.choice}</b>
            </span>
          ) : (
            linkify(m.text)
          )}
          {m.files?.length ? <Files files={m.files} /> : null}
        </div>
      </>
    );
  }

  if (kind === "agent") {
    return (
      <>
        {time}
        <div>
          <Who name="agent" at={m.at} />
          <div className="md">
            <Markdown remarkPlugins={[remarkGfm]} components={{ a: (p) => <a {...p} target="_blank" rel="noopener" /> }}>
              {m.text}
            </Markdown>
          </div>
          {m.files?.length ? <Files files={m.files} /> : null}
        </div>
      </>
    );
  }

  return (
    <>
      {time}
      <div className="sys" data-tone={tone(m.text)} data-mention={forMe ? "me" : undefined}>
        {m.mention ? <span className="mention">@{m.mention}</span> : null}
        <Mrkdwn text={m.text} />
        {m.files?.length ? <Files files={m.files} /> : null}
      </div>
    </>
  );
}
