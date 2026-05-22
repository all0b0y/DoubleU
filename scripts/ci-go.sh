#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ ! -f backend-go/go.mod ]]; then
  echo "backend-go/go.mod not found; skipping Go tests until backend module is present."
  exit 0
fi

mkdir -p "$ROOT_DIR/.qa_coverage/go" "$ROOT_DIR/.qa_coverage/go-build"

cd backend-go
GOCACHE="$ROOT_DIR/.qa_coverage/go-build" go test ./... -coverprofile="$ROOT_DIR/coverage-go.out" -covermode=atomic
