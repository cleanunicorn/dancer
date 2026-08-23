// The page's state: what the server lists, what it streams, and what the
// human is looking at. Everything shown comes from the server; the page
// keeps only the thread it has open and a few per-session counters.
import { useSyncExternalStore } from "react";
import { ME, api, ApiError, storage, type Channel, type Event, type Message, type Thread } from "./api";

export interface Draft {
  channel: string;
  text?: string; // set once sent; the thread event or our relayed message opens it
}

export interface State {
  me: string;
  needLogin: boolean;
  channels: Map<string, Channel>;
  threads: Map<string, Thread>;
  messages: Map<string, Message[]>; // only threads opened this session
  current: string | null;
  draft: Draft | null;
  connected: boolean;
  toast: string | null;
}

type Listener = () => void;

class Store {
  state: State = {
    me: storage.get("name", ""),
    needLogin: false,
    channels: new Map(),
    threads: new Map(),
    messages: new Map(),
    current: null,
    draft: null,
    connected: false,
    toast: null,
  };
  private listeners = new Set<Listener>();
  private seen = storage.get<Record<string, string>>("seen", {});
  private es: EventSource | null = null;
  private everConnected = false;
  private toastTimer: number | undefined;

  subscribe = (l: Listener) => {
    this.listeners.add(l);
    return () => this.listeners.delete(l);
  };
  snapshot = () => this.state;

  private set(patch: Partial<State>) {
    this.state = { ...this.state, ...patch };
    for (const l of this.listeners) l();
  }
  // touch republishes after in-place edits of the maps.
  private touch() {
    this.set({ threads: new Map(this.state.threads), messages: this.state.messages });
  }

  /* ---------- bootstrap ---------- */

  async start() {
    try {
      await this.loadState();
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        this.set({ needLogin: true });
        return;
      }
      this.toast(String(e));
    }
    this.connect();
    this.route();
    window.addEventListener("hashchange", () => this.route());
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && this.state.current) this.markSeen(this.state.current);
    });
  }

  async login(token: string) {
    await api("POST", "/api/login", { token });
    this.set({ needLogin: false });
    await this.start();
  }

  setName(name: string) {
    storage.set("name", name);
    this.set({ me: name });
    if ("Notification" in window && Notification.permission === "default") Notification.requestPermission();
  }

  async loadState() {
    const st = await api<{ channels: Channel[]; threads: Thread[] }>("GET", "/api/state");
    const channels = new Map(st.channels.map((c) => [c.id, c]));
    const threads = new Map<string, Thread>();
    for (const t of st.threads) {
      const prev = this.state.threads.get(t.id);
      threads.set(t.id, prev ? { ...prev, ...t } : t);
    }
    // threads we opened this session that the server does not list yet
    for (const [id, t] of this.state.threads) if (!threads.has(id) && !t.status) threads.set(id, t);
    this.set({ channels, threads });
  }

  private route() {
    const h = decodeURIComponent(location.hash.slice(1));
    if (h.startsWith("new:")) {
      if (!this.state.draft || this.state.draft.channel !== h.slice(4)) this.newThread(h.slice(4));
    } else if (h && h !== this.state.current && this.state.threads.has(h)) {
      this.openThread(h);
    }
  }

  /* ---------- events ---------- */

  private connect() {
    this.es?.close();
    this.es = new EventSource("/api/events");
    this.es.onopen = () => this.setConnected(true);
    this.es.onerror = () => this.setConnected(false);
    this.es.onmessage = (e) => {
      let ev: Event;
      try {
        ev = JSON.parse(e.data);
      } catch {
        return;
      }
      this.handle(ev);
    };
  }

  private setConnected(on: boolean) {
    if (this.state.connected === on) return;
    this.set({ connected: on });
    if (on && this.everConnected) {
      this.loadState().then(() => {
        if (this.state.current) this.reloadThread(this.state.current);
      });
    }
    if (on) this.everConnected = true;
  }

  private handle(ev: Event) {
    switch (ev.type) {
      case "thread": {
        const t = ev.threadInfo;
        this.state.threads.set(t.id, { ...(this.state.threads.get(t.id) || {}), ...t });
        this.touch();
        const d = this.state.draft;
        if (d?.text && d.channel === t.channel) this.openThread(t.id);
        return;
      }
      case "message":
        this.addMessage(ev.message);
        return;
      case "edit":
        this.editMessage(ev.message);
        return;
      case "remove":
        this.removeMessage(ev.thread, ev.id);
        return;
    }
  }

  private thread(id: string): Thread {
    let t = this.state.threads.get(id);
    if (!t) {
      t = { id, channel: id.split("/")[0], transport: "", title: "", updated: new Date().toISOString() };
      this.state.threads.set(id, t);
    }
    return t;
  }

  private addMessage(m: Message) {
    const list = this.state.messages.get(m.thread);
    if (list) {
      if (list.some((x) => x.id === m.id)) return;
      list.push(m);
      this.state.messages.set(m.thread, [...list]);
    }
    const t = this.thread(m.thread);
    t.updated = m.at;
    if (!t.title && m.from && m.text) t.title = firstLine(m.text);
    if (m.prompt) t.waiting = true;
    else if (m.decision || !m.from) t.waiting = false;
    const mine = !!m.from && m.from.via === ME && m.from.name === this.state.me;
    const d = this.state.draft;
    if (mine && d?.text && t.channel === d.channel) {
      this.openThread(m.thread);
    } else if ((this.state.current !== m.thread || document.hidden) && !mine) {
      t.unread = (t.unread || 0) + 1;
    }
    if (this.state.current === m.thread && !document.hidden) this.markSeen(m.thread);
    this.touch();
    this.notify(m, t);
  }

  private editMessage(m: Message) {
    const list = this.state.messages.get(m.thread);
    if (list) {
      const i = list.findIndex((x) => x.id === m.id);
      if (i >= 0) list[i] = { ...list[i], ...m };
      else list.push(m);
      this.state.messages.set(m.thread, [...list]);
    }
    this.thread(m.thread).live = m.text;
    this.touch();
  }

  private removeMessage(th: string, id: number) {
    const list = this.state.messages.get(th);
    if (list) this.state.messages.set(th, list.filter((x) => x.id !== id));
    const t = this.state.threads.get(th);
    if (t) t.live = "";
    this.touch();
  }

  private notify(m: Message, t: Thread) {
    if (!("Notification" in window) || Notification.permission !== "granted") return;
    if (!m.prompt) return;
    if (m.mention && m.mention !== this.state.me) return;
    if (!document.hidden && this.state.current === m.thread) return;
    const n = new Notification("dancer needs an answer", {
      body: (t.title ? t.title + " — " : "") + m.text.replace(/[*_`]/g, "").slice(0, 120),
      tag: m.thread,
    });
    n.onclick = () => {
      window.focus();
      this.openThread(m.thread);
    };
  }

  /* ---------- navigation ---------- */

  async openThread(id: string) {
    this.thread(id);
    this.set({ current: id, draft: null });
    location.hash = id;
    this.markSeen(id);
    if (!this.state.messages.has(id)) await this.reloadThread(id);
  }

  async reloadThread(id: string) {
    try {
      const data = await api<{ messages: Message[] }>("GET", "/api/threads/" + id);
      const msgs = data.messages || [];
      this.state.messages.set(id, msgs);
      const t = this.thread(id);
      const live = msgs.find((m) => m.key);
      t.live = live ? live.text : "";
      const last = [...msgs].reverse().find((m) => !m.key);
      if (last) t.waiting = !!(last.prompt && !msgs.some((m) => m.decision?.promptId === last.prompt!.id));
      this.touch();
    } catch (e) {
      this.toast(String(e));
    }
  }

  newThread(channel: string) {
    if (!this.state.channels.has(channel)) return;
    this.set({ current: null, draft: { channel } });
    location.hash = "new:" + channel;
  }

  markSeen(id: string) {
    const t = this.state.threads.get(id);
    if (!t) return;
    t.unread = 0;
    this.seen[id] = t.updated;
    storage.set("seen", this.seen);
    this.touch();
  }

  isFresh(t: Thread): boolean {
    return !!t.unread || (!!t.updated && this.seen[t.id] !== t.updated && this.state.current !== t.id && t.requester === this.state.me);
  }

  /* ---------- actions ---------- */

  async send(text: string): Promise<boolean> {
    const body: Record<string, string> = { text, user: this.state.me };
    if (this.state.current) body.thread = this.state.current;
    else if (this.state.draft) {
      body.channel = this.state.draft.channel;
      this.set({ draft: { ...this.state.draft, text } });
      window.setTimeout(() => {
        const d = this.state.draft;
        if (d?.text === text) {
          this.toast("The thread did not open — see the server log");
          this.set({ draft: { channel: d.channel } });
        }
      }, 15000);
    } else return false;
    try {
      await api("POST", "/api/messages", body);
      return true;
    } catch (e) {
      this.toast(String(e));
      if (this.state.draft) this.set({ draft: { channel: this.state.draft.channel } });
      return false;
    }
  }

  async decide(thread: string, promptId: string, choice: string) {
    await api("POST", "/api/decide", { thread, promptId, choice, user: this.state.me });
    // the relayed decision arrives on the stream and settles the prompt
  }

  toast(text: string) {
    this.set({ toast: text.replace(/^Error: /, "") });
    window.clearTimeout(this.toastTimer);
    this.toastTimer = window.setTimeout(() => this.set({ toast: null }), 4000);
  }
}

export const store = new Store();

export function useStore(): State {
  return useSyncExternalStore(store.subscribe, store.snapshot);
}

export function firstLine(s: string): string {
  s = s.trim();
  const i = s.indexOf("\n");
  if (i >= 0) s = s.slice(0, i);
  return s.length > 120 ? s.slice(0, 119) + "…" : s;
}

export function label(transport: string): string {
  if (transport === ME) return "Web";
  return transport ? transport.charAt(0).toUpperCase() + transport.slice(1) : "Other";
}

export function ago(at: string): string {
  const s = Math.max(0, (Date.now() - new Date(at).getTime()) / 1000);
  if (s < 60) return "now";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

export function clock(at: string): string {
  if (!at || at.startsWith("0001")) return "";
  return new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
