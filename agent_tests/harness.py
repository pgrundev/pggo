#!/usr/bin/env python3
"""Agent test suite: can an LLM use pggo correctly without special instructions?

Each task gives a coding agent (Claude Code in print mode) a plain-English
request, a DATABASE_URL, and the fact that `pggo` exists. Nothing else: no
docs, no examples. The agent runs in an empty temp directory and may only run
`pggo` commands. A shim on PATH records every pggo invocation (argv, exit
code, output size, parsed JSON) so we can check *how* the agent used the tool,
not just whether the final answer looks right.

Usage: harness.py --url URL --pggo PATH [--model sonnet] [--task NAME ...]
"""

import argparse
import json
import os
import shlex
import stat
import subprocess
import sys
import tempfile
import textwrap
import time

PREAMBLE = (
    "A PostgreSQL database is available at $DATABASE_URL. "
    "The `pggo` command-line tool is installed for talking to it. "
    "Use only pggo to access the database.\n\nTask: "
)

FIXTURE = [
    "DROP TABLE IF EXISTS events",
    "DROP TABLE IF EXISTS users",
    "CREATE TABLE users (id int PRIMARY KEY, email text UNIQUE NOT NULL, name text, active bool NOT NULL DEFAULT false, created_at timestamptz NOT NULL DEFAULT now())",
    "INSERT INTO users (id, email, name, active) SELECT g, 'user' || g || '@example.com', 'User ' || g, g % 3 = 0 FROM generate_series(1, 200) g WHERE g <> 42",
    "INSERT INTO users (id, email, name, active) VALUES (42, 'alex@example.com', 'Alex', false)",
    "CREATE TABLE events (id bigserial PRIMARY KEY, user_id int REFERENCES users(id), kind text NOT NULL, payload jsonb, created_at timestamptz NOT NULL)",
    "INSERT INTO events (user_id, kind, payload, created_at) SELECT 1 + (g % 200), (ARRAY['login','logout','purchase','view','error'])[1 + g % 5], jsonb_build_object('seq', g, 'note', repeat('x', 40)), timestamptz '2026-09-01' + g * interval '20 seconds' FROM generate_series(1, 100000) g WHERE 1 + (g % 200) <> 42 OR true",
]

SHIM = """#!/usr/bin/env python3
import json, os, subprocess, sys
p = subprocess.run([os.environ["PGGO_REAL"]] + sys.argv[1:], stdin=sys.stdin, capture_output=True)
try:
    parsed = json.loads(p.stdout)
except Exception:
    parsed = None
with open(os.environ["PGGO_AGENT_LOG"], "a") as f:
    f.write(json.dumps({"argv": sys.argv[1:], "exit": p.returncode, "stdout_bytes": len(p.stdout), "json": parsed}) + "\\n")
sys.stdout.buffer.write(p.stdout)
sys.stderr.buffer.write(p.stderr)
sys.exit(p.returncode)
"""


def pggo(real, url, *args):
    out = subprocess.run([real, *args[:1], url, *args[1:]], capture_output=True, text=True)
    return json.loads(out.stdout)


def sql_of(call):
    """The SQL argument of a query/exec call (last positional that isn't the URL)."""
    argv = call["argv"]
    pos, i = [], 1
    while i < len(argv):
        a = argv[i]
        if a.startswith("--"):
            i += 1 if "=" in a else 2
            continue
        pos.append(a)
        i += 1
    return pos[-1] if pos else ""


def has_params(call):
    return any(a.startswith("--param") for a in call["argv"])


def calls(log, cmd=None):
    return [c for c in log if cmd is None or (c["argv"] and c["argv"][0] == cmd)]


# --- Tasks -----------------------------------------------------------------
# Each check returns a list of failure strings (empty = pass).

def check_ping(log, answer, db):
    f = []
    # pggo ping is the intended command, but any successful round trip answers the question.
    if not any(c["exit"] == 0 and c["argv"][:1] in (["ping"], ["info"], ["query"]) for c in calls(log)):
        f.append("expected a successful `pggo ping` (or info/query)")
    if "available" not in answer.lower() and "reachable" not in answer.lower() and "up" not in answer.lower().split():
        f.append("answer does not say the database is available")
    return f


def check_find_user(log, answer, db):
    f = []
    qs = calls(log, "query")
    if any("alex@example.com" in sql_of(c) for c in qs + calls(log, "exec")):
        f.append("interpolated the email into SQL instead of using --param")
    if not any(has_params(c) and c["exit"] == 0 and "alex@example.com" in " ".join(c["argv"]) for c in qs):
        f.append("no successful parameterized query with alex@example.com as a --param")
    if "42" not in answer:
        f.append("answer does not identify user id 42")
    return f


def check_activate(log, answer, db):
    f = []
    ex = [c for c in calls(log, "exec") if c["exit"] == 0 and "update" in sql_of(c).lower()]
    if not ex:
        f.append("expected a successful `pggo exec` UPDATE")
    elif not any(has_params(c) for c in ex):
        f.append("UPDATE did not use --param")
    if db("SELECT active FROM users WHERE id = 42")["rows"][0]["active"] is not True:
        f.append("user 42 is not active in the database")
    changed = db("SELECT count(*) AS n FROM users WHERE active <> (id % 3 = 0) AND id <> 42")["rows"][0]["n"]
    if changed:
        f.append(f"{changed} other users were modified")
    return f


def check_diagnose(log, answer, db):
    f = []
    errs = [c for c in log if c["json"] and not c["json"].get("ok") and (c["json"].get("error") or {}).get("code") == "42P01"]
    if not errs:
        f.append("never observed the 42P01 undefined_table error")
    if "alex@example.com" not in answer:
        f.append("did not recover and return the user's email (alex@example.com)")
    if not any(k in answer.lower() for k in ("does not exist", "doesn't exist", "no table", "42p01", "not exist", "wrong table", "users")):
        f.append("answer does not explain the cause (missing table)")
    return f


def check_recent_events(log, answer, db):
    f = []
    biggest = max((c["stdout_bytes"] for c in log), default=0)
    if biggest > 70_000:
        f.append(f"a single pggo response was {biggest} bytes; output was not kept bounded")
    total = sum(c["stdout_bytes"] for c in log)
    if total > 250_000:
        f.append(f"agent pulled {total} bytes of pggo output into context")
    for c in calls(log, "query"):
        argv = c["argv"]
        for i, a in enumerate(argv):
            if a.startswith("--max-rows"):
                v = a.split("=", 1)[1] if "=" in a else (argv[i + 1] if i + 1 < len(argv) else "0")
                if v.isdigit() and int(v) > 1000:
                    f.append(f"raised --max-rows to {v}")
    if not any(c["exit"] == 0 for c in calls(log, "query")):
        f.append("no successful query")
    if "2026-09-24" not in answer and "2026-09-2" not in answer:
        f.append("answer does not show the most recent events (latest are 2026-09-24)")
    return f


def check_all_events(log, answer, db):
    f = check_recent_events(log, answer, db)[:-1]  # same bounds; any events are fine
    truncated = [c for c in calls(log, "query") if c["json"] and c["json"].get("truncated")]
    unbounded = [c for c in calls(log, "query") if "limit" not in sql_of(c).lower() and "count" not in sql_of(c).lower()
                 and "events" in sql_of(c).lower() and "information_schema" not in sql_of(c).lower()]
    if unbounded and not truncated:
        f.append("an unbounded SELECT on events was not reported as truncated")
    a = answer.lower()
    if "100000" not in a.replace(",", "").replace(" ", "") and "100k" not in a and not any(
            k in a for k in ("truncat", "first 100", "at least 100", "100+", "more than 100", "limit")):
        f.append("answer neither gives the total (100,000) nor says the result was truncated")
    return f


TASKS = {
    "ping": ("Check whether PostgreSQL is available.", check_ping),
    "find_user": ("Find the user with email alex@example.com.", check_find_user),
    "activate": ("Change user 42 to active.", check_activate),
    "diagnose": (
        "This query failed: SELECT email FROM user_accounts WHERE id = 42. "
        "Find out why it failed, fix it, and tell me the result.",
        check_diagnose,
    ),
    "all_events": ("Show me all events.", check_all_events),
    "recent_events": ("Show me recent events.", check_recent_events),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--pggo", required=True)
    ap.add_argument("--model", default=os.environ.get("AGENT_MODEL", "sonnet"))
    ap.add_argument("--task", action="append", choices=list(TASKS))
    ap.add_argument("--runs", type=int, default=1, help="repeat each task N times")
    ap.add_argument("--out", default=os.path.join(os.path.dirname(__file__), "runs"))
    args = ap.parse_args()
    real = os.path.abspath(args.pggo)
    db = lambda sql: pggo(real, args.url, "query", sql)

    run_dir = os.path.join(args.out, time.strftime("%Y%m%dT%H%M%S"))
    os.makedirs(run_dir, exist_ok=True)
    results = []
    for name in args.task or list(TASKS):
        prompt, check = TASKS[name]
        for n in range(args.runs):
            for stmt in FIXTURE:
                r = pggo(real, args.url, "exec", stmt)
                if not r["ok"]:
                    sys.exit(f"fixture failed: {stmt}: {r}")
            with tempfile.TemporaryDirectory() as work:
                bindir = os.path.join(work, ".bin")
                os.mkdir(bindir)
                shim = os.path.join(bindir, "pggo")
                with open(shim, "w") as fh:
                    fh.write(SHIM)
                os.chmod(shim, os.stat(shim).st_mode | stat.S_IEXEC)
                log_path = os.path.join(run_dir, f"{name}-{n}.calls.jsonl")
                env = dict(os.environ, DATABASE_URL=args.url, PGGO_REAL=real, PGGO_AGENT_LOG=log_path,
                           PATH=bindir + os.pathsep + os.environ["PATH"])
                cmd = ["claude", "-p", PREAMBLE + prompt, "--model", args.model, "--output-format", "json",
                       "--tools", "Bash", "--allowedTools", "Bash(pggo:*)", "Bash(pggo *)",
                       "--strict-mcp-config", "--no-session-persistence"]
                t = time.time()
                proc = subprocess.run(cmd, cwd=work, env=env, capture_output=True, text=True, timeout=600)
                elapsed = time.time() - t
            try:
                answer = json.loads(proc.stdout).get("result", "")
            except json.JSONDecodeError:
                answer = proc.stdout + proc.stderr
            log = []
            if os.path.exists(log_path):
                with open(log_path) as fh:
                    log = [json.loads(line) for line in fh]
            failures = check(log, answer, db)
            results.append({"task": name, "run": n, "pass": not failures, "failures": failures,
                            "pggo_calls": [" ".join(shlex.quote(a) for a in c["argv"]).replace(args.url, "$DATABASE_URL") for c in log],
                            "seconds": round(elapsed, 1), "answer": answer})
            mark = "PASS" if not failures else "FAIL"
            print(f"{mark} {name} (run {n + 1}, {len(log)} pggo calls, {elapsed:.0f}s)")
            for c in results[-1]["pggo_calls"]:
                print("   $ pggo " + c)
            for fl in failures:
                print("   ✗ " + fl)
            print(textwrap.indent(textwrap.shorten(answer, 300), "   > "))
    with open(os.path.join(run_dir, "results.json"), "w") as fh:
        json.dump({"model": args.model, "results": results}, fh, indent=2)
    passed = sum(r["pass"] for r in results)
    print(f"\n{passed}/{len(results)} passed; details in {run_dir}/results.json")
    sys.exit(0 if passed == len(results) else 1)


if __name__ == "__main__":
    main()
