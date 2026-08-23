import { useEffect, useState } from "react";
import { Button, Input, Kbd, Modal, TextField } from "@heroui/react";
import { Sidebar } from "./Sidebar";
import { ThreadPane } from "./Thread";
import { store, useStore } from "./store";

function Dialog({ open, title, children }: { open: boolean; title: string; children: React.ReactNode }) {
  return (
    <Modal isOpen={open}>
      <Modal.Backdrop isDismissable={false}>
        <Modal.Container size="sm">
          <Modal.Dialog>
            <Modal.Header>
              <Modal.Heading>{title}</Modal.Heading>
            </Modal.Header>
            {children}
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </Modal>
  );
}

function NameDialog({ open, onDone }: { open: boolean; onDone: () => void }) {
  const st = useStore();
  const [name, setName] = useState(st.me);
  return (
    <Dialog open={open} title="What should dancer call you?">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!name.trim()) return;
          store.setName(name.trim());
          onDone();
        }}
      >
        <Modal.Body className="flex flex-col gap-3">
          <p className="text-sm text-muted">Your messages are signed with it, and the agent addresses you by it.</p>
          <TextField aria-label="Name" autoFocus>
            <Input placeholder="your name" maxLength={40} value={name} onChange={(e) => setName(e.target.value)} />
          </TextField>
        </Modal.Body>
        <Modal.Footer>
          <Button type="submit" variant="primary" isDisabled={!name.trim()}>
            Continue
          </Button>
        </Modal.Footer>
      </form>
    </Dialog>
  );
}

function LoginDialog({ open }: { open: boolean }) {
  const [token, setToken] = useState("");
  const [err, setErr] = useState("");
  return (
    <Dialog open={open} title="Access token">
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            await store.login(token);
          } catch (x) {
            setErr(String(x).replace(/^Error: /, ""));
          }
        }}
      >
        <Modal.Body className="flex flex-col gap-3">
          <p className="text-sm text-muted">
            This dancer asks for the token from its config (<code>[web] token</code>).
          </p>
          <TextField aria-label="Token" autoFocus>
            <Input type="password" placeholder="token" value={token} onChange={(e) => setToken(e.target.value)} />
          </TextField>
          {err ? <p className="text-sm text-danger">{err}</p> : null}
        </Modal.Body>
        <Modal.Footer>
          <Button type="submit" variant="primary" isDisabled={!token}>
            Sign in
          </Button>
        </Modal.Footer>
      </form>
    </Dialog>
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
  const [askName, setAskName] = useState(!st.me);
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
          <button className="hover:text-foreground hover:underline" onClick={() => setAskName(true)} title="Change your name">
            {st.me || "—"}
          </button>
          <span className={st.connected ? "text-success" : "text-danger"} title={st.connected ? "Connected" : "Reconnecting…"}>
            ●
          </span>
        </div>
      </aside>
      {menu ? <div className="fixed inset-0 z-10 bg-backdrop md:hidden" onClick={() => setMenu(false)} /> : null}
      <ThreadPane menu={() => setMenu(true)} />
      <NameDialog open={askName && !st.needLogin} onDone={() => setAskName(false)} />
      <LoginDialog open={st.needLogin} />
      <HelpDialog open={help} onClose={() => setHelp(false)} />
      {st.toast ? (
        <div className="fixed bottom-4 left-1/2 z-30 -translate-x-1/2 rounded-md border border-border bg-surface px-3 py-2 text-sm">{st.toast}</div>
      ) : null}
    </div>
  );
}
