// Types and calls of the web transport's API (internal/transport/web).

export const ME = "web"; // Inbound.Transport of this UI

export interface Channel {
  id: string;
  name: string;
  transport: string;
}

export interface Thread {
  id: string;
  transport: string;
  channel: string;
  title: string;
  status?: string;
  closed?: boolean;
  requester?: string;
  updated: string;
  agent?: string; // definition name
  model?: string; // what the session resolved to, else what was asked for
  env?: string; // environment kind
  session?: string; // agent session id
  live?: string; // status line up right now
  waiting?: boolean; // a prompt is open
  ask?: string; // what the open prompt says (first line)
  prompt?: Prompt; // the open prompt, when known
  mention?: string; // who the open prompt is for
  unread?: number; // client-side
}

export interface Agent {
  name: string;
  model?: string;
  env?: string;
}

export interface ToolCall {
  id: string;
  name: string;
  input?: string;
  sub?: boolean; // run by a sub-agent
  done?: boolean;
  error?: boolean;
  denied?: boolean;
  millis?: number;
}

export interface Prompt {
  id: string;
  choices?: string[];
  question?: string;
  options?: { value: string; label: string; description?: string }[];
  freeText?: boolean;
}

export interface Message {
  id: number;
  thread: string;
  at: string;
  text: string;
  markdown?: boolean;
  mention?: string;
  key?: string;
  prompt?: Prompt;
  from?: { id: string; name: string; via: string };
  decision?: { promptId: string; choice: string };
  files?: { name: string; size: number; data?: string }[];
  tool?: ToolCall; // a tool the agent ran; nothing else is set
}

export type Event =
  | { type: "hello" }
  | { type: "message"; message: Message }
  | { type: "edit"; message: Message }
  | { type: "remove"; thread: string; id: number }
  | { type: "thread"; threadInfo: Thread };

export class ApiError extends Error {
  constructor(message: string, public status: number) {
    super(message);
  }
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError((data as { error?: string }).error || res.statusText, res.status);
  return data as T;
}

export const storage = {
  get<T>(k: string, d: T): T {
    try {
      const v = localStorage.getItem("dancer." + k);
      return v == null ? d : (JSON.parse(v) as T);
    } catch {
      return d;
    }
  },
  set(k: string, v: unknown) {
    localStorage.setItem("dancer." + k, JSON.stringify(v));
  },
};
