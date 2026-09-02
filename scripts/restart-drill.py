#!/usr/bin/env python3
"""Graceful-restart drill against the real binary and claude (haiku).

    scripts/restart-drill.py bin/dispatch

Starts a task whose tool call sleeps 8s, sends SIGTERM while it runs, checks
dispatch drains the call and posts the restart notice, then restarts and checks
the "back" notice and that the resumed session remembers the command. A second
leg does the same to a workflow mid-step: the restart grades the step's window,
finds the turn never finished, and the human's retry runs it again.

The two legs get a database and a thread each. The terminal transport has one
thread, and the first leg deliberately leaves a session with a tool call the
restart cut short — sending a workflow's step into *that* is not the thing
being tested, and what the CLI does with the leftover swamps what is.

Auto-resume is off here on purpose; scripts/auto-resume-drill.py covers the
default, where the restarted task carries on without a reply.
"""
import os, select, signal, subprocess, sys, tempfile, time
bin_ = os.path.abspath(sys.argv[1])
def make_config(drain="60s"):
    """A tmp dir, a database and a config of its own, for one leg.

    drain is drain_timeout: the first leg wants the whole 60s, so its tool
    call finishes and the drain is what is being checked. The workflow leg
    wants the opposite — a call that outlasts the drain, so the turn really
    is cut short and the restart has an ungraded step to judge."""
    d = tempfile.mkdtemp(prefix="dispatch-restart-")
    path = os.path.join(d, "config.toml")
    open(path, "w").write(f"""[server]
db = "{d}/dispatch.db"
workdir_root = "{d}/work"
idle_timeout = "60s"
drain_timeout = "{drain}"
transports = ["terminal"]
auto_resume = false          # this drill checks the wait-for-a-reply path

[[definitions]]
name = "coder"
model = "haiku"
permission_mode = "manual"
allowed_tools = ["Read"]

[[workflow]]
name = "drill"

  [[workflow.step]]
  name = "work"
  agent = "coder"
  prompt = "{{{{.Ask}}}}"   # four braces: this is an f-string, the file gets two
""")
    return d, path

tmp, cfg = make_config()
def start(config=None):
    p = subprocess.Popen([bin_, "run", "-config", config or cfg, "-terminal"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1)
    os.set_blocking(p.stdout.fileno(), False); return p
def wait_for(p, pat, timeout=120):
    buf=""; end=time.time()+timeout
    while time.time()<end:
        r,_,_=select.select([p.stdout],[],[],0.2)
        if r:
            c=p.stdout.read()
            if c: buf+=c; sys.stdout.write(c); sys.stdout.flush()
        if pat in buf: return True
        if p.poll() is not None and pat not in buf:
            c=p.stdout.read() or ""; buf+=c; sys.stdout.write(c)
            return pat in buf
    print(f"\n!! timeout waiting for {pat!r}"); return False
def send(p,s): print(f">>> {s}"); p.stdin.write(s+"\n"); p.stdin.flush()
def wait_allowing(p, pat, timeout=240):
    """Wait for pat, answering any permission prompt met on the way.

    A resumed session does not always ask again — the CLI may carry the
    approved call over, or answer without one — so the drill cannot demand
    a prompt and cannot ignore one either."""
    buf=""; end=time.time()+timeout; asked=0
    while time.time()<end:
        r,_,_=select.select([p.stdout],[],[],0.2)
        if r:
            c=p.stdout.read()
            if c: buf+=c; sys.stdout.write(c); sys.stdout.flush()
        if pat in buf: return True
        if buf.count("[allow/deny] >") > asked:
            asked = buf.count("[allow/deny] >"); send(p,"allow")
        if p.poll() is not None:
            c=p.stdout.read() or ""; buf+=c; sys.stdout.write(c)
            return pat in buf
    print(f"\n!! timeout waiting for {pat!r}"); return False
ok=True
p=start(); ok&=wait_for(p,"type `help`")
send(p,"run coder Using Bash run `sleep 8 && touch slept.txt`, then reply exactly: SLEPT")
ok&=wait_for(p,"[allow/deny] >",60); send(p,"allow")
time.sleep(2)  # sleep is now in flight
t0=time.time(); print(">>> SIGTERM"); p.send_signal(signal.SIGTERM)
ok&=wait_for(p,"dispatch is restarting")
p.wait(90); dt=time.time()-t0
print(f"\n[dispatch exited after {dt:.1f}s]"); err=p.stderr.read(); print("STDERR tail:", err[-600:])
drained = dt >= 5
slept = any(os.path.exists(os.path.join(r,"slept.txt")) for r,_,_ in os.walk(tmp)); print("slept.txt exists:", slept)
p=start(); ok&=wait_for(p,"dispatch is back")
send(p,"What was the exact shell command you ran earlier? Reply with just the command.")
ok&=wait_for(p,"resuming session"); ok&=wait_for(p,"✅ done")

p.terminate(); p.wait(30)

# Workflow leg: SIGTERM mid-step. The restart grades the step's window — the
# turn never finished — and asks; the retry runs the step again.
#
# On its own database, so the step's prompt goes into a session with nothing
# left over in it. The leg above ends with a tool call a restart cut short,
# and a follow-up sent into that session is answering the leg above rather
# than starting this one.
_, wcfg = make_config(drain="2s")
print(">>> [workflow leg: fresh database]")
p=start(wcfg); ok&=wait_for(p,"type `help`")
# The sleep outlasts the 2s drain, so the step's turn is cut short with
# nothing in its window — the case the grading is for.
send(p,"workflow drill Using Bash run `sleep 30 && touch drilled.txt`, then reply exactly: DRILLED")
ok&=wait_for(p,"1/1 *work*",60)
ok&=wait_for(p,"[allow/deny] >",120); send(p,"allow")
time.sleep(3)  # the sleep is in flight, inside the step's turn
print(">>> SIGTERM (mid-workflow-step)"); p.send_signal(signal.SIGTERM)
ok&=wait_for(p,"dispatch is restarting")
p.wait(90); p=start(wcfg)
ok&=wait_for(p,"dispatch is back",120)
# The step is graded on what made it to its own window: no result landed, so
# it failed and is put to the thread rather than silently asked for again.
ok&=wait_for(p,"the turn did not finish",120)
send(p,"Retry")
ok&=wait_for(p,"1/1 *work*",60)
# The retry's own turn, allowing whatever it asks for on the way.
ok&=wait_allowing(p,"🏁 workflow *drill*",240)
p.terminate(); p.wait(30)
print("drained:", drained)
ok = ok and drained and slept
print("RESTART_OK" if ok else "RESTART_FAIL")
sys.exit(0 if ok else 1)
