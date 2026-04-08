#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

echo "→ templ generate…"
go run github.com/a-h/templ/cmd/templ@latest generate -path ./internal/server/views

if [ -f package.json ]; then
  echo "→ Tailwind (static CSS)…"
  npm run build:css
fi

echo "→ go build…"
go build -o pikvm ./cmd/pikvm
echo "Build OK: $PROJECT_DIR/pikvm"
