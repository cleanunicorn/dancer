#!/usr/bin/env python3
"""Auto-resume drill against the real binary and claude (haiku).

    scripts/auto-resume-drill.py bin/dancer

Starts a task whose tool call sleeps 8s, sends SIGTERM while it runs, then
restarts dancer and checks the task carries on by itself — nothing is typed
into the thread after the restart.
"""
import os, select, signal, subprocess, sys, tempfile, time
bin_ = os.path.abspath(sys.argv[1])
tmp = tempfile.mkdtemp(prefix="dancer-autoresume-")
cfg = os.path.join(tmp, "config.toml")
open(cfg, "w").write(f"""[server]
db = "{tmp}/dancer.db"
workdir_root = "{tmp}/work"
idle_timeout = "60s"
drain_timeout = "60s"
transports = ["terminal"]
auto_resume = true

[[definitions]]
name = "coder"
model = "haiku"
permission_mode = "bypassPermissions"
allowed_tools = ["Read", "Write", "Bash"]
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
send(p,"run coder Run `sleep 8` with Bash, then write the file done.txt containing DONE, then reply exactly: FINISHED")
time.sleep(6)  # the sleep is in flight
print(">>> SIGTERM"); p.send_signal(signal.SIGTERM)
ok&=wait_for(p,"dancer is restarting")
p.wait(90); print("\n[dancer exited]"); print("STDERR tail:", p.stderr.read()[-600:])

# Restart and type NOTHING: the task has to finish on its own.
p=start()
ok&=wait_for(p,"picking up this task")
ok&=wait_for(p,"✅ done",180)
done = any(os.path.exists(os.path.join(r,"done.txt")) for r,_,_ in os.walk(tmp))
print("done.txt exists:", done)

# Phase 2: a turn that had finished before the stop must NOT be continued —
# its process was only being kept alive for a follow-up.
send(p,"Reply with exactly: HELLO")   # follow-up on the same live session
ok&=wait_for(p,"HELLO",120); ok&=wait_for(p,"✅ done",60)
print(">>> SIGTERM (agent idle, not mid-turn)"); p.send_signal(signal.SIGTERM)
ok&=wait_for(p,"dancer is restarting"); p.wait(90)
p=start(); ok&=wait_for(p,"type `help`")
time.sleep(5)
quiet = not wait_for(p,"picking up this task",timeout=5)
print("idle task left alone:", quiet)
p.terminate(); p.wait(30)
ok = ok and done and quiet
print("AUTO_RESUME_OK" if ok else "AUTO_RESUME_FAIL")
sys.exit(0 if ok else 1)
