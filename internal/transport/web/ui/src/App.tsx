import { useEffect, useState } from "react";
import { Button, Dropdown, Input, Label, Modal, TextField } from "@heroui/react";
import { Sidebar } from "./Sidebar";
import { ThreadPane } from "./Thread";
import { Mark } from "./strip";
import { store, useStore } from "./store";

// The sign-in is one strip on an empty desk.
function LoginPage() {
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <div className="flex h-full items-center justify-center px-4">
      <form
        className="desk-strip flex w-full max-w-sm flex-col gap-4 px-6 py-5"
        data-state="new"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setErr("");
          try {
            await store.login(name.trim(), password);
          } catch (x) {
            setErr(String(x).replace(/^Error: /, ""));
          }
          setBusy(false);
        }}
      >
        <div className="flex items-center gap-2.5">
          <Mark size={22} />
          <span className="text-lg font-semibold leading-none">dispatch</span>
          <span className="flag ml-auto" data-state="new">
            SIGN IN
          </span>
        </div>
        <TextField aria-label="Name" autoFocus>
          <Label className="font-strip text-[10px] uppercase tracking-[0.1em] text-ink-2">Name</Label>
          <Input autoComplete="username" value={name} onChange={(e) => setName(e.target.value)} />
        </TextField>
        <TextField aria-label="Password">
          <Label className="font-strip text-[10px] uppercase tracking-[0.1em] text-ink-2">Password</Label>
          <Input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </TextField>
        {err ? (
          <p className="text-sm text-danger" role="alert">
            {err}
          </p>
        ) : null}
        <Button type="submit" variant="primary" isDisabled={busy || !name.trim() || !password} className="font-strip uppercase tracking-wider">
          {busy ? "Signing in…" : "Sign in"}
        </Button>
        <p className="font-strip text-[11px] text-ink-2">
          no account? the operator makes one with <code>dispatch user add &lt;name&gt;</code>
        </p>
      </form>
    </div>
  );
}

function PasswordDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [err, setErr] = useState("");
  return (
    <Modal isOpen={open} onOpenChange={(o) => !o && onClose()}>
      <Modal.Backdrop>
        <Modal.Container size="sm">
          <Modal.Dialog>
            <Modal.CloseTrigger />
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                try {
                  await store.changePassword(current, next);
                  setCurrent("");
                  setNext("");
                  setErr("");
                  store.toast("Password changed");
                  onClose();
                } catch (x) {
                  setErr(String(x).replace(/^Error: /, ""));
                }
              }}
            >
              <Modal.Header>
                <Modal.Heading>Change password</Modal.Heading>
              </Modal.Header>
              <Modal.Body className="flex flex-col gap-3">
                <TextField aria-label="Current password">
                  <Label>Current password</Label>
                  <Input type="password" autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} />
                </TextField>
                <TextField aria-label="New password">
                  <Label>New password</Label>
                  <Input type="password" autoComplete="new-password" minLength={8} value={next} onChange={(e) => setNext(e.target.value)} />
                </TextField>
                {err ? (
                  <p className="text-sm text-danger" role="alert">
                    {err}
                  </p>
                ) : null}
                <p className="text-xs text-muted">At least 8 characters. Your other browsers are signed out.</p>
              </Modal.Body>
              <Modal.Footer>
                <Button type="submit" variant="primary" isDisabled={!current || next.length < 8}>
                  Change
                </Button>
              </Modal.Footer>
            </form>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </Modal>
  );
}

const COMMANDS: [string, string][] = [
  ["<prompt>", "start a task with the channel's default agent"],
  ["run <agent> <prompt>", "start a task with a specific agent"],
  ["run", "pick the agent from a list, then type the prompt"],
  ["default <agent>", "set this channel's default agent"],
  ["anything else", "follow-up to the task in this thread"],
  ["status", "state of the task in this thread"],
  ["cancel", "stop the task"],
  ["close", "stop the task and end the thread"],
  ["agent list", "list agents (agent add / edit / delete)"],
];

const FLAGS: [string, string][] = [
  ["WAIT", "the agent asked something; the strip is lit and cocked"],
  ["RUN", "the agent is working"],
  ["IDLE", "turn finished; a reply continues the session"],
  ["DONE", "the task is over; a reply starts the next turn"],
  ["FAIL", "the task failed"],
  ["INTR", "cut short by a restart; a reply resumes it"],
  ["CLSD", "thread closed; writing to it reopens it"],
];

function HelpDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    <Modal isOpen={open} onOpenChange={(o) => !o && onClose()}>
      <Modal.Backdrop>
        <Modal.Container size="md">
          <Modal.Dialog>
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Commands</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <table className="text-sm">
                <tbody>
                  {COMMANDS.map(([c, d]) => (
                    <tr key={c}>
                      <td className="py-0.5 pr-4 align-top">
                        <code className="font-strip rounded-[2px] bg-surface-secondary px-1 text-[12px]">{c}</code>
                      </td>
                      <td className="py-0.5 text-muted">{d}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <div>
                <div className="font-strip mb-1 text-[10px] uppercase tracking-[0.1em] text-muted">Flags on a strip</div>
                <table className="text-sm">
                  <tbody>
                    {FLAGS.map(([f, d]) => (
                      <tr key={f}>
                        <td className="py-0.5 pr-4 align-top">
                          <span className="flag" data-surface="console">{f}</span>
                        </td>
                        <td className="py-0.5 text-muted">{d}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <p className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-muted">
                <span className="kbd">Enter</span> send · <span className="kbd">Shift</span>+<span className="kbd">Enter</span> newline · <span className="kbd">/</span> focus the printer ·{" "}
                <span className="kbd">Esc</span> close
              </p>
            </Modal.Body>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </Modal>
  );
}

export default function App() {
  const st = useStore();
  const [password, setPassword] = useState(false);
  const [help, setHelp] = useState(false);
  const [menu, setMenu] = useState(false);

  useEffect(() => {
    store.start();
  }, []);
  useEffect(() => {
    let waiting = 0,
      unread = 0;
    for (const t of st.threads.values()) {
      if (t.waiting) waiting++;
      unread += t.unread || 0;
    }
    document.title = (waiting ? "✋ " : "") + (unread ? `(${unread}) ` : "") + "dispatch";
  }, [st.threads]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenu(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  if (st.needLogin) return <LoginPage />;
  if (!st.me) return null;

  return (
    <div className="flex h-full">
      <aside
        className={`rack fixed inset-y-0 left-0 z-20 flex w-[19.5rem] flex-col transition-transform md:static md:translate-x-0 ${menu ? "translate-x-0" : "-translate-x-full"}`}
        aria-label="The rack"
      >
        <div className="flex items-center gap-2.5 px-4 pb-2 pt-3">
          <Mark />
          <span className="text-[15px] font-semibold leading-none text-foreground">dispatch</span>
          <span className="font-strip ml-1 text-[10px] uppercase tracking-[0.12em]">strip board</span>
          <Button isIconOnly size="sm" variant="ghost" className="-mr-2 ml-auto h-7 w-7 min-w-0 font-strip text-rack-ink" aria-label="Commands and flags" onPress={() => setHelp(true)}>
            ?
          </Button>
        </div>
        <Sidebar onNavigate={() => setMenu(false)} />
        <div className="flex items-center justify-between border-t border-rack-rule px-4 py-2">
          <Dropdown>
            <Button size="sm" variant="ghost" className="-ml-2 h-7 font-strip text-[11px] text-rack-ink">
              {st.me}
            </Button>
            <Dropdown.Popover>
              <Dropdown.Menu
                aria-label="Account"
                onAction={(key) => {
                  if (key === "password") setPassword(true);
                  if (key === "logout") store.logout();
                }}
              >
                <Dropdown.Item id="password">Change password…</Dropdown.Item>
                <Dropdown.Item id="logout">Sign out</Dropdown.Item>
              </Dropdown.Menu>
            </Dropdown.Popover>
          </Dropdown>
          <span className="flex items-center gap-1.5 font-strip text-[10px] uppercase tracking-[0.1em]" title={st.connected ? "Connected" : "Reconnecting…"}>
            <span className="lamp !static" data-state={st.connected ? "run" : "fail"} aria-hidden="true" />
            {st.connected ? "link" : "no link"}
          </span>
        </div>
      </aside>
      {menu ? <div className="fixed inset-0 z-10 bg-backdrop md:hidden" onClick={() => setMenu(false)} /> : null}
      <ThreadPane menu={() => setMenu(true)} />
      <PasswordDialog open={password} onClose={() => setPassword(false)} />
      <HelpDialog open={help} onClose={() => setHelp(false)} />
      {st.toast ? (
        <div role="status" className="desk-strip fixed bottom-4 left-1/2 z-30 -translate-x-1/2 px-4 py-2 text-sm" data-state="new">
          {st.toast}
        </div>
      ) : null}
    </div>
  );
}
