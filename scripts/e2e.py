#!/usr/bin/env python3
"""Drive the dispatch binary end to end through the terminal transport.

    scripts/e2e.py bin/dispatch [model]

Starts dispatch with a throwaway config, runs a task that needs a permission,
allows it, checks the result, status, a follow-up turn, and closing and
reopening the thread. Costs a few cents (haiku by default). Requires a
logged-in `claude`.
"""
import os, select, subprocess, sys, tempfile, time

bin_ = os.path.abspath(sys.argv[1])
model = sys.argv[2] if len(sys.argv) > 2 else "haiku"
tmp = tempfile.mkdtemp(prefix="dispatch-e2e-")
cfg = os.path.join(tmp, "config.toml")
with open(cfg, "w") as f:
    f.write(f'''[server]
db = "{tmp}/dispatch.db"
workdir_root = "{tmp}/work"
idle_timeout = "20s"
transports = ["terminal"]

[[definitions]]
name = "coder"
model = "{model}"
permission_mode = "manual"
allowed_tools = ["Read"]
''')

p = subprocess.Popen([bin_, "run", "-config", cfg, "-terminal"], stdin=subprocess.PIPE,
                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1)
os.set_blocking(p.stdout.fileno(), False)
buf = ""   # output not yet matched
seen = ""  # everything dispatch printed

def wait_for(pat, timeout=180):
    global buf, seen
    end = time.time() + timeout
    while time.time() < end:
        r, _, _ = select.select([p.stdout], [], [], 0.2)
        if r:
            chunk = p.stdout.read()
            if chunk:
                buf += chunk
                seen += chunk
                sys.stdout.write(chunk)
                sys.stdout.flush()
        if pat in buf:
            buf = buf[buf.index(pat) + len(pat):]
            return True
        if p.poll() is not None:
            break
    print(f"\n!! timed out waiting for {pat!r}")
    return False

def send(s):
    print(f">>> {s}")
    p.stdin.write(s + "\n")
    p.stdin.flush()

def done():
    """The closing line — and, on a subscription login, the usage meter
    posted right after it, so terminating here does not cut it off."""
    if not wait_for("✅ done"):
        return False
    return wait_for("📊", 10) if "· subscription ·" in seen else True

ok = wait_for("type `help`")
send("agents");                                   ok &= wait_for("coder")
send("run coder Using Bash, run `touch e2e.txt` then reply with exactly: DONE")
ok &= wait_for("[allow/deny] >")
send("allow");                                    ok &= done()
send("status");                                   ok &= wait_for("status *idle*")
send("Reply with exactly: SECOND");               ok &= done()
send("close");                                    ok &= wait_for("thread closed")
send("close");                                    ok &= wait_for("already closed")
send("Reply with exactly: THIRD");                ok &= wait_for("thread reopened")
ok &= done()
p.terminate()
try:
    p.wait(10)
except Exception:
    p.kill()
created = any(os.path.exists(os.path.join(r, "e2e.txt")) for r, _, _ in os.walk(os.path.join(tmp, "work")))
print("\nSTDERR tail:\n" + p.stderr.read()[-800:])
print("e2e.txt created:", created)
print("E2E_OK" if ok and created else "E2E_FAIL")
sys.exit(0 if ok and created else 1)
