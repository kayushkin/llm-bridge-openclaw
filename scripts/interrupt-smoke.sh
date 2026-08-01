#!/usr/bin/env bash
#
# interrupt-smoke.sh — drive the built binary through its interrupt contract.
#
# The unit tests call watchSignals directly with a channel. This drives the real
# process with real signals from the OS, against an HTTP server that streams SSE
# the way a long OpenClaw turn does, and asserts what a user pressing Stop
# actually gets:
#
#   1. SIGINT cancels the in-flight turn and the process stays alive
#   2. the turn's POST is really aborted (the server sees the client go away)
#   3. every later SIGINT is honoured too, not just the first of the session
#   4. SIGTERM still ends the process
#
# Nothing here needs a real OpenClaw: the bridge's only outbound channel is
# POST /v1/chat/completions, so a stub of that endpoint is the whole surface.
#
# Usage: scripts/interrupt-smoke.sh
set -uo pipefail

cd "$(dirname "$0")/.."

work="$(mktemp -d)"
trap 'rm -rf "$work"; [ -n "${harness_pid:-}" ] && kill -9 "$harness_pid" 2>/dev/null; [ -n "${stub_pid:-}" ] && kill -9 "$stub_pid" 2>/dev/null' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "  ok: $*"; }

echo "building..."
go build -o "$work/llm-bridge-openclaw" . || fail "build"

# ---------------------------------------------------------------------------
# A stub OpenClaw: streams SSE forever and never sends [DONE], and records each
# time a client disconnects mid-stream. That count is how we know an interrupt
# reached the socket rather than merely being logged.
# ---------------------------------------------------------------------------
cat > "$work/stub.py" <<'PY'
import http.server, socketserver, threading, time, sys, os

disconnects = 0
lock = threading.Lock()
state = os.environ["STUB_STATE"]

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        global disconnects
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        with open(state + ".streaming", "a") as f:
            f.write("1\n")
        try:
            while True:
                chunk = b'data: {"choices":[]}\n\n'
                self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
                self.wfile.flush()
                time.sleep(0.05)
        except (BrokenPipeError, ConnectionResetError):
            with lock:
                disconnects += 1
            with open(state + ".disconnects", "a") as f:
                f.write("1\n")

    def log_message(self, *args):
        pass

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

with Server(("127.0.0.1", int(sys.argv[1])), Handler) as httpd:
    httpd.serve_forever()
PY

port=18799
export STUB_STATE="$work/stub"
python3 "$work/stub.py" "$port" &
stub_pid=$!

for _ in $(seq 1 50); do
    if bash -c "exec 3<>/dev/tcp/127.0.0.1/$port" 2>/dev/null; then break; fi
    sleep 0.1
done

count() { [ -f "$1" ] && wc -l < "$1" | tr -d ' ' || echo 0; }

wait_for() { # wait_for <file> <n> <what>
    for _ in $(seq 1 100); do
        [ "$(count "$1")" -ge "$2" ] && return 0
        sleep 0.1
    done
    fail "timed out waiting for $3 (have $(count "$1"), want $2)"
}

# ---------------------------------------------------------------------------
# Run the harness with a stdin pipe so we can feed it JSON-RPC.
# OPENCLAW_DIR is pointed at an empty dir: the tailer finds no session file and
# logs it, which keeps this test on the interrupt path only.
# ---------------------------------------------------------------------------
mkfifo "$work/stdin"
mkdir -p "$work/openclaw"
OPENCLAW_URL="http://127.0.0.1:$port" OPENCLAW_DIR="$work/openclaw" \
    "$work/llm-bridge-openclaw" < "$work/stdin" > "$work/events.ndjson" 2> "$work/harness.log" &
harness_pid=$!
exec 9>"$work/stdin"

alive() { kill -0 "$harness_pid" 2>/dev/null; }

turn() { # turn <n>: start or continue a turn, then wait for the stub to stream
    local want=$1
    wait_for "$work/stub.streaming" "$want" "the stub to start streaming turn $want"
}

echo "turn 1: cold start with a prompt"
echo '{"method":"start","params":{"bridge_session_id":"bs_smoke","prompt":"a long turn"}}' >&9
turn 1
alive || fail "harness died before the first interrupt"

for n in 1 2 3; do
    echo "SIGINT #$n"
    kill -INT "$harness_pid" || fail "could not signal the harness"
    wait_for "$work/stub.disconnects" "$n" "the stub to see client disconnect $n"
    pass "interrupt $n aborted the in-flight POST"

    sleep 0.3
    alive || fail "SIGINT #$n killed the harness — Stop still kills the session, not the turn"
    pass "harness survived SIGINT #$n"

    if [ "$n" -lt 3 ]; then
        echo '{"method":"message","params":{"content":"another long turn"}}' >&9
        turn $((n + 1))
        pass "session accepted a follow-up message after the interrupt"
    fi
done

# ---------------------------------------------------------------------------
# The events the chat UI would see.
# ---------------------------------------------------------------------------
grep -q '"subtype":"interrupt"' "$work/events.ndjson" \
    || fail "no interrupt system event emitted; the transcript shows a turn stopping for no reason"
pass "emitted an interrupt system event"

grep -q '"code":"INTERRUPTED"' "$work/events.ndjson" \
    || fail "no INTERRUPTED error event emitted"
pass "reported the cancelled turn as INTERRUPTED"

grep -q '"code":"SEND_ERROR"' "$work/events.ndjson" \
    && fail "reported an interrupt as SEND_ERROR — a UI cannot tell 'you stopped this' from 'this broke'"
pass "did not misreport the interrupt as a send failure"

echo "SIGTERM"
kill -TERM "$harness_pid"
for _ in $(seq 1 50); do alive || break; sleep 0.1; done
alive && fail "SIGTERM did not stop the harness — it is now unkillable by its own gateway"
pass "SIGTERM shut the harness down"

echo
echo "interrupt smoke PASSED (3 interrupts honoured, session survived all of them)"
