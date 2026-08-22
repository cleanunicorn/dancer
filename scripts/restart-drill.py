#!/usr/bin/env python3
"""Graceful-restart drill against the real binary and claude (haiku).

    scripts/restart-drill.py bin/dancer

Starts a task whose tool call sleeps 8s, sends SIGTERM while it runs, checks
dancer drains the call and posts the restart notice, then restarts and checks
the "back" notice and that the resumed session remembers the command.
"""
import os, select, signal, subprocess, sys, tempfile, time
bin_ = os.path.abspath(sys.argv[1])
tmp = tempfile.mkdtemp(prefix="dancer-restart-")
cfg = os.path.join(tmp, "config.toml")
open(cfg, "w").write(f"""[server]
db = "{tmp}/dancer.db"
workdir_root = "{tmp}/work"
idle_timeout = "60s"
drain_timeout = "60s"
transports = ["terminal"]

[[definitions]]
name = "coder"
model = "haiku"
permission_mode = "manual"
allowed_tools = ["Read"]
""")
def start():
    p = subprocess.Popen([bin_, "run", "-config", cfg, "-terminal"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1)
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
ok=True
p=start(); ok&=wait_for(p,"type `help`")
send(p,"run coder Using Bash run `sleep 8 && touch slept.txt`, then reply exactly: SLEPT")
ok&=wait_for(p,"[allow/deny] >",60); send(p,"allow")
time.sleep(2)  # sleep is now in flight
t0=time.time(); print(">>> SIGTERM"); p.send_signal(signal.SIGTERM)
ok&=wait_for(p,"dancer is restarting")
p.wait(90); dt=time.time()-t0
print(f"\n[dancer exited after {dt:.1f}s]"); err=p.stderr.read(); print("STDERR tail:", err[-600:])
drained = dt >= 5
slept = any(os.path.exists(os.path.join(r,"slept.txt")) for r,_,_ in os.walk(tmp)); print("slept.txt exists:", slept)
p=start(); ok&=wait_for(p,"dancer is back")
send(p,"What was the exact shell command you ran earlier? Reply with just the command.")
ok&=wait_for(p,"resuming session"); ok&=wait_for(p,"✅ done")
p.terminate(); p.wait(30)
print("drained:", drained)
ok = ok and drained and slept
print("RESTART_OK" if ok else "RESTART_FAIL")
sys.exit(0 if ok else 1)
