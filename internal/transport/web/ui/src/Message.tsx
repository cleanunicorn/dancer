import { useMemo, useState } from "react";
import { Avatar, Button, Chip, Input, Spinner, TextField } from "@heroui/react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { ME, type Message } from "./api";
import { Mrkdwn, linkify } from "./mrkdwn";
import { clock, label, store } from "./store";

function initials(name: string): string {
  return (name || "?")
    .split(/\s+/)
    .map((w) => w[0])
    .join("")
    .slice(0, 2)
    .toUpperCase();
}

function size(n: number): string {
  return n < 1024 ? n + " B" : n < 1048576 ? (n / 1024).toFixed(1) + " KB" : (n / 1048576).toFixed(1) + " MB";
}

function tone(text: string): string {
  if (text.startsWith("❌")) return "text-danger";
  if (text.startsWith("✅")) return "text-success";
  if (/^(⏸️|⚠️|🚫|⏹️|♻️)/.test(text)) return "text-warning";
  return "text-muted";
}

export function LiveLine({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-2 pl-11 text-sm text-accent">
      <Spinner size="sm" color="accent" />
      <span>{text}</span>
    </div>
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
          <span key={i} className="text-sm text-muted">
            📎 {f.name} ({size(f.size)}, too large to show)
          </span>
        ) : /\.(png|jpe?g|gif|webp|svg)$/i.test(f.name) ? (
          <a key={i} href={urls[i]} target="_blank" rel="noopener">
            <img src={urls[i]} alt={f.name} className="max-h-80 max-w-full rounded-md border border-border" />
          </a>
        ) : (
          <a key={i} href={urls[i]} download={f.name} className="rounded-md border border-border px-2 py-1 text-sm hover:bg-surface-hover">
            📎 {f.name} <span className="text-muted">({size(f.size)})</span>
          </a>
        ),
      )}
    </div>
  );
}

function PromptCard({ m, answer, open }: { m: Message; answer: Message | null; open: boolean }) {
  const p = m.prompt!;
  const [busy, setBusy] = useState(false);
  const [free, setFree] = useState("");
  const send = async (choice: string) => {
    setBusy(true);
    try {
      await store.decide(m.thread, p.id, choice);
    } catch (e) {
      store.toast(String(e));
      setBusy(false);
    }
  };
  if (answer) {
    return (
      <div className="mt-2 text-sm text-muted">
        ✓ <b className="text-foreground">{answer.decision!.choice}</b>
        {answer.from ? ` — ${answer.from.name}${answer.from.via !== ME ? " via " + label(answer.from.via) : ""}` : null}
      </div>
    );
  }
  if (!open) return <div className="mt-2 text-sm italic text-muted">settled</div>;
  return (
    <div className="mt-3 flex flex-col gap-2">
      <div className="flex flex-wrap gap-2">
        {p.options?.length
          ? p.options.map((o) => (
              <Button key={o.value} variant="secondary" size="sm" isDisabled={busy} onPress={() => send(o.value)} className="h-auto items-start py-1.5 text-left">
                <span className="flex flex-col">
                  <span>{o.label || o.value}</span>
                  {o.description ? <span className="text-xs font-normal text-muted">{o.description}</span> : null}
                </span>
              </Button>
            ))
          : (p.choices || []).map((c) => (
              <Button key={c} size="sm" isDisabled={busy} variant={c === "allow" ? "primary" : c === "deny" ? "danger-soft" : "secondary"} onPress={() => send(c)}>
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
            <Input placeholder="or type your own answer…" value={free} onChange={(e) => setFree(e.target.value)} />
          </TextField>
          <Button type="submit" variant="secondary" isDisabled={busy || !free.trim()}>
            Answer
          </Button>
        </form>
      ) : null}
    </div>
  );
}

export function MessageRow({ m, list, isLast, me }: { m: Message; list: Message[]; isLast: boolean; me: string }) {
  const kind = m.from ? "human" : m.markdown ? "agent" : "system";
  const mine = !!m.from && m.from.via === ME && m.from.name === me;
  const forMe = !!m.mention && m.mention === me;
  const answer = m.prompt ? list.find((x) => x.decision?.promptId === m.prompt!.id) || null : null;

  if (kind === "system" && !m.prompt && !m.files?.length) {
    return (
      <div className={`pl-11 text-sm leading-6 ${tone(m.text)} ${forMe ? "border-l-2 border-accent -ml-px pl-[2.6rem]" : ""}`} title={clock(m.at)}>
        <Mrkdwn text={m.text} mention={m.mention} />
      </div>
    );
  }
  return (
    <div className="flex gap-3">
      {kind === "human" ? (
        <Avatar size="sm" color={mine ? "accent" : "default"} className="mt-0.5 shrink-0">
          <Avatar.Fallback>{initials(m.from!.name)}</Avatar.Fallback>
        </Avatar>
      ) : kind === "agent" ? (
        <div className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-full bg-background-secondary text-base">🤖</div>
      ) : (
        <div className="size-8 shrink-0" />
      )}
      <div className="min-w-0 flex-1">
        {kind !== "system" ? (
          <div className="mb-0.5 flex items-center gap-2 text-xs text-muted">
            <b className="text-foreground">{kind === "human" ? m.from!.name : "agent"}</b>
            {m.from && m.from.via !== ME ? (
              <Chip size="sm" variant="soft">
                via {label(m.from.via)}
              </Chip>
            ) : null}
            <span>{clock(m.at)}</span>
          </div>
        ) : null}
        <div
          className={
            kind === "human"
              ? `rounded-md px-3 py-2 ${mine ? "bg-accent-soft text-accent-soft-foreground" : "border border-border bg-surface"}`
              : kind === "agent"
                ? "rounded-md border border-border bg-surface px-3 py-2"
                : `rounded-md border border-border bg-surface px-3 py-2 text-sm ${forMe ? "border-l-2 border-l-accent" : ""}`
          }
        >
          {m.decision ? (
            <span>
              → <b>{m.decision.choice}</b>
            </span>
          ) : kind === "agent" ? (
            <div className="md">
              <Markdown remarkPlugins={[remarkGfm]} components={{ a: (p) => <a {...p} target="_blank" rel="noopener" /> }}>
                {m.text}
              </Markdown>
            </div>
          ) : kind === "human" ? (
            <span className="whitespace-pre-wrap">{linkify(m.text)}</span>
          ) : (
            <Mrkdwn text={m.text} mention={m.mention} />
          )}
          {m.files?.length ? <Files files={m.files} /> : null}
          {m.prompt ? <PromptCard m={m} answer={answer} open={isLast} /> : null}
        </div>
      </div>
    </div>
  );
}
