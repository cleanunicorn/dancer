import { useEffect, useState } from "react";
import { Button, Dropdown, Input, Kbd, Label, Modal, TextField } from "@heroui/react";
import { Sidebar } from "./Sidebar";
import { ThreadPane } from "./Thread";
import { store, useStore } from "./store";

function LoginPage() {
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <div className="flex h-full items-center justify-center bg-background px-4">
      <form
        className="flex w-full max-w-sm flex-col gap-4 rounded-md border border-border bg-surface p-6"
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
        <div>
          <div className="text-lg font-semibold">🕺 dancer</div>
          <p className="text-sm text-muted">Sign in with your dancer account.</p>
        </div>
        <TextField aria-label="Name" autoFocus>
          <Label>Name</Label>
          <Input autoComplete="username" value={name} onChange={(e) => setName(e.target.value)} />
        </TextField>
        <TextField aria-label="Password">
          <Label>Password</Label>
          <Input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </TextField>
        {err ? <p className="text-sm text-danger">{err}</p> : null}
        <Button type="submit" variant="primary" isDisabled={busy || !name.trim() || !password}>
          Sign in
        </Button>
        <p className="text-xs text-muted">
          No account? The operator makes one with <code>dancer user add &lt;name&gt;</code>.
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
                {err ? <p className="text-sm text-danger">{err}</p> : null}
                <p className="text-xs text-muted">Your other browsers are signed out.</p>
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
            <Modal.Body>
              <table className="text-sm">
                <tbody>
                  {COMMANDS.map(([c, d]) => (
                    <tr key={c}>
                      <td className="py-0.5 pr-4 align-top">
                        <code className="rounded bg-background-secondary px-1">{c}</code>
                      </td>
                      <td className="py-0.5 text-muted">{d}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <p className="mt-3 flex flex-wrap items-center gap-1 text-xs text-muted">
                <Kbd>Enter</Kbd> send · <Kbd>Shift</Kbd>+<Kbd>Enter</Kbd> newline · <Kbd>/</Kbd> focus the box · <Kbd>Esc</Kbd> close
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
    document.title = (waiting ? "✋ " : "") + (unread ? `(${unread}) ` : "") + "dancer";
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
        className={`fixed inset-y-0 left-0 z-20 flex w-72 flex-col border-r border-border bg-background-secondary transition-transform md:static md:translate-x-0 ${menu ? "translate-x-0" : "-translate-x-full"}`}
      >
        <div className="flex items-center justify-between border-b border-border px-4 py-3">
          <span className="font-semibold">🕺 dancer</span>
          <Button isIconOnly size="sm" variant="ghost" aria-label="Commands" onPress={() => setHelp(true)}>
            ?
          </Button>
        </div>
        <Sidebar onNavigate={() => setMenu(false)} />
        <div className="flex items-center justify-between border-t border-border px-4 py-2 text-xs text-muted">
          <Dropdown>
            <Button size="sm" variant="ghost" className="-ml-2 text-xs">
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
          <span className={st.connected ? "text-success" : "text-danger"} title={st.connected ? "Connected" : "Reconnecting…"}>
            ●
          </span>
        </div>
      </aside>
      {menu ? <div className="fixed inset-0 z-10 bg-backdrop md:hidden" onClick={() => setMenu(false)} /> : null}
      <ThreadPane menu={() => setMenu(true)} />
      <PasswordDialog open={password} onClose={() => setPassword(false)} />
      <HelpDialog open={help} onClose={() => setHelp(false)} />
      {st.toast ? (
        <div className="fixed bottom-4 left-1/2 z-30 -translate-x-1/2 rounded-md border border-border bg-surface px-3 py-2 text-sm">{st.toast}</div>
      ) : null}
    </div>
  );
}
