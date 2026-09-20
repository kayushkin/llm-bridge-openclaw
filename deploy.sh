#!/usr/bin/env bash
# Build llm-bridge-openclaw and install it as the binary bridge-server spawns.
#
# bridge-server resolves a harness by name through exec.LookPath
# (llm-bridge-server internal/harness/manager.go), so $HOME/bin/llm-bridge-openclaw
# IS the deployment. Every session spawns a fresh process, which is why there is
# no service to restart here: the next session started after this script runs
# gets the new binary, and sessions already in flight keep the one they started
# with.
#
# The smokes run BEFORE the install, not after. A harness that panics on startup
# compiles perfectly and fails at session-spawn time, where nobody is watching;
# installing first would put that binary in front of live sessions and then tell
# us about it.
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_NAME="llm-bridge-openclaw"
USER_BIN="$HOME/bin/$BIN_NAME"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"

echo "==> Testing $BIN_NAME..."
go vet ./...
go test ./...

echo "==> Building $BIN_NAME..."
go build -o "$BIN_NAME" .
echo "    built: $(ls -lh "$BIN_NAME" | awk '{print $5}')"

echo "==> Boot smoke (scripts/e2e-smoke.sh)..."
bash scripts/e2e-smoke.sh >/dev/null
echo "    boots, discovers and answers"

echo "==> Interrupt smoke (scripts/interrupt-smoke.sh)..."
bash scripts/interrupt-smoke.sh >/dev/null
echo "    Stop cancels the turn and leaves the session alive"

echo "==> Installing to $USER_BIN..."
mkdir -p "$HOME/bin"
cp "$BIN_NAME" "$USER_BIN"

echo "==> Verifying..."
"$USER_BIN" -version
echo "    openclaw gateway: ${OPENCLAW_URL:-http://127.0.0.1:18789} $(curl -sfS -o /dev/null -m 2 "${OPENCLAW_URL:-http://127.0.0.1:18789}/v1/models" 2>/dev/null && echo '(reachable)' || echo '(not reachable from here — sessions resolve it at start)')"

echo "==> Done. bridge-server picks up the new binary on the next session spawn (no restart needed)."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
