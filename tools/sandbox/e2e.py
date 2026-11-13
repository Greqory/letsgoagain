#!/usr/bin/env python3
"""End-to-end check of letsgoagain against a real OpenCode and tools/fakemeridian (AGENTS.md §6).

Never touches the user's OpenCode: its own HOME and XDG dirs under /tmp/opencode/lga-sandbox, its own
ports. Builds the binaries, starts fakemeridian, `opencode serve` and the daemon, then checks:
  1. live: a limit stop is recorded with the quota's reset and resumed when it comes;
  2. missed: a stop that happened while the daemon was off is found by the scan and `resume-now` resumes it.
Takes about 5 minutes (OpenCode retries a limit for ~80 s before it gives up).
Usage: python3 -I tools/sandbox/e2e.py
"""
import base64, json, os, re, shutil, subprocess, sys, time, urllib.request

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SB = "/tmp/opencode/lga-sandbox"
OC_PORT, FAKE_PORT = 47811, 47812
BASE, FAKE = f"http://127.0.0.1:{OC_PORT}", f"http://127.0.0.1:{FAKE_PORT}"
procs, t0 = [], time.time()


def say(msg):
    print(f"{time.time() - t0:6.1f}s {msg}", flush=True)


def fail(msg):
    say("FAIL: " + msg)
    sys.exit(1)


def start(args, log, env=None):
    p = subprocess.Popen(args, stdout=open(log, "a"), stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
                         env=env, cwd=f"{SB}/work", start_new_session=True)
    procs.append(p)
    return p


def call(method, url, body=None, auth=None):
    req = urllib.request.Request(url, method=method, data=None if body is None else json.dumps(body).encode())
    req.add_header("Content-Type", "application/json")
    if auth:
        req.add_header("Authorization", auth)
    with urllib.request.urlopen(req, timeout=30) as r:
        raw = r.read()
        return json.loads(raw) if raw else None


def wait(cond, timeout, what):
    end = time.time() + timeout
    while time.time() < end:
        if cond():
            return
        time.sleep(1)
    fail(f"timed out waiting for {what}")


def main():
    shutil.rmtree(SB, ignore_errors=True)
    for d in ("home", "config/opencode", "data", "state", "cache", "work", "lga/state/opencode", "lga/run"):
        os.makedirs(f"{SB}/{d}", mode=0o700, exist_ok=True)
    for name, pkg in (("letsgoagain", "./cmd/letsgoagain"), ("fakemeridian", "./tools/fakemeridian")):
        subprocess.run(["go", "build", "-o", f"{SB}/{name}", pkg], cwd=REPO, check=True)
    json.dump({"$schema": "https://opencode.ai/config.json", "model": "anthropic/claude-sonnet-5-5",
               "small_model": "anthropic/claude-sonnet-5-5", "autoupdate": False,
               "provider": {"anthropic": {"options": {"baseURL": FAKE, "apiKey": "dummy"}}}},
              open(f"{SB}/config/opencode/opencode.json", "w"))

    start([f"{SB}/fakemeridian", "-addr", f"127.0.0.1:{FAKE_PORT}"], f"{SB}/fake.log")
    oc_env = dict(os.environ, HOME=f"{SB}/home", XDG_CONFIG_HOME=f"{SB}/config", XDG_DATA_HOME=f"{SB}/data",
                  XDG_STATE_HOME=f"{SB}/state", XDG_CACHE_HOME=f"{SB}/cache")
    start(["opencode", "serve", "--hostname", "127.0.0.1", "--port", str(OC_PORT)], f"{SB}/serve.log", oc_env)
    wait(lambda: "server password" in open(f"{SB}/serve.log").read(), 60, "opencode serve")
    pw = re.search(r"server password (\S+)", open(f"{SB}/serve.log").read()).group(1)
    auth = "Basic " + base64.b64encode(f"opencode:{pw}".encode()).decode()
    svc = f"{SB}/lga/state/opencode/service.json"
    with open(os.open(svc, os.O_WRONLY | os.O_CREAT, 0o600), "w") as f:
        json.dump({"id": "sandbox", "url": BASE, "pid": 0, "password": pw}, f)
    say("opencode " + subprocess.run(["opencode", "--version"], capture_output=True, text=True).stdout.strip() + " serving")

    lga_env = dict(os.environ, XDG_STATE_HOME=f"{SB}/lga/state", XDG_RUNTIME_DIR=f"{SB}/lga/run")
    daemon_log = f"{SB}/lga/daemon.log"
    log = lambda: open(daemon_log).read()

    def lga(*args):
        return subprocess.run([f"{SB}/letsgoagain", *args], env=lga_env, capture_output=True, text=True)

    def daemon():
        return start([f"{SB}/letsgoagain", "daemon", "--meridian", FAKE], daemon_log, lga_env)

    def limited_session(title):
        call("POST", FAKE + "/fake/limit", {"limited": True, "retry_after": 4868})
        sid = call("POST", BASE + "/api/session", {"title": title}, auth)["data"]["id"]
        call("POST", BASE + f"/api/session/{sid}/prompt", {"text": "do the work"}, auth)
        return sid

    def outcome(sid):
        return call("GET", BASE + f"/api/session/{sid}", auth=auth)["data"].get("outcome")

    # 1. live
    d = daemon()
    wait(lambda: "connected to the OpenCode service" in log(), 30, "daemon connect")
    reset_ms = int((time.time() + 120) * 1000)
    call("POST", FAKE + "/fake/buckets", [{"type": "five_hour", "status": "rejected", "utilization": 1,
                                           "resetsAt": reset_ms, "observedAt": int(time.time() * 1000)}])
    sid = limited_session("e2e live")
    say(f"live: {sid} limited, quota resets in 120 s")
    wait(lambda: "limit stop recorded, reset from the meridian quota (five_hour)" in log(), 150, "stop recorded")
    say("live: stop recorded from the quota")
    call("POST", FAKE + "/fake/limit", {"limited": False})
    wait(lambda: "resume sent (reset reached" in log(), 240, "resume")
    if time.time() * 1000 < reset_ms:
        fail("resumed before the reset")
    wait(lambda: outcome(sid) == "succeeded" and "running again, stop done" in log(), 60, "success")
    say("live: resumed after the reset, session succeeded")

    # 2. missed while the daemon was off
    d.terminate()
    d.wait()
    call("POST", FAKE + "/fake/buckets", [])
    sid = limited_session("e2e missed")
    wait(lambda: outcome(sid) == "failed", 150, "opencode giving up")
    call("POST", FAKE + "/fake/limit", {"limited": False})
    say(f"missed: {sid} stopped while the daemon was off")
    daemon()
    wait(lambda: log().count("limit stop recorded, reset from retry_after") == 1, 30, "scan")
    st = lga("status")
    if "waiting" not in st.stdout or sid not in st.stdout:
        fail("status:\n" + st.stdout + st.stderr)
    r = lga("resume-now", sid)
    if r.returncode != 0 or not r.stdout.strip().endswith(": resumed"):
        fail("resume-now:\n" + r.stdout + r.stderr)
    wait(lambda: outcome(sid) == "succeeded", 60, "success")
    say("missed: found by the scan, resume-now resumed it, session succeeded")
    print("\n--- daemon log\n" + log())
    say("PASS")


if __name__ == "__main__":
    try:
        main()
    finally:
        for p in procs:
            if p.poll() is None:
                p.terminate()
        for p in procs:
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()
