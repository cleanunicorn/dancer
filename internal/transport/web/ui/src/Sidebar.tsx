// The rack: every thread is a strip, racked in its channel's bay. The
// strips that need a human are moved to a bay of their own at the top, lit
// and cocked out of the rack, so the controller never hunts for them.
import { Button, Tooltip } from "@heroui/react";
import type { Channel, Message, Thread } from "./api";
import { ME } from "./api";
import { Choices } from "./Message";
import { plain } from "./mrkdwn";
import { ago, label, store, useStore } from "./store";
import { Flag, Lamp, state } from "./strip";

function Strip({ t, active, channel }: { t: Thread; active: boolean; channel?: string }) {
  const fresh = store.isFresh(t);
  const s = state(t);
  const line = t.live || (channel ? "#" + channel : "") || (t.requester ? "by " + t.requester : "");
  return (
    <a
      href={"#" + t.id}
      onClick={(e) => {
        e.preventDefault();
        store.openThread(t.id);
      }}
      className="strip"
      data-state={s}
      data-host={t.transport && t.transport !== ME ? t.transport : undefined}
      data-current={active || undefined}
      aria-current={active ? "page" : undefined}
    >
      <Lamp state={s} />
      <span className={"title" + (fresh ? " fresh" : "")}>{t.title || t.id}</span>
      <span className="line" title={line}>
        {line || " "}
      </span>
      <span className="flag-slot">
        {t.unread ? <span className="stamp">{t.unread} NEW</span> : null}
        <Flag state={s} />
      </span>
      <span className="when">{ago(t.updated)}</span>
    </a>
  );
}

// Ask is what a cocked strip waits for, under it in the bay, with a
// permission's Allow/Deny right there: the rack keeps the open prompt of
// every thread, so the controller answers without pulling the strip. A
// question (options, free text) is answered on the desk.
function Ask({ t, me }: { t: Thread; me: string }) {
  if (!t.prompt || !t.ask) return null;
  const m: Message = { id: 0, thread: t.id, at: t.updated, text: t.ask, prompt: t.prompt, mention: t.mention };
  const quick = !!t.prompt.choices?.length && !t.prompt.options?.length;
  return (
    <div className="strip-ask" data-mention={t.mention && t.mention === me ? "me" : undefined}>
      <span className="text" title={plain(t.ask)}>
        {plain(t.ask)}
      </span>
      {quick ? <Choices m={m} open inline /> : <span className="hint">pull the strip to answer</span>}
    </div>
  );
}

const ORDER: Record<string, number> = { wait: 0, run: 1, queue: 1, fail: 2 };
function byUrgency(a: Thread, b: Thread): number {
  const d = (ORDER[state(a)] ?? 3) - (ORDER[state(b)] ?? 3);
  return d || +new Date(b.updated) - +new Date(a.updated);
}

export function Sidebar({ onNavigate }: { onNavigate: () => void }) {
  const st = useStore();
  const groups = new Map<string, (Channel & { implicit?: boolean })[]>();
  for (const c of st.channels.values()) {
    if (!groups.has(c.transport)) groups.set(c.transport, []);
    groups.get(c.transport)!.push(c);
  }
  const byChannel = new Map<string, Thread[]>();
  for (const t of st.threads.values()) {
    if (!byChannel.has(t.channel)) byChannel.set(t.channel, []);
    byChannel.get(t.channel)!.push(t);
  }
  for (const [ch, threads] of byChannel) {
    if (st.channels.has(ch)) continue;
    const tr = threads[0].transport || "other";
    if (!groups.has(tr)) groups.set(tr, []);
    groups.get(tr)!.push({ id: ch, name: ch, transport: tr, implicit: true });
  }
  const order = [...groups.keys()].sort((a, b) => (a === ME ? -1 : b === ME ? 1 : a.localeCompare(b)));
  const waiting = [...st.threads.values()].filter((t) => state(t) === "wait").sort(byUrgency);
  const name = (t: Thread) => st.channels.get(t.channel)?.name || t.channel;

  return (
    <nav className="flex-1 overflow-y-auto pb-3" aria-label="Threads">
      {!groups.size ? <div className="px-4 py-3 text-sm">No channels — check server.transports</div> : null}
      {waiting.length ? (
        <section aria-label="Needs you">
          <div className="bay-label">
            <span className="lamp" data-state="wait" aria-hidden="true" />
            needs you · {waiting.length}
          </div>
          {waiting.map((t) => (
            <div key={t.id}>
              <div onClick={onNavigate}>
                <Strip t={t} active={t.id === st.current} channel={name(t)} />
              </div>
              <Ask t={t} me={st.me} />
            </div>
          ))}
        </section>
      ) : null}
      {order.map((tr) => (
        <section key={tr} aria-label={label(tr)}>
          {groups
            .get(tr)!
            .sort((a, b) => a.name.localeCompare(b.name))
            .map((c) => {
              // a waiting strip is racked in the needs-you bay, not cloned here
              const all = byChannel.get(c.id) || [];
              const threads = all.filter((t) => state(t) !== "wait").sort(byUrgency);
              const cocked = all.length - threads.length;
              return (
                <div key={c.id}>
                  <div className="bay-label">
                    <span>
                      {label(tr).toLowerCase()} · #{c.name}
                    </span>
                    {!c.implicit ? (
                      <Tooltip delay={400}>
                        <Tooltip.Trigger className="order-last">
                          <Button
                            isIconOnly
                            size="sm"
                            variant="ghost"
                            className="-my-1 h-6 w-6 min-w-0 text-rack-ink"
                            aria-label={"New thread in #" + c.name}
                            onPress={() => {
                              store.newThread(c.id);
                              onNavigate();
                            }}
                          >
                            +
                          </Button>
                        </Tooltip.Trigger>
                        <Tooltip.Content>New thread in #{c.name}</Tooltip.Content>
                      </Tooltip>
                    ) : null}
                  </div>
                  {!threads.length ? (
                    <div className="px-4 pb-2 pt-1 font-strip text-[11px]">
                      {cocked ? (cocked === 1 ? "its one strip needs you, above" : cocked + " strips need you, above") : "empty bay"}
                    </div>
                  ) : null}
                  {threads.slice(0, 50).map((t) => (
                    <div key={t.id} onClick={onNavigate}>
                      <Strip t={t} active={t.id === st.current} />
                    </div>
                  ))}
                  {threads.length > 50 ? <div className="px-4 pb-2 font-strip text-[11px]">{threads.length - 50} older strips not shown</div> : null}
                </div>
              );
            })}
        </section>
      ))}
    </nav>
  );
}
