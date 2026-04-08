#!/bin/bash
# Reload script for pikvm-key-cli server (deck-hub compatible).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# ── vault: pull fresh secrets from Infisical before sourcing .env ─────────────
_VAULT_SYNC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && cd ../../infrastructure && pwd)/sync-secrets.sh"
if [[ -f "$_VAULT_SYNC" ]] && [[ -n "${INFISICAL_CLIENT_ID:-}" ]]; then
  echo "→ Pulling secrets from vault (pikvm-key-cli)..."
  "$_VAULT_SYNC" --pull pikvm-key-cli 2>/dev/null || echo "  ⚠  Vault pull skipped (using cached .env)"
fi
# ─────────────────────────────────────────────────────────────────────────────
BINARY="$PROJECT_DIR/pikvm"
LOG_FILE="$PROJECT_DIR/pikvm-server.log"
PORT="${PORT:-8095}"

export PATH="$HOME/go/bin:$PATH"

echo "=== PiKVM server reload ==="

echo "→ Stopping existing process..."
pkill -f "$BINARY server" 2>/dev/null && sleep 1 || echo "  (none running)"

echo "→ Building..."
cd "$PROJECT_DIR"

# Templ generate
go run github.com/a-h/templ/cmd/templ@latest generate -path ./internal/server/views 2>&1 || true

# Tailwind CSS
if [ -f package.json ] && [ -d node_modules ]; then
  npm run build:css 2>&1 || true
fi

go build -o "$BINARY" ./cmd/pikvm/
echo "  Build OK: $BINARY"

if [[ -f "$PROJECT_DIR/.env" ]]; then
  set -a; source "$PROJECT_DIR/.env"; set +a
fi

echo "→ Starting on :${PORT}..."
PORT="$PORT" PIKVM_HOST="${PIKVM_HOST:-}" PIKVM_USER="${PIKVM_USER:-admin}" PIKVM_PASS="${PIKVM_PASS:-admin}" \
  PIKVM_SSH_USER="${PIKVM_SSH_USER:-root}" PIKVM_SSH_PASS="${PIKVM_SSH_PASS:-}" \
  PIKVM_SSH_KEY="${PIKVM_SSH_KEY:-}" \
  nohup "$BINARY" server --port "$PORT" >"$LOG_FILE" 2>&1 &
echo $! >"$PROJECT_DIR/pikvm-server.pid"

for i in $(seq 1 25); do
  sleep 0.2
  if curl -fsS "http://127.0.0.1:${PORT}/api/health" >/dev/null 2>&1; then
    echo "✓ PiKVM server at http://127.0.0.1:${PORT}/"
    exit 0
  fi
done

echo "✗ Server did not respond — check $LOG_FILE"
exit 1
