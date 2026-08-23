import { Button, Chip, Tooltip } from "@heroui/react";
import type { Channel, Thread } from "./api";
import { ME } from "./api";
import { ago, label, store, useStore } from "./store";

function icon(t: Thread): { text: string; title: string } {
  if (t.waiting) return { text: "✋", title: "waiting for an answer" };
  if (t.live) return { text: "⏳", title: t.live };
  if (t.status === "running" || t.status === "queued") return { text: "⏳", title: t.status };
  if (t.status === "failed") return { text: "❌", title: "failed" };
  if (t.closed) return { text: "✓", title: "closed" };
  return { text: "", title: t.status || "" };
}

function ThreadRow({ t, active }: { t: Thread; active: boolean }) {
  const fresh = store.isFresh(t);
  const ic = icon(t);
  return (
    <a
      href={"#" + t.id}
      onClick={(e) => {
        e.preventDefault();
        store.openThread(t.id);
      }}
      className={`mx-2 flex items-center gap-2 rounded-md px-2 py-1.5 text-sm ${active ? "bg-surface text-foreground" : "hover:bg-surface-hover"} ${t.closed ? "text-muted" : ""}`}
    >
      <span className="w-5 shrink-0 text-center text-xs" title={ic.title}>
        {ic.text}
      </span>
      <span className={`min-w-0 flex-1 truncate ${fresh ? "font-semibold" : ""}`}>{t.title || t.id}</span>
      {t.unread ? (
        <Chip size="sm" color="accent" variant="primary">
          {t.unread}
        </Chip>
      ) : (
        <span className="text-xs text-muted">{ago(t.updated)}</span>
      )}
    </a>
  );
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
  return (
    <nav className="flex-1 overflow-y-auto py-2">
      {!groups.size ? <div className="px-4 py-2 text-sm text-muted">No channels — check server.transports</div> : null}
      {order.map((tr) => (
        <div key={tr} className="mb-3">
          <div className="px-4 pb-1 pt-2 text-[11px] font-medium uppercase tracking-wider text-muted">{label(tr)}</div>
          {groups
            .get(tr)!
            .sort((a, b) => a.name.localeCompare(b.name))
            .map((c) => {
              const threads = (byChannel.get(c.id) || []).sort((a, b) => +new Date(b.updated) - +new Date(a.updated));
              return (
                <div key={c.id} className="mb-1">
                  <div className="flex items-center gap-1 pl-4 pr-2 text-sm font-medium">
                    <span className="text-muted">#</span>
                    <span className="truncate">{c.name}</span>
                    {!c.implicit ? (
                      <Tooltip delay={400}>
                        <Tooltip.Trigger className="ml-auto">
                          <Button
                            isIconOnly
                            size="sm"
                            variant="ghost"
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
                  {!threads.length ? <div className="pl-8 pr-3 text-xs text-muted">no threads yet</div> : null}
                  {threads.slice(0, 50).map((t) => (
                    <div key={t.id} onClick={onNavigate}>
                      <ThreadRow t={t} active={t.id === st.current} />
                    </div>
                  ))}
                  {threads.length > 50 ? <div className="pl-8 text-xs text-muted">{threads.length - 50} older threads not shown</div> : null}
                </div>
              );
            })}
        </div>
      ))}
    </nav>
  );
}
