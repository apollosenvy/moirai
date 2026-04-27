#!/usr/bin/env bash
# Smoke test for the Reviewer-Orchestrator (RO) rewrite.
#
# This does NOT exercise real LLMs. It starts three tiny Python HTTP servers
# that mimic llama-server's /v1/chat/completions endpoint with canned
# responses, then runs the full daemon against them.
#
# What it verifies:
#   - The daemon boots
#   - /submit accepts a task
#   - The RO loop reaches ask_planner, ask_coder, fs.write, test.run, done
#   - The trace records ro_tool_call, p_reply, c_reply events
#
# What it does NOT verify:
#   - That any real model produces sensible output
#   - That models actually load in VRAM
#
# Run from repo root:
#   ./smoke-test-ro.sh

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="${REPO_DIR}/bin/moirai"
PORT="${AGENT_ROUTER_PORT:-5987}"

# Stub LLM ports. AR_SMOKE_PORT_BASE chooses a fixed contiguous block; if
# unset (or set to 0), pick three free ports dynamically via the OS so two
# concurrent smoke runs don't collide -- the pass-3 audit caught a leftover
# stub from a previous run binding the hardcoded port and silently
# misrouting the reviewer LLM calls.
pick_three_free_ports() {
    python3 - <<'PY'
import socket
ports = []
socks = []
for _ in range(3):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    socks.append(s)
    ports.append(s.getsockname()[1])
# Close after collecting all three so the OS hands us distinct ports.
for s in socks:
    s.close()
print(" ".join(str(p) for p in ports))
PY
}
if [ "${AR_SMOKE_PORT_BASE:-0}" -gt 0 ]; then
    PLANNER_PORT=$((AR_SMOKE_PORT_BASE + 0))
    CODER_PORT=$((AR_SMOKE_PORT_BASE + 1))
    REVIEWER_PORT=$((AR_SMOKE_PORT_BASE + 2))
else
    read -r PLANNER_PORT CODER_PORT REVIEWER_PORT <<<"$(pick_three_free_ports)"
fi
echo "[smoke-ro] stub ports: planner=$PLANNER_PORT coder=$CODER_PORT reviewer=$REVIEWER_PORT"
TEST_REPO="$(mktemp -d /tmp/agent-router-ro-smoke.XXXXXX)"
CONFIG="$(mktemp /tmp/agent-router-ro-smoke-config.XXXXXX.json)"
STUB_LOG="/tmp/agent-router-ro-stub.log"
DAEMON_LOG="/tmp/agent-router-ro-daemon.log"
STUB_SCRIPT="$(mktemp /tmp/agent-router-ro-stub.XXXXXX.py)"

# We fake llama-server by pointing llama_server_bin at our stub-launcher
# shell script. The launcher takes the same CLI args (--model, --port, --host)
# and starts the Python stub listening on the given port.
STUB_LAUNCHER="$(mktemp /tmp/agent-router-ro-launcher.XXXXXX.sh)"

cleanup() {
    if [ -n "${DAEMON_PID:-}" ]; then
        kill -INT "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
    fi
    # Any stub servers the daemon spawned will be in the same process group
    # as the daemon and should have been reaped by the SIGINT.
    pkill -f "$STUB_SCRIPT" 2>/dev/null || true
    rm -rf "$TEST_REPO" "$CONFIG" "$STUB_SCRIPT" "$STUB_LAUNCHER" 2>/dev/null || true
}
trap cleanup EXIT

# -----------------------------------------------------------------------------
# Build the daemon
# -----------------------------------------------------------------------------
echo "[smoke-ro] building moirai..."
cd "$REPO_DIR"
mkdir -p bin
go build -o "$BIN" ./cmd/moirai

# -----------------------------------------------------------------------------
# Write the Python stub (llama-server mock)
# -----------------------------------------------------------------------------
cat > "$STUB_SCRIPT" <<'PYEOF'
#!/usr/bin/env python3
"""
Tiny llama-server stand-in for the agent-router RO smoke test.

Accepts --port N and a --role flag via env var AR_STUB_ROLE
(planner, coder, reviewer). Serves:
  GET  /v1/models                    -> empty model list (readiness probe)
  POST /v1/chat/completions          -> canned response keyed off role and
                                        a per-role turn counter

Responses are scripted to drive the RO loop through:
  ask_planner -> ask_coder -> fs.write -> test.run -> done
"""
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

ROLE = os.environ.get("AR_STUB_ROLE", "reviewer")
TURN_LOCK = threading.Lock()
TURN = {"count": 0}

# Reviewer (orchestrator) scripted turns. Each entry is the assistant
# content returned from /v1/chat/completions; the daemon will parse the
# <TOOL>...</TOOL> block and dispatch the named tool.
REVIEWER_SCRIPT = [
    # Turn 1: ask the planner
    'I will start by asking the planner for a concrete plan.\n'
    '<TOOL>{"name":"ask_planner","args":{"instruction":"create hello.py that prints hi"}}</TOOL>',
    # Turn 2: ask the coder with the plan returned
    'The plan looks good. Now get the coder to write the file.\n'
    '<TOOL>{"name":"ask_coder","args":{"instruction":"implement per plan","plan":"1. Write hello.py that prints hi"}}</TOOL>',
    # Turn 3: write the file the coder produced
    'Commit hello.py to disk.\n'
    '<TOOL>{"name":"fs.write","args":{"path":"hello.py","content":"print(\\"hi\\")\\n"}}</TOOL>',
    # Turn 4: run the configured tests
    'Run tests to confirm.\n'
    '<TOOL>{"name":"test.run","args":{}}</TOOL>',
    # Turn 5: tests passed, declare done
    'Tests passed. Task complete.\n'
    '<TOOL>{"name":"done","args":{"summary":"hello.py written; smoke test ok"}}</TOOL>',
]

PLANNER_REPLY = (
    "Plan:\n"
    "1. Create hello.py in the repo root.\n"
    "2. The file should contain a single print statement that prints hi.\n"
    "3. No tests required beyond the configured test command.\n"
)

CODER_REPLY = (
    "Here is the file:\n"
    "```python\n"
    "# file: hello.py\n"
    "print(\"hi\")\n"
    "```\n"
)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        # Quiet; logs go to stderr which is captured.
        sys.stderr.write("[%s stub] " % ROLE + (fmt % args) + "\n")

    def _json(self, code, body):
        data = json.dumps(body).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path.startswith("/v1/models"):
            self._json(200, {"object": "list", "data": []})
            return
        if self.path == "/health":
            self._json(200, {"ok": True})
            return
        self._json(404, {"error": "not found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        _ = self.rfile.read(length)  # we don't care about the request body
        if not self.path.startswith("/v1/chat/completions"):
            self._json(404, {"error": "not found"})
            return

        with TURN_LOCK:
            TURN["count"] += 1
            n = TURN["count"]

        if ROLE == "reviewer":
            idx = min(n - 1, len(REVIEWER_SCRIPT) - 1)
            content = REVIEWER_SCRIPT[idx]
        elif ROLE == "planner":
            content = PLANNER_REPLY
        elif ROLE == "coder":
            content = CODER_REPLY
        else:
            content = "(unknown role)"

        self._json(200, {
            "id": "stub-1",
            "object": "chat.completion",
            "model": ROLE,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": content},
                "finish_reason": "stop",
            }],
        })


def main():
    port = None
    host = "127.0.0.1"
    args = sys.argv[1:]
    i = 0
    while i < len(args):
        a = args[i]
        if a in ("--port", "-p"):
            port = int(args[i + 1]); i += 2; continue
        if a in ("--host",):
            host = args[i + 1]; i += 2; continue
        i += 1
    if port is None:
        print("stub: --port required", file=sys.stderr)
        sys.exit(2)
    srv = HTTPServer((host, port), Handler)
    sys.stderr.write("[%s stub] listening on %s:%d\n" % (ROLE, host, port))
    srv.serve_forever()


if __name__ == "__main__":
    main()
PYEOF
chmod +x "$STUB_SCRIPT"

# -----------------------------------------------------------------------------
# Write the launcher. The daemon spawns this with the same args as the real
# llama-server (--model PATH --port N --host H -c CTX -ngl N ...); we parse
# --port and --model, pick a role based on which model path the daemon asked
# for, and exec the Python stub.
# -----------------------------------------------------------------------------
cat > "$STUB_LAUNCHER" <<LAUNCHEREOF
#!/usr/bin/env bash
set -e
PORT=""
MODEL=""
HOST="127.0.0.1"
while [ \$# -gt 0 ]; do
    case "\$1" in
        --port) PORT="\$2"; shift 2;;
        -p) PORT="\$2"; shift 2;;
        --host) HOST="\$2"; shift 2;;
        --model) MODEL="\$2"; shift 2;;
        -m) MODEL="\$2"; shift 2;;
        *) shift;;
    esac
done
# Role key off the model path.
case "\$MODEL" in
    *planner*|*Planner*) ROLE="planner";;
    *coder*|*Coder*) ROLE="coder";;
    *reviewer*|*Reviewer*) ROLE="reviewer";;
    *) ROLE="reviewer";;
esac
export AR_STUB_ROLE="\$ROLE"
exec python3 "$STUB_SCRIPT" --port "\$PORT" --host "\$HOST"
LAUNCHEREOF
chmod +x "$STUB_LAUNCHER"

# -----------------------------------------------------------------------------
# Test repo
# -----------------------------------------------------------------------------
echo "[smoke-ro] preparing test repo at $TEST_REPO"
cd "$TEST_REPO"
git init -q
git config user.email "ro-smoke@agent-router.local"
git config user.name "ro-smoke"
cat > .agent-router.toml <<'TOML'
[commands]
test = "echo noop-test-ok && exit 0"
compile = "true"
lint = "true"

[style]
language = "python"

[budget]
max_runtime = "5m"
max_iterations = 10
TOML
git add -A
git commit -q -m "smoke test seed"

# -----------------------------------------------------------------------------
# Daemon config
# -----------------------------------------------------------------------------
# Path placeholders that encode role names so the launcher can pick ROLE.
PLANNER_FAKE="/tmp/agent-router-ro-smoke-planner.gguf"
CODER_FAKE="/tmp/agent-router-ro-smoke-coder.gguf"
REVIEWER_FAKE="/tmp/agent-router-ro-smoke-reviewer.gguf"
: > "$PLANNER_FAKE"
: > "$CODER_FAKE"
: > "$REVIEWER_FAKE"

cat > "$CONFIG" <<JSON
{
  "port": $PORT,
  "llama_server_bin": "$STUB_LAUNCHER",
  "default_repo": "$TEST_REPO",
  "max_coder_retries": 5,
  "max_replans": 3,
  "boot_timeout_seconds": 30,
  "models": {
    "planner":  {"slot":"planner","model_path":"$PLANNER_FAKE","ctx_size":2048,"n_gpu_layers":0,"port":$PLANNER_PORT},
    "coder":    {"slot":"coder","model_path":"$CODER_FAKE","ctx_size":2048,"n_gpu_layers":0,"port":$CODER_PORT},
    "reviewer": {"slot":"reviewer","model_path":"$REVIEWER_FAKE","ctx_size":2048,"n_gpu_layers":0,"port":$REVIEWER_PORT}
  }
}
JSON

# -----------------------------------------------------------------------------
# Daemon
# -----------------------------------------------------------------------------
echo "[smoke-ro] starting daemon on :$PORT"
"$BIN" daemon --config "$CONFIG" > "$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

echo "[smoke-ro] waiting for daemon /health"
for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
        echo "[smoke-ro] daemon alive (pid=$DAEMON_PID)"
        break
    fi
    sleep 0.5
done
if ! curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    echo "[smoke-ro] FAIL: daemon did not come up"
    tail -40 "$DAEMON_LOG"
    exit 1
fi

echo "[smoke-ro] submitting task"
RESP=$(curl -sf -X POST "http://127.0.0.1:$PORT/submit" \
    -H "Content-Type: application/json" \
    -d "{\"description\":\"create hello.py that prints hi\",\"repo_root\":\"$TEST_REPO\"}")
echo "$RESP"
TASK_ID=$(echo "$RESP" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')
if [ -z "$TASK_ID" ]; then
    echo "[smoke-ro] FAIL: could not extract task id"
    exit 1
fi
echo "[smoke-ro] task id: $TASK_ID"

TRACE_PATH="$HOME/.local/share/agent-router/traces/$TASK_ID.jsonl"

echo "[smoke-ro] polling for terminal status (up to 60s)"
STATUS="pending"
for i in $(seq 1 60); do
    RAW=$(curl -sf "http://127.0.0.1:$PORT/tasks/$TASK_ID" || true)
    STATUS=$(echo "$RAW" | sed -n 's/.*"status": *"\([^"]*\)".*/\1/p' | head -1)
    case "$STATUS" in
        succeeded|failed|aborted) break ;;
    esac
    sleep 1
done
echo "[smoke-ro] final status: $STATUS"

# -----------------------------------------------------------------------------
# Assertions
# -----------------------------------------------------------------------------
PASS=0
FAIL=0
assert_contains() {
    local needle="$1"; local file="$2"
    if grep -q "$needle" "$file"; then
        echo "[smoke-ro]   PASS: trace contains '$needle'"
        PASS=$((PASS + 1))
    else
        echo "[smoke-ro]   FAIL: trace missing '$needle'"
        FAIL=$((FAIL + 1))
    fi
}

echo "[smoke-ro] checking trace $TRACE_PATH"
if [ ! -f "$TRACE_PATH" ]; then
    echo "[smoke-ro] FAIL: trace file missing"
    exit 1
fi

assert_contains "ro_tool_call" "$TRACE_PATH"
assert_contains "ask_planner" "$TRACE_PATH"
assert_contains "ask_coder" "$TRACE_PATH"
assert_contains "fs.write" "$TRACE_PATH"
assert_contains "test.run" "$TRACE_PATH"
assert_contains '"name":"done"' "$TRACE_PATH"
assert_contains "p_reply" "$TRACE_PATH"
assert_contains "c_reply" "$TRACE_PATH"

if [ -f "$TEST_REPO/hello.py" ]; then
    echo "[smoke-ro]   PASS: hello.py exists"
    PASS=$((PASS + 1))
else
    echo "[smoke-ro]   FAIL: hello.py not written"
    FAIL=$((FAIL + 1))
fi

echo ""
echo "[smoke-ro] trace sample (last 20 events):"
tail -20 "$TRACE_PATH" | while IFS= read -r line; do
    echo "  $line"
done

echo ""
echo "[smoke-ro] summary: $PASS passed, $FAIL failed. task=$TASK_ID status=$STATUS"
if [ "$FAIL" -ne 0 ] || [ "$STATUS" != "succeeded" ]; then
    echo "[smoke-ro] daemon log tail:"
    tail -40 "$DAEMON_LOG"
    exit 1
fi
echo "[smoke-ro] OK"
