// The strip vocabulary: what state a thread is in, the lamp that shows it,
// the flag printed next to it, and dispatch's mark. Shared by the rack
// (Sidebar), the desk (Thread) and the log (Message).
import type { Thread } from "./api";

export type StripState = "wait" | "run" | "fail" | "closed" | "done" | "idle" | "queue" | "intr" | "cncl" | "new";

// state reads the thread the way a controller reads a strip: does it need
// me, is it moving, did it fail, is it over.
export function state(t: Thread): StripState {
  if (t.closed) return "closed";
  if (t.waiting || t.status === "waiting_permission") return "wait";
  if (t.live || t.status === "running") return "run";
  switch (t.status) {
    case "queued":
      return "queue";
    case "failed":
      return "fail";
    case "done":
      return "done";
    case "idle":
      return "idle";
    case "interrupted":
      return "intr";
    case "cancelled":
      return "cncl";
  }
  return "new";
}

export const FLAG: Record<StripState, string> = {
  wait: "WAIT",
  run: "RUN",
  fail: "FAIL",
  closed: "CLSD",
  done: "DONE",
  idle: "IDLE",
  queue: "QUED",
  intr: "INTR",
  cncl: "CNCL",
  new: "NEW",
};

export const TITLE: Record<StripState, string> = {
  wait: "waiting for a human",
  run: "agent working",
  fail: "failed",
  closed: "closed",
  done: "done",
  idle: "idle — reply to continue",
  queue: "queued",
  intr: "interrupted — reply to resume",
  cncl: "cancelled",
  new: "new",
};

// lit says whether the lamp is on: only the states a controller must act on
// or watch light a lamp.
function lit(s: StripState): "wait" | "run" | "fail" | "off" {
  return s === "wait" || s === "run" || s === "fail" ? s : "off";
}

export function Lamp({ state: s, className }: { state: StripState; className?: string }) {
  return <span className={"lamp " + (className || "")} data-state={lit(s)} aria-hidden="true" />;
}

export function Flag({ state: s }: { state: StripState }) {
  return (
    <span className="flag" data-state={s} title={TITLE[s]}>
      {FLAG[s]}
    </span>
  );
}

// Mark is dispatch's mark: a strip with its lamp lit.
export function Mark({ size = 18 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 20 20" aria-hidden="true" className="shrink-0">
      <rect x="1.5" y="5" width="17" height="10" rx="1" fill="var(--paper)" stroke="var(--ink)" strokeWidth="1" />
      <line x1="4.5" y1="5.5" x2="4.5" y2="14.5" stroke="var(--ink)" strokeWidth="1" strokeDasharray="1 1.2" opacity="0.5" />
      <circle cx="8.5" cy="10" r="1.6" fill="var(--lamp-wait)" />
      <rect x="11" y="8.5" width="5" height="1.3" fill="var(--ink)" opacity="0.75" />
      <rect x="11" y="11" width="3.5" height="1.3" fill="var(--ink)" opacity="0.45" />
    </svg>
  );
}

// elapsed prints a duration the way the strip does: 1m05s, 2h13m, 3d. A
// missing or zero timestamp prints a dash, the way the clock does.
export function elapsed(from: string, to: number = Date.now()): string {
  const at = from ? new Date(from).getTime() : NaN;
  if (!isFinite(at) || at <= 0) return "—";
  const s = Math.max(0, Math.floor((to - at) / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m" + String(s % 60).padStart(2, "0") + "s";
  if (s < 86400) return Math.floor(s / 3600) + "h" + String(Math.floor((s % 3600) / 60)).padStart(2, "0") + "m";
  return Math.floor(s / 86400) + "d";
}
