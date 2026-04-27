#!/usr/bin/env bash
# End-to-end smoke test for moirai (formerly agent-router).
#
# Starts the daemon with three small placeholder GGUFs, submits a trivial
# task, waits for it to finish, then prints the trace.
#
# Run from repo root:
#   ./smoke-test.sh
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="${REPO_DIR}/bin/moirai"
PORT="${AGENT_ROUTER_PORT:-5984}"
TEST_REPO="$(mktemp -d /tmp/agent-router-smoke.XXXXXX)"
CONFIG="$(mktemp /tmp/agent-router-smoke-config.XXXXXX.json)"
LLAMA="/home/aegis/Projects/llama-cpp-turboquant/build/bin/llama-server"

cleanup() {
    if [ -n "${DAEMON_PID:-}" ]; then
        kill -INT "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
    fi
    rm -rf "$TEST_REPO" "$CONFIG"
}
trap cleanup EXIT

echo "[smoke] building moirai..."
cd "$REPO_DIR"
mkdir -p bin
go build -o "$BIN" ./cmd/moirai

echo "[smoke] preparing test repo at $TEST_REPO"
cd "$TEST_REPO"
git init -q
git config user.email "smoke@agent-router.local"
git config user.name "agent-router smoke"
cat > .agent-router.toml <<'TOML'
[commands]
test = "echo noop-test-ok"
compile = "echo noop-compile-ok"
lint = "true"

[style]
language = "text"
line_length = 100

[budget]
max_runtime = "5m"
max_iterations = 3
TOML
git add -A
git commit -q -m "smoke test seed"

echo "[smoke] writing throwaway config to $CONFIG"
cat > "$CONFIG" <<JSON
{
  "port": $PORT,
  "llama_server_bin": "$LLAMA",
  "default_repo": "$TEST_REPO",
  "models": {
    "planner": {
      "slot": "planner",
      "model_path": "/home/aegis/Models/tinyllama-1.1b-chat-v1.0.Q8_0.gguf",
      "ctx_size": 2048,
      "n_gpu_layers": 99,
      "port": 8901
    },
    "coder": {
      "slot": "coder",
      "model_path": "/home/aegis/Models/Qwen3-8B-Q4_K_M.gguf",
      "ctx_size": 4096,
      "n_gpu_layers": 99,
      "port": 8902
    },
    "reviewer": {
      "slot": "reviewer",
      "model_path": "/home/aegis/Models/Phi-3-mini-4k-instruct-q4.gguf",
      "ctx_size": 2048,
      "n_gpu_layers": 99,
      "port": 8903
    }
  }
}
JSON

echo "[smoke] starting daemon..."
"$BIN" daemon --config "$CONFIG" > /tmp/agent-router-smoke.log 2>&1 &
DAEMON_PID=$!

echo "[smoke] waiting for :$PORT to come up..."
for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
        echo "[smoke] daemon alive (pid=$DAEMON_PID)"
        break
    fi
    sleep 0.5
done
if ! curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    echo "[smoke] FAIL: daemon did not come up"
    tail -40 /tmp/agent-router-smoke.log
    exit 1
fi

echo "[smoke] submitting task..."
RESP="$("$BIN" task --port "$PORT" --repo "$TEST_REPO" "create a file named hello.txt containing the text hello")"
echo "$RESP"
TASK_ID="$(echo "$RESP" | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p')"
if [ -z "$TASK_ID" ]; then
    echo "[smoke] FAIL: could not extract task id"
    exit 1
fi
echo "[smoke] task id: $TASK_ID"

echo "[smoke] polling for completion (up to 10 min)..."
for i in $(seq 1 120); do
    STATUS="$(curl -sf "http://127.0.0.1:$PORT/tasks/$TASK_ID" | sed -n 's/.*"status": *"\([^"]*\)".*/\1/p' | head -1)"
    echo "[smoke] t=${i}0s status=${STATUS}"
    case "$STATUS" in
        succeeded|failed|aborted)
            break
            ;;
    esac
    sleep 10
done

echo "[smoke] trace path:"
ls -la "$HOME/.local/share/agent-router/traces/$TASK_ID.jsonl"
echo "[smoke] first 40 trace events:"
head -40 "$HOME/.local/share/agent-router/traces/$TASK_ID.jsonl" | while IFS= read -r line; do
    echo "  $line"
done

echo "[smoke] cli status:"
"$BIN" status --port "$PORT" | head -30

echo "[smoke] git log on test repo:"
git -C "$TEST_REPO" --no-pager log --all --oneline

if [ -f "$TEST_REPO/hello.txt" ]; then
    echo "[smoke] hello.txt contents:"
    cat "$TEST_REPO/hello.txt"
    echo
fi

echo "[smoke] done. task=$TASK_ID status=$STATUS"
